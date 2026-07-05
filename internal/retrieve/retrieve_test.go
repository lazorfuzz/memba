package retrieve

import (
	"testing"

	"github.com/lazorfuzz/memba/internal/store"
)

func c(kind, id string) store.Candidate { return store.Candidate{Kind: kind, ID: id} }

func TestFuseRRFBasics(t *testing.T) {
	// Item B appears in both lists → must outrank A and C which appear once.
	l1 := []store.Candidate{c("chunk", "A"), c("chunk", "B")}
	l2 := []store.Candidate{c("chunk", "B"), c("chunk", "C")}
	fused := FuseLists([][]store.Candidate{l1, l2}, 60, 10)
	if len(fused) != 3 {
		t.Fatalf("expected 3 fused, got %d", len(fused))
	}
	if fused[0].ID != "B" {
		t.Fatalf("expected B first (multi-list), got %s", fused[0].ID)
	}
	// rrf(B) = 1/(60+2) + 1/(60+1); rrf(A) = 1/(60+1)
	wantB := 1.0/62 + 1.0/61
	if diff := fused[0].Score - wantB; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("rrf score wrong: got %v want %v", fused[0].Score, wantB)
	}
}

func TestFuseRRFPoolCap(t *testing.T) {
	var l1 []store.Candidate
	for i := 0; i < 300; i++ {
		l1 = append(l1, c("chunk", string(rune('a'+i%26))+string(rune('0'+i/26))))
	}
	fused := FuseLists([][]store.Candidate{l1}, 60, 150)
	if len(fused) != 150 {
		t.Fatalf("pool cap not applied: %d", len(fused))
	}
}

func TestFuseRRFKindsDontCollide(t *testing.T) {
	// A card and a chunk sharing an id must not merge.
	fused := FuseLists([][]store.Candidate{
		{c("card", "X")}, {c("chunk", "X")},
	}, 60, 10)
	if len(fused) != 2 {
		t.Fatalf("card/chunk with same id merged: %d", len(fused))
	}
}

func TestAnalyze(t *testing.T) {
	aq := Analyze("How do I add a new invoice status safely to invoice_status.pb.go before the migration? See TICKET-123 and InvoiceStatus", nil)
	wantIdent := map[string]bool{}
	for _, id := range aq.Identifiers {
		wantIdent[id] = true
	}
	if !wantIdent["invoice_status"] && !wantIdent["invoice_status.pb.go"] {
		t.Fatalf("snake_case/path identifier missed: %v", aq.Identifiers)
	}
	if !wantIdent["TICKET-123"] {
		t.Fatalf("ticket pattern missed: %v", aq.Identifiers)
	}
	if !wantIdent["InvoiceStatus"] {
		t.Fatalf("CamelCase missed: %v", aq.Identifiers)
	}
	if !aq.Temporal {
		t.Fatal("temporal qualifier 'before the' not detected")
	}
	for _, term := range aq.Terms {
		if term == "the" || term == "how" {
			t.Fatalf("stopword leaked into terms: %v", aq.Terms)
		}
	}
}
