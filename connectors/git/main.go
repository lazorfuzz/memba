// git is the repository connector (spec §14.4): it maintains a local clone
// under the verifier's repo root (keeping branch verification and the code
// index live, §9.4) and incrementally ingests changed files as git evidence
// — diff-driven, re-indexing only what changed (§9.3).
//
// Stateless beyond a cursor: the last-ingested commit is stored in the
// clone at .git/memba-cursor; re-runs are cheap no-ops thanks to evidence
// idempotency.
//
//	git-connector --repo-url https://github.com/acme/billing-api.git \
//	  --repo billing-api --namespace /acme/payments/repos/billing-api \
//	  --token $TOK [--repo-root ./data/repos] [--branch main] [--interval 60s]
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lazorfuzz/memba/pkg/client"
	"github.com/lazorfuzz/memba/pkg/memory"
)

func main() {
	base := flag.String("base", "http://localhost:8080", "memd base URL")
	token := flag.String("token", os.Getenv("MEMBA_TOKEN"), "bearer token")
	repoURL := flag.String("repo-url", "", "clone URL (omit if the clone already exists)")
	repo := flag.String("repo", "", "repo name (clone dir + git:// URI prefix)")
	namespace := flag.String("namespace", "", "target namespace")
	repoRoot := flag.String("repo-root", "./data/repos", "clone root (= memd verify.repo_root)")
	branch := flag.String("branch", "", "branch to track (default: remote HEAD)")
	interval := flag.Duration("interval", 0, "poll interval; 0 = run once")
	maxBytes := flag.Int64("max-bytes", 1<<20, "skip files larger than this")
	flag.Parse()
	if *repo == "" || *namespace == "" || *token == "" {
		fatal("--repo, --namespace, --token required")
	}

	c := client.New(*base, *token)
	ctx := context.Background()
	if err := c.UpsertNamespace(ctx, memory.Namespace{ID: *namespace, Kind: "repo"}); err != nil {
		fatal("namespace: %v", err)
	}

	for {
		n, head, err := sync(ctx, c, *repoURL, *repo, *namespace, *repoRoot, *branch, *maxBytes)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sync: %v\n", err)
			if *interval == 0 {
				os.Exit(1)
			}
		} else {
			fmt.Printf("%s @ %.8s: %d files ingested\n", *repo, head, n)
		}
		if *interval == 0 {
			return
		}
		time.Sleep(*interval)
	}
}

func sync(ctx context.Context, c *client.Client, repoURL, repo, namespace, repoRoot, branch string, maxBytes int64) (int, string, error) {
	dir := filepath.Join(repoRoot, filepath.Base(filepath.Clean(repo)))

	// Clone or fetch (blobless clone keeps verification checkouts cheap, §9.4).
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if repoURL == "" {
			return 0, "", fmt.Errorf("no clone at %s and no --repo-url", dir)
		}
		if _, err := run(ctx, "", "git", "clone", "--filter=blob:none", repoURL, dir); err != nil {
			return 0, "", fmt.Errorf("clone: %w", err)
		}
	} else if repoURL != "" || hasRemote(ctx, dir) {
		if _, err := run(ctx, dir, "git", "fetch", "--all", "--prune"); err != nil {
			return 0, "", fmt.Errorf("fetch: %w", err)
		}
	}

	ref := "HEAD"
	if branch != "" {
		ref = "origin/" + branch
		if _, err := run(ctx, dir, "git", "rev-parse", "--verify", ref); err != nil {
			ref = branch
		}
	} else if hasRemote(ctx, dir) {
		if out, err := run(ctx, dir, "git", "rev-parse", "--verify", "origin/HEAD"); err == nil && out != "" {
			ref = "origin/HEAD"
		}
	}
	head, err := run(ctx, dir, "git", "rev-parse", ref)
	if err != nil {
		return 0, "", fmt.Errorf("resolve %s: %w", ref, err)
	}
	// Keep the local checkout at the tracked tip so verification and the
	// code index see current state.
	if branch != "" {
		_, _ = run(ctx, dir, "git", "checkout", "-q", "-B", branch, head)
	} else {
		_, _ = run(ctx, dir, "git", "checkout", "-q", head)
	}

	cursorPath := filepath.Join(dir, ".git", "memba-cursor")
	cursor := ""
	if b, err := os.ReadFile(cursorPath); err == nil {
		cursor = strings.TrimSpace(string(b))
	}
	if cursor == head {
		return 0, head, nil
	}

	// Changed files: full tree on first run, diff-driven after (§9.3).
	var paths []string
	if cursor != "" {
		if out, err := run(ctx, dir, "git", "diff", "--name-only", "--diff-filter=AMR", cursor, head); err == nil {
			paths = splitLines(out)
		}
	}
	if paths == nil {
		out, err := run(ctx, dir, "git", "ls-tree", "-r", "--name-only", head)
		if err != nil {
			return 0, "", err
		}
		paths = splitLines(out)
	}

	ingested := 0
	for _, path := range paths {
		if skipPath(path) {
			continue
		}
		body, err := run(ctx, dir, "git", "show", head+":"+path)
		if err != nil || body == "" || int64(len(body)) > maxBytes || !utf8.ValidString(body) {
			continue
		}
		_, err = c.InsertEvidence(ctx, memory.InsertRequest{
			NamespaceID:   namespace,
			SourceType:    memory.SourceGit,
			SourceURI:     "git://" + repo + "/" + path,
			SourceVersion: head,
			Body:          body,
			Metadata:      map[string]any{"repo": repo, "path": path, "branch": branch},
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", path, err)
			continue
		}
		ingested++
	}
	if err := os.WriteFile(cursorPath, []byte(head+"\n"), 0o644); err != nil {
		return ingested, head, fmt.Errorf("write cursor: %w", err)
	}
	return ingested, head, nil
}

func skipPath(path string) bool {
	base := filepath.Base(path)
	if strings.HasPrefix(base, ".") && base != ".gitattributes" && base != ".gitignore" {
		return true
	}
	for _, part := range strings.Split(path, "/") {
		switch part {
		case "node_modules", "vendor", "dist", "build", ".git":
			return true
		}
	}
	switch strings.ToLower(filepath.Ext(base)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".ico", ".pdf", ".zip", ".gz",
		".tar", ".zst", ".woff", ".woff2", ".ttf", ".so", ".dylib", ".exe", ".bin",
		".jar", ".class", ".o", ".a", ".sum", ".lock":
		return true
	}
	return false
}

func hasRemote(ctx context.Context, dir string) bool {
	out, err := run(ctx, dir, "git", "remote")
	return err == nil && strings.TrimSpace(out) != ""
}

func run(ctx context.Context, dir string, name string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	if out == nil {
		out = []string{}
	}
	return out
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
