// slack is the chat connector (spec §14.4): it polls conversations.history
// for the given channels and ingests message batches as slack evidence
// (authority 7 — chat can never self-promote past B2). Stateless beyond a
// per-channel timestamp cursor. Injection/secret scanning happens
// server-side at ingest (§15.3/§15.4 D2) — the connector ships verbatim.
//
//	slack-connector --channels C0PAY,C0PLAT --namespace /acme/team/payments \
//	  --token $TOK [--interval 5m]
//
// Requires SLACK_TOKEN (bot token with channels:history).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lazorfuzz/memba/pkg/client"
	"github.com/lazorfuzz/memba/pkg/memory"
)

const batchSize = 25 // messages per evidence item, matching chat chunking (§10.2)

func main() {
	base := flag.String("base", "http://localhost:8080", "memd base URL")
	token := flag.String("token", os.Getenv("MEMBA_TOKEN"), "memba bearer token")
	channels := flag.String("channels", "", "comma-separated Slack channel IDs")
	namespace := flag.String("namespace", "", "target namespace")
	slackAPI := flag.String("slack-api", "https://slack.com/api", "Slack API base")
	cursorFile := flag.String("cursor-file", "", "cursor path (default ./data/connectors/slack.json)")
	interval := flag.Duration("interval", 0, "poll interval; 0 = run once")
	flag.Parse()
	slackToken := os.Getenv("SLACK_TOKEN")
	if *channels == "" || *namespace == "" || *token == "" || slackToken == "" {
		fatal("--channels, --namespace, --token and SLACK_TOKEN required")
	}
	if *cursorFile == "" {
		*cursorFile = filepath.Join("data", "connectors", "slack.json")
	}

	mem := client.New(*base, *token)
	sc := &slackClient{base: *slackAPI, token: slackToken, hc: &http.Client{Timeout: 30 * time.Second}}
	ctx := context.Background()
	if err := mem.UpsertNamespace(ctx, memory.Namespace{ID: *namespace, Kind: "team"}); err != nil {
		fatal("namespace: %v", err)
	}

	for {
		cursors := loadCursors(*cursorFile)
		total := 0
		for _, ch := range strings.Split(*channels, ",") {
			ch = strings.TrimSpace(ch)
			if ch == "" {
				continue
			}
			n, err := syncChannel(ctx, sc, mem, ch, *namespace, cursors)
			if err != nil {
				fmt.Fprintf(os.Stderr, "channel %s: %v\n", ch, err)
				continue
			}
			total += n
		}
		saveCursors(*cursorFile, cursors)
		fmt.Printf("ingested %d message batches\n", total)
		if *interval == 0 {
			return
		}
		time.Sleep(*interval)
	}
}

type slackMessage struct {
	TS   string `json:"ts"`
	User string `json:"user"`
	Text string `json:"text"`
	Type string `json:"type"`
	Sub  string `json:"subtype"`
}

func syncChannel(ctx context.Context, sc *slackClient, mem *client.Client, channel, namespace string, cursors map[string]string) (int, error) {
	oldest := cursors[channel]
	var resp struct {
		OK       bool           `json:"ok"`
		Error    string         `json:"error"`
		Messages []slackMessage `json:"messages"`
	}
	params := url.Values{"channel": {channel}, "limit": {"200"}, "inclusive": {"false"}}
	if oldest != "" {
		params.Set("oldest", oldest)
	}
	if err := sc.call(ctx, "conversations.history", params, &resp); err != nil {
		return 0, err
	}
	if !resp.OK {
		return 0, fmt.Errorf("slack: %s", resp.Error)
	}
	// Slack returns newest-first; process oldest-first for stable batching.
	msgs := resp.Messages
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	var keep []slackMessage
	for _, m := range msgs {
		if m.Type == "message" && m.Sub == "" && strings.TrimSpace(m.Text) != "" {
			keep = append(keep, m)
		}
	}
	if len(keep) == 0 {
		return 0, nil
	}

	batches := 0
	for start := 0; start < len(keep); start += batchSize {
		end := start + batchSize
		if end > len(keep) {
			end = len(keep)
		}
		batch := keep[start:end]
		b := &strings.Builder{}
		for _, m := range batch {
			fmt.Fprintf(b, "%s: %s\n\n", m.User, m.Text)
		}
		eventTime := tsToTime(batch[0].TS)
		_, err := mem.InsertEvidence(ctx, memory.InsertRequest{
			NamespaceID: namespace,
			SourceType:  memory.SourceSlack,
			SourceURI:   fmt.Sprintf("slack://%s/p%s", channel, strings.ReplaceAll(batch[0].TS, ".", "")),
			Body:        b.String(),
			EventTime:   eventTime,
			Metadata:    map[string]any{"channel": channel, "messages": len(batch)},
		})
		if err != nil {
			return batches, err
		}
		batches++
	}
	cursors[channel] = keep[len(keep)-1].TS
	return batches, nil
}

func tsToTime(ts string) *time.Time {
	parts := strings.SplitN(ts, ".", 2)
	sec, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return nil
	}
	t := time.Unix(sec, 0).UTC()
	return &t
}

type slackClient struct {
	base  string
	token string
	hc    *http.Client
}

func (s *slackClient) call(ctx context.Context, method string, params url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/"+method+"?"+params.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := s.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %d %s", method, resp.StatusCode, string(body))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func loadCursors(path string) map[string]string {
	out := map[string]string{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

func saveCursors(path string, c map[string]string) {
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	b, _ := json.Marshal(c)
	_ = os.WriteFile(path, b, 0o644)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
