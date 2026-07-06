package consolidate

// Integration test: a full consolidate_ns run (C1–C7) against pgvector.
// Skipped unless MEMBA_TEST_PG_DSN is set.

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lazorfuzz/memba/internal/cards"
	"github.com/lazorfuzz/memba/internal/config"
	"github.com/lazorfuzz/memba/internal/embed"
	"github.com/lazorfuzz/memba/internal/ingest"
	"github.com/lazorfuzz/memba/internal/objstore"
	"github.com/lazorfuzz/memba/internal/store"
	"github.com/lazorfuzz/memba/internal/store/postgres"
	"github.com/lazorfuzz/memba/pkg/memory"
)

func TestConsolidateRunEndToEnd(t *testing.T) {
	dsn := os.Getenv("MEMBA_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MEMBA_TEST_PG_DSN not set")
	}
	if err := postgres.Migrate(dsn); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pg, err := postgres.New(ctx, dsn, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()

	tenant := "t-consol"
	ns := fmt.Sprintf("/t-consol/repos/x-%d", time.Now().UnixNano())
	if err := pg.UpsertNamespace(ctx, memory.Namespace{ID: ns, TenantID: tenant, Kind: "repo"}); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	emb := embed.NewHashEmbedder(1024)
	cardSvc := &cards.Service{Store: pg, Embedder: emb, Cfg: cfg}
	ing := &ingest.Service{Store: pg, Embedder: emb, Cfg: cfg}
	blobs := &objstore.FS{Root: t.TempDir()}
	principal := memory.Principal{TenantID: tenant, ID: "user:seeder", ACLSubjects: []string{"tenant:" + tenant}}

	// --- Seed C1: two near-identical active cards, same subject, distinct
	// subjects strings identical; each with 2 independent sources so P4
	// promotes them to active. Bodies differ by one word → cosine ≥ 0.92
	// under the hash embedder, but not body-equivalent (so propose-time dedup
	// creates a second card rather than folding citations).
	mkRefs := func(n int) []memory.SourceRef {
		return []memory.SourceRef{
			{SourceURI: fmt.Sprintf("git://repo-x/file%d.go", n), Quote: "q"},
			{SourceURI: fmt.Sprintf("doc://docs-x/readme%d.md", n), Quote: "q"},
		}
	}
	bodyA := "Run make proto after editing the invoice proto files; the generated go files are overwritten by codegen on every build run."
	bodyB := "Run make proto after editing the invoice proto files; the generated go files are overwritten by codegen on every single build run."
	respA, err := cardSvc.Propose(ctx, principal, memory.ProposalRequest{
		NamespaceID: ns, CardType: memory.CardConvention, Title: "Regenerate proto outputs",
		Body: bodyA, Subject: "repo-x", SourceRefs: mkRefs(1),
	})
	if err != nil || respA.Status != memory.StatusActive {
		t.Fatalf("seed A: %+v %v", respA, err)
	}
	respB, err := cardSvc.Propose(ctx, principal, memory.ProposalRequest{
		NamespaceID: ns, CardType: memory.CardConvention, Title: "Regenerate proto outputs after edits",
		Body: bodyB, Subject: "repo-x", SourceRefs: mkRefs(2),
	})
	if err != nil || respB.Status != memory.StatusActive {
		t.Fatalf("seed B: %+v %v", respB, err)
	}

	// --- Seed C6: the same failure signature in 3 failed agent runs.
	// URIs carry the run timestamp: evidence insert is idempotent on
	// (tenant, uri, version, hash), so reused URIs would dedupe to prior
	// test runs' rows in other namespaces.
	nano := time.Now().UnixNano()
	for i := 0; i < 3; i++ {
		_, err := ing.Insert(ctx, principal, memory.InsertRequest{
			NamespaceID: ns, SourceType: memory.SourceAgentRun,
			SourceURI: fmt.Sprintf("agentrun://run-%d-%d/log", nano, i),
			Body:      fmt.Sprintf("step 1 ok\nERROR: migration 00%d failed: relation \"invoices\" does not exist\n", i),
			Metadata:  map[string]any{"run_outcome": "failed"},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	cons := &Consolidator{Store: pg, Cards: cardSvc, Blobs: blobs, Cfg: cfg}
	stats, err := cons.Run(ctx, tenant, ns)
	if err != nil {
		t.Fatalf("consolidate run: %v (stats %+v)", err, stats)
	}

	// C1: one merged proposal citing the union, promoted via P4 (4 independent
	// sources), originals deprecated.
	if stats.C1MergedProposals != 1 || stats.C1Promoted != 1 {
		t.Fatalf("C1 stats wrong: %+v", stats)
	}
	a, _ := pg.GetCard(ctx, tenant, respA.CardID)
	b, _ := pg.GetCard(ctx, tenant, respB.CardID)
	if a.Status != memory.StatusDeprecated || b.Status != memory.StatusDeprecated {
		t.Fatalf("originals must be deprecated after promoted merge: %s %s", a.Status, b.Status)
	}
	merged, err := pg.ListCards(ctx, tenant, store.CardFilter{NamespaceID: ns, Status: memory.StatusActive, CardType: memory.CardConvention})
	if err != nil || len(merged) != 1 {
		t.Fatalf("expected exactly one active merged card: %v %d", err, len(merged))
	}
	if len(merged[0].Sources) < 4 {
		t.Fatalf("merged card must cite the union of sources, got %d", len(merged[0].Sources))
	}

	// C2: profile written and cites the merged card.
	rc, err := blobs.Get(ctx, ProfileKey(tenant, ns))
	if err != nil {
		t.Fatalf("profile not written: %v", err)
	}
	profile, _ := io.ReadAll(rc)
	rc.Close()
	if !strings.Contains(string(profile), "[card:"+merged[0].ID+"]") {
		t.Fatalf("profile must cite the merged card:\n%s", profile)
	}

	// C6: one recurring-failure gotcha proposal, still gated (agent_run-only
	// sources cannot self-promote).
	if stats.C6GotchaProposals != 1 {
		t.Fatalf("C6 stats wrong: %+v", stats)
	}
	gotchas, err := pg.ListCards(ctx, tenant, store.CardFilter{NamespaceID: ns, CardType: memory.CardGotcha})
	if err != nil || len(gotchas) != 1 {
		t.Fatalf("gotcha listing: %v %d", err, len(gotchas))
	}
	if gotchas[0].Status != memory.StatusProposed {
		t.Fatalf("failure-mined gotcha must stay proposed pending P1/P2, got %s", gotchas[0].Status)
	}
	if gotchas[0].CreatedFrom != memory.FromConsolidation {
		t.Fatalf("provenance wrong: %s", gotchas[0].CreatedFrom)
	}

	// Idempotence: a second nightly run must not re-merge or re-propose.
	stats2, err := cons.Run(ctx, tenant, ns)
	if err != nil {
		t.Fatal(err)
	}
	if stats2.C1MergedProposals != 0 || stats2.C6GotchaProposals != 0 {
		t.Fatalf("second run must be a no-op for C1/C6: %+v", stats2)
	}
}
