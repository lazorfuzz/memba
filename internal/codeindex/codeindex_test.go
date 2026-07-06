package codeindex

import "testing"

func TestGoSymbols(t *testing.T) {
	src := []byte(`package invoice

type InvoiceStatus int

const StatusDraft InvoiceStatus = 0

var transitionTable map[InvoiceStatus]bool

func ValidTransition(from, to InvoiceStatus) bool { return true }

func (s *Store) Apply(x InvoiceStatus) error { return nil }

func (g Gen[T]) Emit() {}
`)
	syms := GoSymbols("status.go", src)
	got := map[string]bool{}
	for _, s := range syms {
		got[s] = true
	}
	for _, want := range []string{
		"InvoiceStatus", "StatusDraft", "transitionTable", "ValidTransition",
		"Apply", "Store.Apply", "Emit", "Gen.Emit",
	} {
		if !got[want] {
			t.Errorf("symbol %q missing: %v", want, syms)
		}
	}
	if got["_"] {
		t.Error("blank identifier must not be indexed")
	}
}

func TestGoSymbolsFallsBackOnParseError(t *testing.T) {
	// Broken Go still yields regex-extracted symbols rather than nothing.
	syms := GoSymbols("broken.go", []byte("package x\nfunc Working() {\nfunc Broken( {"))
	found := false
	for _, s := range syms {
		if s == "Working" {
			found = true
		}
	}
	if !found {
		t.Fatalf("fallback extraction missed Working: %v", syms)
	}
}
