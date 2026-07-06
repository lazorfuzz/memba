package main

// LongMemEval (v1) adapter — spec §17.2 primary benchmark, P1 exit criterion.
//
// Consumes the official dataset format (longmemeval_s.json / _m / oracle):
// each instance carries haystack chat sessions with dates plus one question.
// The adapter drives the public /v1 API only (I5): sessions are inserted as
// chat evidence, the question runs through POST /v1/query, and scoring
// measures EVIDENCE RECALL — did the pack surface the gold answer — plus the
// abstention axis (question_id suffix "_abs"). A pluggable LLM reader for
// answer-level EM/F1 sits on top of these packs (§17.1); evidence recall is
// the memory-side metric this harness owns.
//
//	mem-bench longmemeval --file longmemeval_s.json --token $TOK [--limit 50] [--mode deep]

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/lazorfuzz/memba/pkg/client"
	"github.com/lazorfuzz/memba/pkg/evalapi"
	"github.com/lazorfuzz/memba/pkg/memory"
)

// lmeInstance mirrors the LongMemEval v1 JSON schema.
type lmeInstance struct {
	QuestionID       string   `json:"question_id"`
	QuestionType     string   `json:"question_type"`
	Question         string   `json:"question"`
	Answer           any      `json:"answer"` // string or number
	QuestionDate     string   `json:"question_date"`
	HaystackDates    []string `json:"haystack_dates"`
	HaystackSessionIDs []string `json:"haystack_session_ids"`
	HaystackSessions [][]lmeTurn `json:"haystack_sessions"`
	AnswerSessionIDs []string `json:"answer_session_ids"`
}

type lmeTurn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (i lmeInstance) abstention() bool { return strings.HasSuffix(i.QuestionID, "_abs") }

func (i lmeInstance) answerString() string {
	switch v := i.Answer.(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return strings.TrimSuffix(strings.TrimSuffix(fmt.Sprint(v), ".000000"), ".0")
	}
}

// parseLMEDate handles "2023/05/20 (Sat) 02:21".
func parseLMEDate(s string) *time.Time {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return nil
	}
	layout := "2006/01/02"
	value := fields[0]
	if len(fields) >= 3 {
		layout, value = "2006/01/02 15:04", fields[0]+" "+fields[2]
	}
	t, err := time.Parse(layout, value)
	if err != nil {
		if t2, err2 := time.Parse("2006/01/02", fields[0]); err2 == nil {
			t = t2
		} else {
			return nil
		}
	}
	t = t.UTC()
	return &t
}

type lmeTypeScore struct {
	total, hits int
	tokens      int
	latency     time.Duration
}

