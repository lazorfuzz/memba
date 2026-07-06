package consolidate

import (
	"strings"
	"testing"

	"github.com/lazorfuzz/memba/pkg/memory"
)

func TestFailureSignatureClustersAcrossRuns(t *testing.T) {
	a := "step 3 ok\nERROR: connection to db-7f3a2b1c timed out after 30s (attempt 4)\nretrying"
	b := "ERROR: connection to db-99e10c44 timed out after 31s (attempt 1)"
	sigA, quoteA := FailureSignature(a)
	sigB, _ := FailureSignature(b)
	if sigA == "" || sigB == "" {
		t.Fatal("failure lines not detected")
	}
	if sigA != sigB {
		t.Fatalf("same failure with different ids/numbers must cluster:\n%q\n%q", sigA, sigB)
	}
	if !strings.Contains(quoteA, "db-7f3a2b1c") {
		t.Fatalf("quote must stay verbatim for citation: %q", quoteA)
	}
	// Different failures don't cluster.
	sigC, _ := FailureSignature("panic: nil pointer dereference in invoice.Apply")
	if sigC == sigA {
		t.Fatal("distinct failures must not share a signature")
	}
	// Clean logs produce no signature.
	if sig, _ := FailureSignature("all 132 tests passed\ndone"); sig != "" {
		t.Fatalf("clean log produced signature %q", sig)
	}
}

func TestRenderProfileByteStable(t *testing.T) {
	cards := []memory.Card{
		{ID: "c2", CardType: memory.CardConvention, Title: "B convention"},
		{ID: "c1", CardType: memory.CardProcedure, Title: "run make test"},
		{ID: "c3", CardType: memory.CardGotcha, Title: "never edit pb.go", Importance: 0.9,
			Structured: map[string]any{"trigger": "*_pb.go"}},
		{ID: "c4", CardType: memory.CardOwnership, Title: "owned by team:payments"},
	}
	p1 := RenderProfile("/acme/repos/x", cards)
	// Input order must not matter (byte-stable serialization, §11 C2).
	reversed := []memory.Card{cards[3], cards[2], cards[1], cards[0]}
	p2 := RenderProfile("/acme/repos/x", reversed)
	if p1 != p2 {
		t.Fatalf("profile not byte-stable across input orderings:\n%s\n---\n%s", p1, p2)
	}
	for _, want := range []string{"[card:c1]", "[card:c2]", "[card:c3]", "[card:c4]", "Top gotchas", "(trigger: *_pb.go)"} {
		if !strings.Contains(p1, want) {
			t.Fatalf("profile missing %q:\n%s", want, p1)
		}
	}
	// Empty namespace renders the bootstrap hint, still deterministic.
	if p := RenderProfile("/acme/empty", nil); !strings.Contains(p, "No active profile cards yet") {
		t.Fatalf("empty profile wrong:\n%s", p)
	}
}

func TestTopGotchasCappedAtFive(t *testing.T) {
	var cards []memory.Card
	for i := 0; i < 8; i++ {
		cards = append(cards, memory.Card{
			ID: string(rune('a' + i)), CardType: memory.CardGotcha,
			Title: "gotcha " + string(rune('a'+i)), Importance: float64(i),
		})
	}
	p := RenderProfile("/ns", cards)
	if strings.Count(p, "[card:") != 5 {
		t.Fatalf("expected top-5 gotchas, got:\n%s", p)
	}
	// Highest importance first.
	if !strings.Contains(p, "1. gotcha h") {
		t.Fatalf("importance ordering wrong:\n%s", p)
	}
}
