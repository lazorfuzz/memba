package gates

import (
	"context"
	"testing"
	"time"

	"github.com/lazorfuzz/memba/internal/store"
	"github.com/lazorfuzz/memba/pkg/memory"
)

func TestG5DiversityCap(t *testing.T) {
	var cands []store.Candidate
	for i := 0; i < 10; i++ {
		cands = append(cands, store.Candidate{
			Kind: "chunk", ID: string(rune('a' + i)),
			SourceURI: "doc://poisoned/one-doc", RawID: "raw-1",
		})
	}
	g := Apply(context.Background(), cands, Options{Now: time.Now()})
	if len(g.Kept) != 3 {
		t.Fatalf("G5 must cap at 3 per source_uri, kept %d", len(g.Kept))
	}
	if g.DroppedByG5 != 7 {
		t.Fatalf("expected 7 drops, got %d", g.DroppedByG5)
	}
}

func TestG2SupersededFactsSectioned(t *testing.T) {
	cands := []store.Candidate{
		{Kind: "fact", ID: "f1", Status: memory.FactActive},
		{Kind: "fact", ID: "f2", Status: memory.FactSuperseded},
	}
	g := Apply(context.Background(), cands, Options{Now: time.Now()})
	if len(g.Kept) != 1 || g.Kept[0].ID != "f1" {
		t.Fatalf("active fact should be kept: %+v", g.Kept)
	}
	if len(g.Superseded) != 1 || g.Superseded[0].ID != "f2" {
		t.Fatalf("superseded fact should be sectioned: %+v", g.Superseded)
	}
}

func TestStaleCardsFlaggedNotDropped(t *testing.T) {
	cands := []store.Candidate{{Kind: "card", ID: "c1", Status: memory.StatusStale, Title: "old gotcha"}}
	g := Apply(context.Background(), cands, Options{Now: time.Now()})
	if len(g.Kept) != 0 {
		t.Fatal("stale cards must not enter default sections")
	}
	if len(g.StaleFlagged) != 1 {
		t.Fatal("stale cards must be served flagged, never silently dropped (§9.2)")
	}
}

func TestResolveConflictOrder(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.AddDate(0, 3, 0)

	// Branch-verified beats everything.
	g := ResolveConflict([]ClaimInfo{
		{ID: "a", BranchVerified: true, Authority: 5, ValidFrom: t0, AssertedAt: t0},
		{ID: "b", RuntimeValidTTL: true, Authority: 1, ValidFrom: t1, AssertedAt: t1},
	})
	if g.Winner != "a" || g.Rule != "branch-verified" {
		t.Fatalf("branch-verified must win: %+v", g)
	}

	// Runtime-validated beats authority.
	g = ResolveConflict([]ClaimInfo{
		{ID: "a", RuntimeValidTTL: true, Authority: 6, ValidFrom: t0, AssertedAt: t0},
		{ID: "b", Authority: 1, ValidFrom: t1, AssertedAt: t1},
	})
	if g.Winner != "a" || g.Rule != "runtime-validated" {
		t.Fatalf("runtime-validated must win: %+v", g)
	}

	// Authority beats recency.
	g = ResolveConflict([]ClaimInfo{
		{ID: "a", Authority: 2, ValidFrom: t0, AssertedAt: t0},
		{ID: "b", Authority: 6, ValidFrom: t1, AssertedAt: t1},
	})
	if g.Winner != "a" || g.Rule != "source-authority" {
		t.Fatalf("higher authority must win: %+v", g)
	}

	// Later valid_from breaks authority ties (authority > 3 → resolvable).
	g = ResolveConflict([]ClaimInfo{
		{ID: "a", Authority: 5, ValidFrom: t1, AssertedAt: t0},
		{ID: "b", Authority: 5, ValidFrom: t0, AssertedAt: t1},
	})
	if g.Winner != "a" || g.Rule != "later-valid-from" {
		t.Fatalf("later valid_from must win: %+v", g)
	}

	// Tie at authority ≤ 3 → unresolved, lowers answerability (§8.7).
	g = ResolveConflict([]ClaimInfo{
		{ID: "a", Authority: 2, ValidFrom: t0, AssertedAt: t0},
		{ID: "b", Authority: 2, ValidFrom: t0, AssertedAt: t0},
	})
	if !g.Unresolved || g.Winner != "" {
		t.Fatalf("authority≤3 tie must be unresolved: %+v", g)
	}
}
