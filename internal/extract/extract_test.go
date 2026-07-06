package extract

import (
	"strings"
	"testing"

	"github.com/lazorfuzz/memba/pkg/memory"
)

func TestParseCandidatesToleratesFencesAndProse(t *testing.T) {
	raw := "Here are the candidates:\n```json\n[{\"kind\":\"card\",\"card_type\":\"gotcha\",\"title\":\"t\",\"body\":\"b\",\"citations\":[{\"quote\":\"q\"}]}]\n```\nDone."
	cands, err := ParseCandidates(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].CardType != "gotcha" {
		t.Fatalf("bad parse: %+v", cands)
	}
	// Empty array is valid (evidence taught nothing durable).
	cands, err = ParseCandidates("[]")
	if err != nil || len(cands) != 0 {
		t.Fatalf("empty array should parse: %v %v", cands, err)
	}
	// Garbage errors rather than silently passing.
	if _, err := ParseCandidates("no json here"); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestCitationGateVerbatimOnly(t *testing.T) {
	e := &Extractor{}
	ev := memory.RawEvidence{
		ID:        "raw-1",
		SourceURI: "doc://x/readme.md",
		Body:      "line one\nRun `make test` before pushing.\nline three",
	}

	// Verbatim quote passes and gets line numbers.
	refs, ok := e.validateCitations(candidate{
		Citations: []struct {
			Quote string `json:"quote"`
		}{{Quote: "Run `make test` before pushing."}},
	}, ev)
	if !ok || len(refs) != 1 {
		t.Fatalf("verbatim quote must pass: %v %v", refs, ok)
	}
	if refs[0].LineStart != 2 || refs[0].LineEnd != 2 {
		t.Fatalf("line range wrong: %+v", refs[0])
	}
	if refs[0].RawID != "raw-1" || refs[0].SourceURI != "doc://x/readme.md" {
		t.Fatalf("provenance missing: %+v", refs[0])
	}

	// Paraphrase is NOT a citation (I2) — candidate discarded.
	_, ok = e.validateCitations(candidate{
		Citations: []struct {
			Quote string `json:"quote"`
		}{{Quote: "You should run the test suite before pushing"}},
	}, ev)
	if ok {
		t.Fatal("paraphrased quote must fail the citation gate")
	}

	// Zero citations → discarded.
	if _, ok := e.validateCitations(candidate{}, ev); ok {
		t.Fatal("candidate without citations must be discarded")
	}
}

func TestLoadPromptsFallsBackToEmbedded(t *testing.T) {
	system, user := LoadPrompts("/nonexistent")
	if !strings.Contains(system, "{{max_candidates}}") || !strings.Contains(user, "{{body}}") {
		t.Fatal("embedded prompts must carry template slots")
	}
}
