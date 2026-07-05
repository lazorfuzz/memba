// Package chunk implements the per-kind chunking strategies of spec §10.2
// and identifier/symbol extraction for the trigram and symbol retrievers.
//
// Code chunking here is block-boundary based (blank-line + brace/indent
// heuristics with a hard line cap); AST-aware chunking via go/ast and
// tree-sitter lands with internal/codeindex in Phase 2 and only changes the
// boundaries, not the schema.
package chunk

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/lazorfuzz/memba/internal/tokens"
	"github.com/lazorfuzz/memba/pkg/memory"
)

// Piece is one chunk before storage.
type Piece struct {
	Ordinal    int
	Kind       string
	Path       string
	LineStart  int
	LineEnd    int
	TokenCount int
	Body       string
	IdentText  string
	Symbols    []string
}

// KindFor guesses the chunk kind from source type and path.
func KindFor(sourceType, path string) string {
	switch sourceType {
	case memory.SourceSlack:
		return memory.ChunkChat
	case memory.SourceCI:
		return memory.ChunkLog
	case memory.SourceGithubPR:
		return memory.ChunkDiff
	}
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".go", ".py", ".js", ".ts", ".tsx", ".jsx", ".java", ".rb", ".rs", ".c", ".h",
		".cpp", ".cc", ".cs", ".kt", ".swift", ".proto", ".sql", ".sh", ".bash":
		return memory.ChunkCode
	case ".yaml", ".yml", ".json", ".toml", ".ini", ".env", ".tf", ".hcl":
		return memory.ChunkConfig
	case ".md", ".rst", ".txt", ".adoc", "":
		return memory.ChunkProse
	default:
		return memory.ChunkProse
	}
}

// Split chunks a body per spec §10.2 based on kind.
func Split(kind, path, body string) []Piece {
	var pieces []Piece
	switch kind {
	case memory.ChunkCode, memory.ChunkConfig:
		pieces = splitCode(path, body, 300)
	case memory.ChunkChat:
		pieces = splitByBlankBlocks(body, 800, 0)
	case memory.ChunkLog:
		pieces = splitLog(body, 800)
	case memory.ChunkDiff:
		pieces = splitDiff(body)
	default: // prose
		pieces = splitProse(body, 800, 80)
	}
	for i := range pieces {
		pieces[i].Ordinal = i
		pieces[i].Kind = kind
		if pieces[i].Path == "" {
			pieces[i].Path = path
		}
		pieces[i].TokenCount = tokens.Estimate(pieces[i].Body)
		idents := ExtractIdentifiers(pieces[i].Body)
		pieces[i].IdentText = strings.Join(idents, " ")
		if kind == memory.ChunkCode {
			pieces[i].Symbols = ExtractSymbols(path, pieces[i].Body)
		}
	}
	return pieces
}

// splitCode: one chunk per top-level block, hard cap maxLines, prefixed with
// a file header comment (spec §10.2).
func splitCode(path, body string, maxLines int) []Piece {
	lines := strings.Split(body, "\n")
	var out []Piece
	header := "// file: " + path + "\n"
	start := 0
	flush := func(end int) { // [start, end) exclusive
		if end <= start {
			return
		}
		out = append(out, Piece{
			Path:      path,
			LineStart: start + 1,
			LineEnd:   end,
			Body:      header + strings.Join(lines[start:end], "\n"),
		})
		start = end
	}
	for i := 0; i < len(lines); i++ {
		blockLen := i - start + 1
		atBoundary := i+1 < len(lines) &&
			strings.TrimSpace(lines[i]) != "" &&
			strings.TrimSpace(lines[i+1]) == "" &&
			// close-brace or dedent at column 0 ends a top-level block
			(strings.HasPrefix(lines[i], "}") || strings.HasPrefix(lines[i], "end") || blockLen >= 40)
		if blockLen >= maxLines || (atBoundary && blockLen >= 10) {
			flush(i + 1)
		}
	}
	flush(len(lines))
	return out
}

