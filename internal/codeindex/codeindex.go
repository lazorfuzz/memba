// Package codeindex maintains per-(repo, head) symbol tables (spec §9.3):
// definitions extracted from source at a given commit, used by branch
// verification step B ("symbol exists?") and by ingest to enrich
// chunks.symbols.
//
// Go files are parsed with the stdlib go/ast (the spec's prescribed path);
// other languages fall back to the regex extractor in internal/chunk until
// tree-sitter grammars are vendored (an accuracy upgrade, not a schema
// change). Tables are cached per (repo, head_sha) with a small LRU.
package codeindex

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lazorfuzz/memba/internal/chunk"
)

// SymbolTable maps symbol name → files defining it, for one (repo, head).
type SymbolTable struct {
	Repo    string
	HeadSHA string
	// Defs: symbol name → repo-relative paths that define it.
	Defs map[string][]string
}

// Has reports whether the symbol is defined anywhere at this head.
func (t *SymbolTable) Has(symbol string) bool {
	if t == nil {
		return false
	}
	_, ok := t.Defs[symbol]
	return ok
}

// DefinedIn returns the files defining symbol (nil if absent).
func (t *SymbolTable) DefinedIn(symbol string) []string {
	if t == nil {
		return nil
	}
	return t.Defs[symbol]
}

// Index builds symbol tables from local git clones (the same clones branch
// verification uses, under repoRoot/<repo>).
type Index struct {
	RepoRoot string

	mu    sync.Mutex
	cache map[string]*SymbolTable // key repo+"@"+head
	order []string                // LRU order, oldest first
}

const cacheSize = 8

func New(repoRoot string) *Index {
	return &Index{RepoRoot: repoRoot, cache: map[string]*SymbolTable{}}
}

var indexableExt = map[string]bool{
	".go": true, ".py": true, ".js": true, ".ts": true, ".tsx": true, ".jsx": true,
	".java": true, ".rb": true, ".rs": true, ".c": true, ".h": true, ".cpp": true,
	".cc": true, ".cs": true, ".kt": true, ".swift": true, ".proto": true,
}

// At returns the symbol table for (repo, headSHA), building it on miss.
func (ix *Index) At(ctx context.Context, repo, headSHA string) (*SymbolTable, error) {
	key := repo + "@" + headSHA
	ix.mu.Lock()
	if t, ok := ix.cache[key]; ok {
		ix.mu.Unlock()
		return t, nil
	}
	ix.mu.Unlock()

	t, err := ix.build(ctx, repo, headSHA)
	if err != nil {
		return nil, err
	}
	ix.mu.Lock()
	if len(ix.order) >= cacheSize {
		delete(ix.cache, ix.order[0])
		ix.order = ix.order[1:]
	}
	ix.cache[key] = t
	ix.order = append(ix.order, key)
	ix.mu.Unlock()
	return t, nil
}

func (ix *Index) build(ctx context.Context, repo, headSHA string) (*SymbolTable, error) {
	dir := filepath.Join(ix.RepoRoot, filepath.Base(filepath.Clean(repo)))
	t := &SymbolTable{Repo: repo, HeadSHA: headSHA, Defs: map[string][]string{}}

	files, err := gitLines(ctx, dir, "ls-tree", "-r", "--name-only", headSHA)
	if err != nil {
		return nil, err
	}
	for _, path := range files {
		ext := strings.ToLower(filepath.Ext(path))
		if !indexableExt[ext] {
			continue
		}
		body, err := gitShow(ctx, dir, headSHA, path)
		if err != nil || len(body) == 0 || len(body) > 1<<20 {
			continue
		}
		var symbols []string
		if ext == ".go" {
			symbols = GoSymbols(path, body)
		} else {
			symbols = chunk.ExtractSymbols(path, string(body))
		}
		for _, s := range symbols {
			t.Defs[s] = append(t.Defs[s], path)
		}
	}
	return t, nil
}

// GoSymbols extracts top-level definitions (funcs, methods, types, consts,
// vars) from a Go file via go/ast. Parse errors fall back to the regex
// extractor rather than dropping the file.
func GoSymbols(path string, src []byte) []string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		return chunk.ExtractSymbols(path, string(src))
	}
	seen := map[string]struct{}{}
	var out []string
	add := func(name string) {
		if name == "" || name == "_" {
			return
		}
		if _, dup := seen[name]; dup {
			return
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			add(d.Name.Name)
			// Methods are also indexed as Type.Method for qualified lookups.
			if d.Recv != nil && len(d.Recv.List) == 1 {
				if recv := receiverTypeName(d.Recv.List[0].Type); recv != "" {
					add(recv + "." + d.Name.Name)
				}
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch sp := spec.(type) {
				case *ast.TypeSpec:
					add(sp.Name.Name)
				case *ast.ValueSpec:
					for _, n := range sp.Names {
						add(n.Name)
					}
				}
			}
		}
	}
	return out
}

func receiverTypeName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.StarExpr:
		return receiverTypeName(e.X)
	case *ast.IndexExpr: // generic receiver T[P]
		return receiverTypeName(e.X)
	case *ast.IndexListExpr:
		return receiverTypeName(e.X)
	default:
		return ""
	}
}

func gitLines(ctx context.Context, dir string, args ...string) ([]string, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines, nil
}

func gitShow(ctx context.Context, dir, sha, path string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", "show", sha+":"+path)
	cmd.Dir = dir
	return cmd.Output()
}
