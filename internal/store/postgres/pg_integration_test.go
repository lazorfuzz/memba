package postgres

// Integration tests run against a real pgvector Postgres when
// MEMBA_TEST_PG_DSN is set (spec §14.6); they are skipped otherwise.
//
//	docker run -d -p 5433:5432 -e POSTGRES_USER=memba -e POSTGRES_PASSWORD=memba \
//	  -e POSTGRES_DB=memba pgvector/pgvector:pg16
//	MEMBA_TEST_PG_DSN='postgres://memba:memba@localhost:5433/memba?sslmode=disable' go test ./internal/store/postgres

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/lazorfuzz/memba/internal/store"
	"github.com/lazorfuzz/memba/pkg/memory"
)

func testPG(t *testing.T) *PG {
	t.Helper()
	dsn := os.Getenv("MEMBA_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MEMBA_TEST_PG_DSN not set")
	}
	if err := Migrate(dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pg, err := New(context.Background(), dsn, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pg.Close)
	return pg
}

func mkNamespace(t *testing.T, pg *PG, tenant, id string) {
	t.Helper()
	if err := pg.UpsertNamespace(context.Background(), memory.Namespace{
		ID: id, TenantID: tenant, Kind: "repo",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestEvidenceIdempotency(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	ns := fmt.Sprintf("/t-idem/repos/r-%d", time.Now().UnixNano())
	mkNamespace(t, pg, "t-idem", ns)

	ev := memory.RawEvidence{
		NamespaceID: ns, SourceType: "doc", SourceURI: "doc://x/y.md",
		SourceVersion: "v1", ContentHash: fmt.Sprintf("h-%d", time.Now().UnixNano()),
		Body: "hello", ACL: memory.ACL{Read: []string{"tenant:t-idem"}},
	}
	first, deduped, err := pg.InsertEvidence(ctx, "t-idem", ev)
	if err != nil || deduped {
		t.Fatalf("first insert: err=%v deduped=%v", err, deduped)
	}
	second, deduped, err := pg.InsertEvidence(ctx, "t-idem", ev)
	if err != nil || !deduped || second.ID != first.ID {
		t.Fatalf("re-post must dedupe to same id: err=%v deduped=%v ids %s vs %s", err, deduped, first.ID, second.ID)
	}
}

func TestTenantIsolationAtEveryRetriever(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	nsA := fmt.Sprintf("/tenant-a/repos/iso-%d", suffix)
	nsB := fmt.Sprintf("/tenant-b/repos/iso-%d", suffix)
	mkNamespace(t, pg, "tenant-a", nsA)
	mkNamespace(t, pg, "tenant-b", nsB)

	// Identical content in both tenants (spec §14.6 tenant-isolation test).
	marker := fmt.Sprintf("zebra_unicorn_%d", suffix)
	for _, tc := range []struct{ tenant, ns string }{{"tenant-a", nsA}, {"tenant-b", nsB}} {
		ev := memory.RawEvidence{
			NamespaceID: tc.ns, SourceType: "doc",
			SourceURI: "doc://iso/readme.md", SourceVersion: tc.tenant,
			ContentHash: fmt.Sprintf("%s-%d", tc.tenant, suffix),
			Body:        "the " + marker + " migration runs make " + marker,
			ACL:         memory.ACL{Read: []string{"tenant:" + tc.tenant}},
		}
		stored, _, err := pg.InsertEvidence(ctx, tc.tenant, ev)
		if err != nil {
			t.Fatal(err)
		}
		err = pg.InsertChunks(ctx, tc.tenant, tc.ns, ev.ACL, []store.ChunkRow{{
			RawID: stored.ID, Ordinal: 0, Kind: "prose", TokenCount: 10,
			Body: ev.Body, IdentText: marker, Symbols: []string{},
		}})
		if err != nil {
			t.Fatal(err)
		}
	}

	// Tenant A must never see tenant B's rows, even when it names B's
	// namespace explicitly and holds B-looking subjects.
	filter := store.ScopeFilter{
		TenantID:    "tenant-a",
		Namespaces:  []string{nsA, nsB},
		ACLSubjects: []string{"tenant:tenant-a", "tenant:tenant-b"},
	}
	hits, err := pg.SearchChunksFTS(ctx, filter, marker, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.NamespaceID == nsB {
			t.Fatalf("TENANT ISOLATION VIOLATION: tenant-a query returned tenant-b chunk %s", h.ID)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("expected exactly tenant-a's chunk, got %d", len(hits))
	}
	hits, err = pg.SearchChunksTrgm(ctx, filter, marker, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.NamespaceID == nsB {
			t.Fatalf("trigram retriever leaked cross-tenant")
		}
	}
}

func TestACLFilterInSQL(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	ns := fmt.Sprintf("/t-acl/repos/r-%d", suffix)
	mkNamespace(t, pg, "t-acl", ns)

	marker := fmt.Sprintf("gryphon_%d", suffix)
	ev := memory.RawEvidence{
		NamespaceID: ns, SourceType: "doc", SourceURI: "doc://acl/secret.md",
		ContentHash: fmt.Sprintf("acl-%d", suffix),
		Body:        "secret " + marker,
		ACL:         memory.ACL{Read: []string{"team:secret-club"}},
	}
	stored, _, err := pg.InsertEvidence(ctx, "t-acl", ev)
	if err != nil {
		t.Fatal(err)
	}
	if err := pg.InsertChunks(ctx, "t-acl", ns, ev.ACL, []store.ChunkRow{{
		RawID: stored.ID, Ordinal: 0, Kind: "prose", TokenCount: 5, Body: ev.Body, Symbols: []string{},
	}}); err != nil {
		t.Fatal(err)
	}

	insider := store.ScopeFilter{TenantID: "t-acl", Namespaces: []string{ns}, ACLSubjects: []string{"team:secret-club"}}
	outsider := store.ScopeFilter{TenantID: "t-acl", Namespaces: []string{ns}, ACLSubjects: []string{"team:other", "tenant:t-acl"}}
	if hits, _ := pg.SearchChunksFTS(ctx, insider, marker, 10); len(hits) != 1 {
		t.Fatalf("insider should see 1 hit, got %d", len(hits))
	}
	if hits, _ := pg.SearchChunksFTS(ctx, outsider, marker, 10); len(hits) != 0 {
		t.Fatalf("ACL LEAK: outsider saw %d hits", len(hits))
	}
}

func TestJobQueueSkipLocked(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	if err := pg.EnqueueJob(ctx, "t-jobs", "decay_sweep", map[string]any{"k": 1}); err != nil {
		t.Fatal(err)
	}
	j, err := pg.DequeueJob(ctx, []string{"decay_sweep"})
	if err != nil || j == nil {
		t.Fatalf("dequeue: %v %v", j, err)
	}
	if err := pg.CompleteJob(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
}