// splitProse: heading-aware markdown splitter, 300–800 tokens with ~10–15%
// overlap (spec §10.2).
func splitProse(body string, maxTokens, overlapTokens int) []Piece {
	sections := splitOnHeadings(body)
	var out []Piece
	for _, sec := range sections {
		if tokens.Estimate(sec.text) <= maxTokens {
			if strings.TrimSpace(sec.text) != "" {
				out = append(out, Piece{LineStart: sec.lineStart, LineEnd: sec.lineEnd, Body: sec.text})
			}
			continue
		}
		// Oversized section: split on paragraph boundaries with overlap.
		paras := strings.Split(sec.text, "\n\n")
		var cur []string
		curTok := 0
		for _, p := range paras {
			pt := tokens.Estimate(p)
			if curTok+pt > maxTokens && curTok > 0 {
				text := strings.Join(cur, "\n\n")
				out = append(out, Piece{LineStart: sec.lineStart, LineEnd: sec.lineEnd, Body: text})
				// Overlap: carry the tail paragraph forward.
				if overlapTokens > 0 && len(cur) > 0 {
					tail := cur[len(cur)-1]
					if tokens.Estimate(tail) <= overlapTokens {
						cur = []string{tail}
						curTok = tokens.Estimate(tail)
					} else {
						cur, curTok = nil, 0
					}
				} else {
					cur, curTok = nil, 0
				}
			}
			cur = append(cur, p)
			curTok += pt
		}
		if curTok > 0 && strings.TrimSpace(strings.Join(cur, "\n\n")) != "" {
			out = append(out, Piece{LineStart: sec.lineStart, LineEnd: sec.lineEnd, Body: strings.Join(cur, "\n\n")})
		}
	}
	if len(out) == 0 && strings.TrimSpace(body) != "" {
		out = append(out, Piece{LineStart: 1, LineEnd: len(strings.Split(body, "\n")), Body: body})
	}
	return out
}

type section struct {
	text      string
	lineStart int
	lineEnd   int
}

var headingRe = regexp.MustCompile(`^#{1,6}\s`)

func splitOnHeadings(body string) []section {
	lines := strings.Split(body, "\n")
	var out []section
	start := 0
	for i := 1; i < len(lines); i++ {
		if headingRe.MatchString(lines[i]) {
			out = append(out, section{strings.Join(lines[start:i], "\n"), start + 1, i})
			start = i
		}
	}
	out = append(out, section{strings.Join(lines[start:], "\n"), start + 1, len(lines)})
	return out
}

// splitByBlankBlocks: chat threads → ≤ maxTokens blocks on message boundaries.
func splitByBlankBlocks(body string, maxTokens, _ int) []Piece {
	blocks := strings.Split(body, "\n\n")
	var out []Piece
	var cur []string
	curTok := 0
	flush := func() {
		if curTok == 0 {
			return
		}
		out = append(out, Piece{Body: strings.Join(cur, "\n\n")})
		cur, curTok = nil, 0
	}
	for _, b := range blocks {
		bt := tokens.Estimate(b)
		if curTok+bt > maxTokens && curTok > 0 {
			flush()
		}
		cur = append(cur, b)
		curTok += bt
	}
	flush()
	return out
}

var failureLineRe = regexp.MustCompile(`(?i)\b(error|fail(ed|ure)?|panic|fatal|exception|traceback|assert)\b`)

// splitLog keeps failure blocks and the tail of huge logs (spec §10.2).
func splitLog(body string, maxTokens int) []Piece {
	if tokens.Estimate(body) <= maxTokens {
		return []Piece{{Body: body, LineStart: 1, LineEnd: len(strings.Split(body, "\n"))}}
	}
	lines := strings.Split(body, "\n")
	var out []Piece
	// Failure blocks: ±10 lines of context around failure markers, merged.
	type span struct{ s, e int }
	var spans []span
	for i, l := range lines {
		if failureLineRe.MatchString(l) {
			s, e := max(0, i-10), min(len(lines), i+11)
			if len(spans) > 0 && s <= spans[len(spans)-1].e {
				spans[len(spans)-1].e = e
			} else {
				spans = append(spans, span{s, e})
			}
		}
	}
	budget := maxTokens
	for _, sp := range spans {
		text := strings.Join(lines[sp.s:sp.e], "\n")
		t := tokens.Estimate(text)
		if t > budget {
			break
		}
		out = append(out, Piece{Body: text, LineStart: sp.s + 1, LineEnd: sp.e})
		budget -= t
	}
	// Tail sample.
	tailStart := max(0, len(lines)-40)
	tail := strings.Join(lines[tailStart:], "\n")
	if tokens.Estimate(tail) <= maxTokens {
		out = append(out, Piece{Body: tail, LineStart: tailStart + 1, LineEnd: len(lines)})
	}
	if len(out) == 0 {
		head := strings.Join(lines[:min(len(lines), 60)], "\n")
		out = append(out, Piece{Body: head, LineStart: 1, LineEnd: min(len(lines), 60)})
	}
	return out
}

