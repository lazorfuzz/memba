package chunk

import (
	"strings"
	"testing"

	"github.com/lazorfuzz/memba/pkg/memory"
)

func TestKindFor(t *testing.T) {
	cases := []struct{ st, path, want string }{
		{memory.SourceGit, "a/b.go", memory.ChunkCode},
		{memory.SourceGit, "config.yaml", memory.ChunkConfig},
		{memory.SourceGit, "README.md", memory.ChunkProse},
		{memory.SourceSlack, "", memory.ChunkChat},
		{memory.SourceCI, "", memory.ChunkLog},
		{memory.SourceGithubPR, "", memory.ChunkDiff},
	}
	for _, c := range cases {
		if got := KindFor(c.st, c.path); got != c.want {
			t.Errorf("KindFor(%s,%s)=%s want %s", c.st, c.path, got, c.want)
		}
	}
}

func TestSplitProseHeadingAware(t *testing.T) {
	doc := "# Title\n\nintro paragraph\n\n## Build\n\nrun make test\n\n## Deploy\n\nuse the pipeline\n"
	pieces := Split(memory.ChunkProse, "README.md", doc)
	if len(pieces) < 3 {
		t.Fatalf("expected ≥3 heading sections, got %d", len(pieces))
	}
	for i, p := range pieces {
		if p.Ordinal != i {
			t.Fatalf("ordinals must be sequential")
		}
		if p.TokenCount <= 0 {
			t.Fatalf("token counts required")
		}
	}
}

func TestSplitCodeCapsAndHeader(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 700; i++ {
		b.WriteString("var x = 1\n")
	}
	pieces := Split(memory.ChunkCode, "big.go", b.String())
	if len(pieces) < 2 {
		t.Fatalf("oversized code must split, got %d pieces", len(pieces))
	}
	for _, p := range pieces {
		if p.LineEnd-p.LineStart+1 > 300 {
			t.Fatalf("chunk exceeds 300 lines: %d-%d", p.LineStart, p.LineEnd)
		}
		if !strings.HasPrefix(p.Body, "// file: big.go") {
			t.Fatal("code chunks must carry the file header prefix (§10.2)")
		}
	}
}

func TestSplitLogKeepsFailureBlocks(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 2000; i++ {
		b.WriteString("ok line\n")
	}
	b.WriteString("ERROR: test failed: TestInvoiceStatus\n")
	for i := 0; i < 2000; i++ {
		b.WriteString("more output\n")
	}
	pieces := Split(memory.ChunkLog, "ci.log", b.String())
	found := false
	for _, p := range pieces {
		if strings.Contains(p.Body, "TestInvoiceStatus") {
			found = true
		}
	}
	if !found {
		t.Fatal("failure block must survive log sampling")
	}
}

func TestExtractSymbols(t *testing.T) {
	src := "func VerifyInvoice(x int) {}\ntype InvoiceStatus struct{}\ndef status_check():\nclass Exporter:\n"
	syms := ExtractSymbols("x.go", src)
	want := []string{"VerifyInvoice", "InvoiceStatus", "status_check", "Exporter"}
	got := map[string]bool{}
	for _, s := range syms {
		got[s] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("symbol %s missed: %v", w, syms)
		}
	}
}

func TestExtractIdentifiers(t *testing.T) {
	ids := ExtractIdentifiers("edit invoice_status.proto then run make proto; see PAY-991 and getInvoiceStatus")
	got := map[string]bool{}
	for _, s := range ids {
		got[s] = true
	}
	for _, w := range []string{"invoice_status", "PAY-991", "getInvoiceStatus"} {
		if !got[w] {
			t.Errorf("identifier %s missed: %v", w, ids)
		}
	}
}