func cmdLongMemEval(args []string) {
	fs := flag.NewFlagSet("longmemeval", flag.ExitOnError)
	base := fs.String("base", "http://localhost:8080", "memd base URL")
	token := fs.String("token", os.Getenv("MEMBA_TOKEN"), "bearer token")
	file := fs.String("file", "", "LongMemEval JSON file (longmemeval_s.json / _m / oracle)")
	limit := fs.Int("limit", 0, "max instances (0 = all)")
	mode := fs.String("mode", memory.ModeDeep, "query mode (scoped|deep|workspace)")
	verbose := fs.Bool("v", false, "log per-instance results")
	fs.Parse(args)
	if *file == "" || *token == "" {
		fatal("--file and --token are required")
	}

	data, err := os.ReadFile(*file)
	if err != nil {
		fatal("read dataset: %v", err)
	}
	var instances []lmeInstance
	if err := json.Unmarshal(data, &instances); err != nil {
		fatal("parse dataset (expected the LongMemEval v1 JSON array): %v", err)
	}
	if *limit > 0 && len(instances) > *limit {
		instances = instances[:*limit]
	}

	ctx := context.Background()
	mem := evalapi.NewV1(client.New(*base, *token))
	if err := mem.Client.Healthz(ctx); err != nil {
		fatal("memd not reachable: %v", err)
	}

	perType := map[string]*lmeTypeScore{}
	abstain := &lmeTypeScore{}
	insertedSessions, failures := 0, 0

	for n, inst := range instances {
		nsBase := "/lme/" + inst.QuestionID
		if err := mem.Reset(ctx, nsBase); err != nil {
			fatal("reset %s: %v", inst.QuestionID, err)
		}
		// Insert every haystack session as one chat evidence item (I5:
		// through POST /v1/evidence like any connector).
		for si, session := range inst.HaystackSessions {
			var b strings.Builder
			for _, turn := range session {
				fmt.Fprintf(&b, "%s: %s\n\n", turn.Role, turn.Content)
			}
			if strings.TrimSpace(b.String()) == "" {
				continue
			}
			sid := fmt.Sprintf("s%03d", si)
			if si < len(inst.HaystackSessionIDs) {
				sid = inst.HaystackSessionIDs[si]
			}
			var eventTime *time.Time
			if si < len(inst.HaystackDates) {
				eventTime = parseLMEDate(inst.HaystackDates[si])
			}
			err := mem.Insert(ctx, evalapi.BenchmarkItem{
				NamespaceID: nsBase,
				SourceType:  memory.SourceSlack, // chat-shaped history → chat chunking
				SourceURI:   "lme://" + inst.QuestionID + "/session/" + sid,
				Body:        b.String(),
				EventTime:   eventTime,
			})
			if err != nil {
				failures++
				continue
			}
			insertedSessions++
		}

		started := time.Now()
		pack, err := mem.Query(ctx, evalapi.BenchmarkQuery{
			NamespaceID: nsBase,
			Query:       inst.Question,
			Mode:        *mode,
		})
		lat := time.Since(started)
		if err != nil {
			failures++
			fmt.Fprintf(os.Stderr, "query %s: %v\n", inst.QuestionID, err)
			continue
		}

		var hit bool
		if inst.abstention() {
			// Abstention credit: the system flags the gap instead of serving
			// confident-looking evidence (§17.3 abstention set, T2).
			hit = pack.Answerability == "low" || len(pack.MissingEvidence) > 0
			abstain.total++
			abstain.tokens += pack.TokenTotal
			abstain.latency += lat
			if hit {
				abstain.hits++
			}
		} else {
			answer := inst.answerString()
			hit = answer != "" && strings.Contains(strings.ToLower(packText(pack)), strings.ToLower(answer))
			ts := perType[inst.QuestionType]
			if ts == nil {
				ts = &lmeTypeScore{}
				perType[inst.QuestionType] = ts
			}
			ts.total++
			ts.tokens += pack.TokenTotal
			ts.latency += lat
			if hit {
				ts.hits++
			}
		}
		if *verbose {
			status := "MISS"
			if hit {
				status = "HIT "
			}
			fmt.Printf("[%3d/%3d] %s %-24s %-28s tokens=%-6d lat=%s\n",
				n+1, len(instances), status, inst.QuestionType, inst.QuestionID,
				pack.TokenTotal, lat.Round(time.Millisecond))
		}
	}

	// Report per axis, never one collapsed number (§17.4).
	fmt.Println("\n=== LongMemEval (v1) — evidence-recall report ===")
	fmt.Printf("dataset: %s · %d instances · %d sessions inserted · mode=%s\n",
		*file, len(instances), insertedSessions, *mode)
	types := make([]string, 0, len(perType))
	for t := range perType {
		types = append(types, t)
	}
	sort.Strings(types)
	var total, hits int
	for _, t := range types {
		ts := perType[t]
		total += ts.total
		hits += ts.hits
		fmt.Printf("%-28s %3d/%3d recalled (%.1f%%) · avg %d tok · avg %s\n",
			t, ts.hits, ts.total, pct(ts.hits, ts.total),
			ts.tokens/max(ts.total, 1), (ts.latency / time.Duration(max(ts.total, 1))).Round(time.Millisecond))
	}
	if total > 0 {
		fmt.Printf("%-28s %3d/%3d recalled (%.1f%%)\n", "OVERALL (non-abstention)", hits, total, pct(hits, total))
	}
	if abstain.total > 0 {
		fmt.Printf("%-28s %3d/%3d correct (%.1f%%)   [T2 target ≥ 90%%]\n",
			"abstention", abstain.hits, abstain.total, pct(abstain.hits, abstain.total))
	}
	if failures > 0 {
		fmt.Printf("failures: %d\n", failures)
		os.Exit(1)
	}
}

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
