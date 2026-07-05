// memctl is the memba admin CLI (spec §14.1): migrations, dev tokens,
// namespaces, the review queue, and repo bootstrap (spec §20, local-file
// stage: docs, build files, CI configs ingested and seed procedure cards
// proposed + branch-verified).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/lazorfuzz/memba/internal/authz"
	"github.com/lazorfuzz/memba/internal/store/postgres"
	"github.com/lazorfuzz/memba/pkg/client"
	"github.com/lazorfuzz/memba/pkg/memory"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "migrate":
		cmdMigrate(os.Args[2:])
	case "token":
		cmdToken(os.Args[2:])
	case "namespace":
		cmdNamespace(os.Args[2:])
	case "review":
		cmdReview(os.Args[2:])
	case "bootstrap":
		cmdBootstrap(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  memctl migrate                                        (env MEMD_PG_DSN)
  memctl token --tenant T --principal P [--subjects a,b] [--ttl 24h]  (env MEMD_TOKEN_SECRET)
  memctl namespace --base URL --token TOK --id /acme/x --kind repo
  memctl review --base URL --token TOK [--namespace NS] [--approve CARD_ID] [--reject CARD_ID] [--reason R]
  memctl bootstrap --base URL --token TOK --repo-dir PATH --repo NAME --namespace NS`)
	os.Exit(2)
}

func cmdMigrate(args []string) {
	dsn := os.Getenv("MEMD_PG_DSN")
	if dsn == "" {
		fatal("MEMD_PG_DSN not set")
	}
	if err := postgres.Migrate(dsn); err != nil {
		fatal("migrate: %v", err)
	}
	fmt.Println("migrations applied")
}

func cmdToken(args []string) {
	fs := flag.NewFlagSet("token", flag.ExitOnError)
	tenant := fs.String("tenant", "", "tenant id")
	principal := fs.String("principal", "", "principal, e.g. user:jdoe or agent:coder-1")
	subjects := fs.String("subjects", "", "comma-separated extra acl subjects")
	ttl := fs.Duration("ttl", 0, "token ttl (0 = no expiry)")
	fs.Parse(args)
	secret := os.Getenv("MEMD_TOKEN_SECRET")
	if secret == "" || *tenant == "" || *principal == "" {
		fatal("need MEMD_TOKEN_SECRET, --tenant, --principal")
	}
	var subs []string
	for _, s := range strings.Split(*subjects, ",") {
		if s = strings.TrimSpace(s); s != "" {
			subs = append(subs, s)
		}
	}
	tok, err := authz.Sign([]byte(secret), memory.Principal{TenantID: *tenant, ID: *principal, ACLSubjects: subs}, *ttl)
	if err != nil {
		fatal("sign: %v", err)
	}
	fmt.Println(tok)
}

func cmdNamespace(args []string) {
	fs := flag.NewFlagSet("namespace", flag.ExitOnError)
	base := fs.String("base", "http://localhost:8080", "")
	token := fs.String("token", os.Getenv("MEMBA_TOKEN"), "")
	id := fs.String("id", "", "namespace path id")
	kind := fs.String("kind", "repo", "org|team|service|repo|incident_family")
	parent := fs.String("parent", "", "parent namespace id")
	fs.Parse(args)
	if *id == "" || *token == "" {
		fatal("--id and --token required")
	}
	c := client.New(*base, *token)
	if err := c.UpsertNamespace(context.Background(), memory.Namespace{ID: *id, Kind: *kind, ParentID: *parent}); err != nil {
		fatal("namespace: %v", err)
	}
	fmt.Println("ok:", *id)
}

func cmdReview(args []string) {
	fs := flag.NewFlagSet("review", flag.ExitOnError)
	base := fs.String("base", "http://localhost:8080", "")
	token := fs.String("token", os.Getenv("MEMBA_TOKEN"), "")
	namespace := fs.String("namespace", "", "")
	approve := fs.String("approve", "", "card id to approve")
	reject := fs.String("reject", "", "card id to reject")
	reason := fs.String("reason", "", "")
	fs.Parse(args)
	if *token == "" {
		fatal("--token required")
	}
	c := client.New(*base, *token)
	ctx := context.Background()
	switch {
	case *approve != "":
		resp, err := c.Review(ctx, *approve, memory.ReviewRequest{Decision: "approve", Reason: *reason})
		if err != nil {
			fatal("approve: %v", err)
		}
		fmt.Printf("card %s → %s\n", resp.CardID, resp.Status)
	case *reject != "":
		resp, err := c.Review(ctx, *reject, memory.ReviewRequest{Decision: "reject", Reason: *reason})
		if err != nil {
			fatal("reject: %v", err)
		}
		fmt.Printf("card %s → %s\n", resp.CardID, resp.Status)
	default:
		cards, err := c.ListCards(ctx, *namespace, memory.StatusProposed, 100)
		if err != nil {
			fatal("list: %v", err)
		}
		if len(cards) == 0 {
			fmt.Println("review queue empty")
			return
		}
		for _, card := range cards {
			fmt.Printf("%s  [%s]  %s\n    %s\n", card.ID, card.CardType, card.Title, firstLine(card.Body))
		}
	}
}

// --- bootstrap (spec §20, steps 1–3 for a local checkout) --------------------

var bootstrapGlobs = []string{
	"README*", "readme*", "docs/**/*.md", "doc/**/*.md", "adr/**/*.md", "ADR*/*.md",
	"CODEOWNERS", ".github/CODEOWNERS", "Makefile", "justfile", "package.json",
	".github/workflows/*.yml", ".github/workflows/*.yaml", "Dockerfile", "docker-compose.yml",
}

func cmdBootstrap(args []string) {
	fs := flag.NewFlagSet("bootstrap", flag.ExitOnError)
	base := fs.String("base", "http://localhost:8080", "")
	token := fs.String("token", os.Getenv("MEMBA_TOKEN"), "")
	repoDir := fs.String("repo-dir", "", "local checkout path")
	repo := fs.String("repo", "", "repo name for git:// URIs")
	namespace := fs.String("namespace", "", "target namespace id")
	fs.Parse(args)
	if *repoDir == "" || *repo == "" || *namespace == "" || *token == "" {
		fatal("--repo-dir, --repo, --namespace, --token required")
	}
	c := client.New(*base, *token)
	ctx := context.Background()

	if err := c.UpsertNamespace(ctx, memory.Namespace{ID: *namespace, Kind: "repo"}); err != nil {
		fatal("namespace: %v", err)
	}

	// 1. Ingest docs/build/CI files verbatim.
	ingested := 0
	seen := map[string]struct{}{}
	for _, glob := range bootstrapGlobs {
		matches, _ := filepath.Glob(filepath.Join(*repoDir, glob))
		deep, _ := filepath.Glob(filepath.Join(*repoDir, strings.ReplaceAll(glob, "**/", "")))
		for _, m := range append(matches, deep...) {
			rel, err := filepath.Rel(*repoDir, m)
			if err != nil {
				continue
			}
			if _, dup := seen[rel]; dup {
				continue
			}
			seen[rel] = struct{}{}
			info, err := os.Stat(m)
			if err != nil || info.IsDir() || info.Size() > 1<<20 {
				continue
			}
			body, err := os.ReadFile(m)
			if err != nil || len(body) == 0 {
				continue
			}
			_, err = c.InsertEvidence(ctx, memory.InsertRequest{
				NamespaceID: *namespace,
				SourceType:  memory.SourceGit,
				SourceURI:   "git://" + *repo + "/" + filepath.ToSlash(rel),
				Body:        string(body),
				Metadata:    map[string]any{"repo": *repo, "path": filepath.ToSlash(rel), "bootstrap": true},
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "skip %s: %v\n", rel, err)
				continue
			}
			ingested++
		}
	}

	// 2. Extract seed procedure cards from Makefile/justfile targets.
	proposed, promoted := 0, 0
	for _, mk := range []string{"Makefile", "justfile"} {
		mkPath := filepath.Join(*repoDir, mk)
		body, err := os.ReadFile(mkPath)
		if err != nil {
			continue
		}
		for _, target := range makeTargets(string(body)) {
			resp, err := c.Propose(ctx, memory.ProposalRequest{
				NamespaceID: *namespace,
				CardType:    memory.CardProcedure,
				Title:       fmt.Sprintf("%s: run `make %s`", *repo, target),
				Body:        fmt.Sprintf("The %s target %q is defined in %s. Run `make %s` from the repo root.", *repo, target, mk, target),
				Subject:     *repo,
				Structured: map[string]any{
					"steps":          []string{fmt.Sprintf("run `make %s`", target)},
					"verify_command": "make " + target,
				},
				SourceRefs: []memory.SourceRef{{
					SourceURI: "git://" + *repo + "/" + mk,
					Path:      mk,
					Quote:     target + ":",
				}},
			})
			if err != nil {
				continue
			}
			proposed++
			// 3. Verify immediately (P3 branch check → instant promotion when
			// the repo is available under the verifier's repo root).
			if v, err := c.Verify(ctx, memory.VerifyRequest{
				Target:           "card:" + resp.CardID,
				VerificationType: memory.VerifyCodeBranchCheck,
				Repo:             *repo,
			}); err == nil && v.Result == memory.VerifyPassed {
				promoted++
			}
		}
	}
	fmt.Printf("bootstrap report: %d files ingested · %d seed cards proposed · %d branch-verified\n",
		ingested, proposed, promoted)
	fmt.Println("review queue: memctl review --namespace " + *namespace)
}

var targetRe = regexp.MustCompile(`(?m)^([A-Za-z0-9][A-Za-z0-9_./-]*):(?:[^=]|$)`)

func makeTargets(body string) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, m := range targetRe.FindAllStringSubmatch(body, -1) {
		t := m[1]
		if t == "" || strings.HasPrefix(t, ".") {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
		if len(out) >= 8 { // extraction cap per document (§10.4)
			break
		}
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

