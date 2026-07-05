// mem-bench drives the memba /v1 API with benchmark cases (spec §17).
// It is the Phase-0 harness skeleton: JSONL case files of inserts and
// queries, scored on retrieval hit rate, abstention, tokens, and latency —
// reported separately, never one collapsed number (§17.4).
//
//	mem-bench run --base http://localhost:8080 --token $TOK --cases testdata/bench/smoke.jsonl
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lazorfuzz/memba/pkg/client"
	"github.com/lazorfuzz/memba/pkg/evalapi"
	"github.com/lazorfuzz/memba/pkg/memory"
)

type caseLine struct {
	Type  string                  `json:"type"` // insert | query | reset
	Item  *evalapi.BenchmarkItem  `json:"item,omitempty"`
	Query *evalapi.BenchmarkQuery `json:"query,omitempty"`
	NS    string                  `json:"namespace,omitempty"`
}

type scores struct {
	queries        int
	retrievalHits  int
	abstainCases   int
	abstainCorrect int
	tokenTotal     int
	latencyTotal   time.Duration
	failures       []string
}

func main() {
	runCmd := flag.NewFlagSet("run", flag.ExitOnError)
	base := runCmd.String("base", "http://localhost:8080", "memd base URL")
	token := runCmd.String("token", os.Getenv("MEMBA_TOKEN"), "bearer token")
	cases := runCmd.String("cases", "", "JSONL case file")
	mode := runCmd.String("mode", "", "override query mode (scoped|deep|workspace)")
	verbose := runCmd.Bool("v", false, "log per-case results")

	if len(os.Args) < 2 || os.Args[1] != "run" {
		fmt.Fprintln(os.Stderr, "usage: mem-bench run --base URL --token TOK --cases FILE [--mode deep] [-v]")
		os.Exit(2)
	}
	runCmd.Parse(os.Args[2:])
	if *cases == "" || *token == "" {
		fmt.Fprintln(os.Stderr, "--cases and --token are required")
		os.Exit(2)
	}

	ctx := context.Background()
	mem := evalapi.NewV1(client.New(*base, *token))
	if err := mem.Client.Healthz(ctx); err != nil {
		fatal("memd not reachable: %v", err)
	}

	f, err := os.Open(*cases)
	if err != nil {
		fatal("open cases: %v", err)
	}
	defer f.Close()

	var s scores
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var cl caseLine
		if err := json.Unmarshal([]byte(line), &cl); err != nil {
			fatal("line %d: %v", lineNo, err)
		}
		switch cl.Type {
		case "reset":
			if err := mem.Reset(ctx, cl.NS); err != nil {
				fatal("line %d reset: %v", lineNo, err)
			}
		case "insert":
			if cl.Item == nil {
				fatal("line %d: insert without item", lineNo)
			}
			if err := mem.Insert(ctx, *cl.Item); err != nil {
				fatal("line %d insert: %v", lineNo, err)
			}
		case "query":
			if cl.Query == nil {
				fatal("line %d: query without query", lineNo)
			}
			q := *cl.Query
			if *mode != "" {
				q.Mode = *mode
			}
			started := time.Now()
			pack, err := mem.Query(ctx, q)
			lat := time.Since(started)
			if err != nil {
				s.failures = append(s.failures, fmt.Sprintf("%s: %v", q.CaseID, err))
				s.queries++
				continue
			}
			s.queries++
			s.tokenTotal += pack.TokenTotal
			s.latencyTotal += lat
			hit := scoreCase(q, pack)
			if q.Abstain {
				s.abstainCases++
				if hit {
					s.abstainCorrect++
				}
			} else if hit {
				s.retrievalHits++
			}
			if *verbose {
				status := "MISS"
				if hit {
					status = "HIT "
				}
				fmt.Printf("%s %-28s answerability=%-7s tokens=%-6d lat=%s\n",
					status, q.CaseID, pack.Answerability, pack.TokenTotal, lat.Round(time.Millisecond))
			}
		default:
			fatal("line %d: unknown type %q", lineNo, cl.Type)
		}
	}
	if err := sc.Err(); err != nil {
		fatal("scan: %v", err)
	}
	report(s)
	if len(s.failures) > 0 {
		os.Exit(1)
	}
}

// scoreCase: for answerable cases, every expected string must appear
// somewhere in the pack; for abstention cases, credit requires low
// answerability or an explicit missing_evidence entry (§17.3 abstention set).
func scoreCase(q evalapi.BenchmarkQuery, p memory.EvidencePack) bool {
	if q.Abstain {
		return p.Answerability == "low" || len(p.MissingEvidence) > 0
	}
	hay := packText(p)
	for _, want := range q.Expected {
		if !strings.Contains(strings.ToLower(hay), strings.ToLower(want)) {
			return false
		}
	}
	return len(q.Expected) > 0
}

func packText(p memory.EvidencePack) string {
	b := &strings.Builder{}
	b.WriteString(p.ProfileExcerpt)
	for _, c := range p.Cards {
		b.WriteString(c.Title + "\n" + c.Body + "\n")
	}
	for _, f := range p.Facts {
		b.WriteString(f.Subject + " " + f.Predicate + " " + f.Object + "\n")
	}
	for _, f := range p.Superseded {
		b.WriteString(f.Subject + " " + f.Predicate + " " + f.Object + "\n")
	}
	for _, s := range p.RawSpans {
		b.WriteString(s.Excerpt + "\n")
	}
	for _, s := range p.StaleFlagged {
		b.WriteString(s.Detail + "\n")
	}
	return b.String()
}

func report(s scores) {
	answerable := s.queries - s.abstainCases
	fmt.Println("\n=== mem-bench report (metrics reported separately, §17.4) ===")
	if answerable > 0 {
		fmt.Printf("retrieval   : %d/%d cases contained all expected evidence (%.1f%%)\n",
			s.retrievalHits, answerable, 100*float64(s.retrievalHits)/float64(answerable))
	}
	if s.abstainCases > 0 {
		fmt.Printf("abstention  : %d/%d correctly flagged as unanswerable (%.1f%%)\n",
			s.abstainCorrect, s.abstainCases, 100*float64(s.abstainCorrect)/float64(s.abstainCases))
	}
	if s.queries > 0 {
		fmt.Printf("efficiency  : avg %d evidence tokens/pack · avg latency %s\n",
			s.tokenTotal/s.queries, (s.latencyTotal / time.Duration(s.queries)).Round(time.Millisecond))
	}
	for _, f := range s.failures {
		fmt.Printf("FAILED      : %s\n", f)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