// splitDiff: PR description, then each diff hunk with its file header.
func splitDiff(body string) []Piece {
	lines := strings.Split(body, "\n")
	var out []Piece
	var cur []string
	curStart := 0
	var currentFile string
	flush := func(end int) {
		if len(cur) == 0 {
			return
		}
		text := strings.Join(cur, "\n")
		if strings.TrimSpace(text) != "" {
			out = append(out, Piece{Body: text, Path: currentFile, LineStart: curStart + 1, LineEnd: end})
		}
		cur = nil
	}
	for i, l := range lines {
		if strings.HasPrefix(l, "diff --git") || strings.HasPrefix(l, "+++ b/") {
			flush(i)
			curStart = i
			if strings.HasPrefix(l, "+++ b/") {
				currentFile = strings.TrimPrefix(l, "+++ b/")
			} else if parts := strings.Fields(l); len(parts) >= 4 {
				currentFile = strings.TrimPrefix(parts[3], "b/")
			}
		}
		cur = append(cur, l)
	}
	flush(len(lines))
	if len(out) == 0 && strings.TrimSpace(body) != "" {
		out = append(out, Piece{Body: body, LineStart: 1, LineEnd: len(lines)})
	}
	return out
}

// ---------------------------------------------------------------------------
// Identifier and symbol extraction (query analysis + trigram/symbol indexes)
// ---------------------------------------------------------------------------

var identRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]{2,}`)

// ExtractIdentifiers pulls CamelCase/snake_case identifiers, path-like
// strings, error codes, and TICKET-123 patterns (spec §8.2 step 3).
func ExtractIdentifiers(text string) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(s string) {
		if len(s) < 3 {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	for _, m := range regexp.MustCompile(`[A-Za-z0-9_./\-]+/[A-Za-z0-9_./\-]+\.[A-Za-z0-9]+`).FindAllString(text, -1) {
		add(m) // path-like
	}
	for _, m := range regexp.MustCompile(`\b[A-Z][A-Z0-9]+-\d+\b`).FindAllString(text, -1) {
		add(m) // TICKET-123
	}
	for _, m := range identRe.FindAllString(text, -1) {
		if isInterestingIdent(m) {
			add(m)
		}
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

func isInterestingIdent(s string) bool {
	if strings.Contains(s, "_") {
		return true
	}
	// CamelCase or mixedCase
	hasLower, hasUpper := false, false
	for _, r := range s {
		if r >= 'a' && r <= 'z' {
			hasLower = true
		}
		if r >= 'A' && r <= 'Z' {
			hasUpper = true
		}
	}
	return hasLower && hasUpper
}

var symbolDefRes = []*regexp.Regexp{
	regexp.MustCompile(`(?m)^\s*func\s+(?:\([^)]*\)\s*)?([A-Za-z_][A-Za-z0-9_]*)`), // go func / method
	regexp.MustCompile(`(?m)^\s*type\s+([A-Za-z_][A-Za-z0-9_]*)`),                  // go type
	regexp.MustCompile(`(?m)^\s*(?:def|class)\s+([A-Za-z_][A-Za-z0-9_]*)`),         // python
	regexp.MustCompile(`(?m)^\s*(?:export\s+)?(?:async\s+)?function\s+([A-Za-z_$][A-Za-z0-9_$]*)`), // js/ts
	regexp.MustCompile(`(?m)^\s*(?:export\s+)?(?:abstract\s+)?class\s+([A-Za-z_$][A-Za-z0-9_$]*)`),
	regexp.MustCompile(`(?m)^\s*(?:pub\s+)?(?:fn|struct|enum|trait)\s+([A-Za-z_][A-Za-z0-9_]*)`), // rust
	regexp.MustCompile(`(?m)^\s*(?:message|service|rpc|enum)\s+([A-Za-z_][A-Za-z0-9_]*)`),        // proto
}

// ExtractSymbols returns definitions found in a code chunk (regex-based
// until the tree-sitter code index lands, §9.3).
func ExtractSymbols(path, body string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, re := range symbolDefRes {
		for _, m := range re.FindAllStringSubmatch(body, -1) {
			name := m[1]
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	return out
}

func max(a, b int) int { if a > b { return a }; return b }
func min(a, b int) int { if a < b { return a }; return b }
