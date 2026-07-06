// github is the PR + CI connector (spec §14.4): it polls the GitHub REST
// API for merged pull requests (title, description, review verdicts →
// github_pr evidence) and failed workflow runs (run + failed-job summaries
// → ci evidence). Stateless beyond a cursor file (last PR update time /
// last run id).
//
//	github-connector --owner acme --repo billing-api \
//	  --namespace /acme/payments/repos/billing-api --token $TOK \
//	  [--interval 5m] [--cursor-file ./data/connectors/github-acme-billing-api.json]
//
// GITHUB_TOKEN is optional for public repos (rate limits apply).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lazorfuzz/memba/pkg/client"
	"github.com/lazorfuzz/memba/pkg/memory"
)

type cursor struct {
	PRUpdatedAt time.Time `json:"pr_updated_at"`
	LastRunID   int64     `json:"last_run_id"`
}

func main() {
	base := flag.String("base", "http://localhost:8080", "memd base URL")
	token := flag.String("token", os.Getenv("MEMBA_TOKEN"), "memba bearer token")
	owner := flag.String("owner", "", "GitHub owner/org")
	repo := flag.String("repo", "", "GitHub repo name")
	namespace := flag.String("namespace", "", "target namespace")
	apiBase := flag.String("github-api", "https://api.github.com", "GitHub API base")
	cursorFile := flag.String("cursor-file", "", "cursor path (default ./data/connectors/github-<owner>-<repo>.json)")
	interval := flag.Duration("interval", 0, "poll interval; 0 = run once")
	limit := flag.Int("limit", 30, "max items per stream per cycle")
	flag.Parse()
	if *owner == "" || *repo == "" || *namespace == "" || *token == "" {
		fatal("--owner, --repo, --namespace, --token required")
	}
	if *cursorFile == "" {
		*cursorFile = filepath.Join("data", "connectors", fmt.Sprintf("github-%s-%s.json", *owner, *repo))
	}

	gh := &ghClient{base: *apiBase, token: os.Getenv("GITHUB_TOKEN"), hc: &http.Client{Timeout: 30 * time.Second}}
	mem := client.New(*base, *token)
	ctx := context.Background()
	if err := mem.UpsertNamespace(ctx, memory.Namespace{ID: *namespace, Kind: "repo"}); err != nil {
		fatal("namespace: %v", err)
	}

	for {
		cur := loadCursor(*cursorFile)
		prs, cis, err := syncOnce(ctx, gh, mem, *owner, *repo, *namespace, &cur, *limit)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sync: %v\n", err)
			if *interval == 0 {
				os.Exit(1)
			}
		} else {
			saveCursor(*cursorFile, cur)
			fmt.Printf("%s/%s: %d merged PRs, %d failed CI runs ingested\n", *owner, *repo, prs, cis)
		}
		if *interval == 0 {
			return
		}
		time.Sleep(*interval)
	}
}

func syncOnce(ctx context.Context, gh *ghClient, mem *client.Client, owner, repo, namespace string, cur *cursor, limit int) (prCount, ciCount int, err error) {
	// --- merged PRs (authority 4) -------------------------------------------
	var prs []struct {
		Number    int        `json:"number"`
		Title     string     `json:"title"`
		Body      string     `json:"body"`
		MergedAt  *time.Time `json:"merged_at"`
		UpdatedAt time.Time  `json:"updated_at"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
		MergeCommitSHA string `json:"merge_commit_sha"`
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls?state=closed&sort=updated&direction=desc&per_page=%d", owner, repo, limit)
	if err := gh.getJSON(ctx, path, &prs); err != nil {
		return 0, 0, fmt.Errorf("list PRs: %w", err)
	}
	maxUpdated := cur.PRUpdatedAt
	for _, pr := range prs {
		if pr.MergedAt == nil || !pr.UpdatedAt.After(cur.PRUpdatedAt) {
			continue
		}
		var reviews []struct {
			User struct {
				Login string `json:"login"`
			} `json:"user"`
			State string `json:"state"`
			Body  string `json:"body"`
		}
		_ = gh.getJSON(ctx, fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews?per_page=30", owner, repo, pr.Number), &reviews)

		b := &strings.Builder{}
		fmt.Fprintf(b, "PR #%d: %s\nauthor: %s · merged: %s · merge commit: %s\n\n%s\n",
			pr.Number, pr.Title, pr.User.Login, pr.MergedAt.Format(time.RFC3339), pr.MergeCommitSHA, pr.Body)
		for _, rv := range reviews {
			if strings.TrimSpace(rv.Body) == "" && rv.State == "COMMENTED" {
				continue
			}
			fmt.Fprintf(b, "\nreview by %s [%s]:\n%s\n", rv.User.Login, rv.State, rv.Body)
		}
		mergedAt := *pr.MergedAt
		_, err := mem.InsertEvidence(ctx, memory.InsertRequest{
			NamespaceID:   namespace,
			SourceType:    memory.SourceGithubPR,
			SourceURI:     fmt.Sprintf("pr://%s/%s/%d", owner, repo, pr.Number),
			SourceVersion: pr.MergeCommitSHA,
			Title:         pr.Title,
			Body:          b.String(),
			EventTime:     &mergedAt,
			Metadata:      map[string]any{"repo": repo, "pr": pr.Number, "author": pr.User.Login},
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "PR #%d: %v\n", pr.Number, err)
			continue
		}
		prCount++
		if pr.UpdatedAt.After(maxUpdated) {
			maxUpdated = pr.UpdatedAt
		}
	}
	cur.PRUpdatedAt = maxUpdated

	// --- failed workflow runs (authority 2, ci) ------------------------------
	var runs struct {
		WorkflowRuns []struct {
			ID         int64     `json:"id"`
			Name       string    `json:"name"`
			HeadSHA    string    `json:"head_sha"`
			HeadBranch string    `json:"head_branch"`
			Conclusion string    `json:"conclusion"`
			HTMLURL    string    `json:"html_url"`
			CreatedAt  time.Time `json:"created_at"`
		} `json:"workflow_runs"`
	}
	path = fmt.Sprintf("/repos/%s/%s/actions/runs?status=failure&per_page=%d", owner, repo, limit)
	if err := gh.getJSON(ctx, path, &runs); err != nil {
		return prCount, 0, fmt.Errorf("list workflow runs: %w", err)
	}
	maxRun := cur.LastRunID
	for _, run := range runs.WorkflowRuns {
		if run.ID <= cur.LastRunID {
			continue
		}
		var jobs struct {
			Jobs []struct {
				Name       string `json:"name"`
				Conclusion string `json:"conclusion"`
				Steps      []struct {
					Name       string `json:"name"`
					Conclusion string `json:"conclusion"`
				} `json:"steps"`
			} `json:"jobs"`
		}
		_ = gh.getJSON(ctx, fmt.Sprintf("/repos/%s/%s/actions/runs/%d/jobs?per_page=50", owner, repo, run.ID), &jobs)

		b := &strings.Builder{}
		fmt.Fprintf(b, "CI FAILURE: workflow %q on %s@%s\nrun: %s\n",
			run.Name, run.HeadBranch, run.HeadSHA, run.HTMLURL)
		for _, j := range jobs.Jobs {
			if j.Conclusion != "failure" {
				continue
			}
			fmt.Fprintf(b, "\njob %q failed:\n", j.Name)
			for _, s := range j.Steps {
				if s.Conclusion == "failure" {
					fmt.Fprintf(b, "  ERROR: step %q failed\n", s.Name)
				}
			}
		}
		created := run.CreatedAt
		_, err := mem.InsertEvidence(ctx, memory.InsertRequest{
			NamespaceID:   namespace,
			SourceType:    memory.SourceCI,
			SourceURI:     fmt.Sprintf("ci://%s/%s/runs/%d", owner, repo, run.ID),
			SourceVersion: run.HeadSHA,
			Title:         "CI failure: " + run.Name,
			Body:          b.String(),
			EventTime:     &created,
			Metadata:      map[string]any{"repo": repo, "workflow": run.Name, "branch": run.HeadBranch, "run_outcome": "failed"},
		})
		if err != nil {
			continue
		}
		ciCount++
		if run.ID > maxRun {
			maxRun = run.ID
		}
	}
	cur.LastRunID = maxRun
	return prCount, ciCount, nil
}

// --- GitHub REST helper --------------------------------------------------------

type ghClient struct {
	base  string
	token string
	hc    *http.Client
}

func (g *ghClient) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("GET %s: %d %s", path, resp.StatusCode, string(body))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// --- cursor ---------------------------------------------------------------------

func loadCursor(path string) cursor {
	var c cursor
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	return c
}

func saveCursor(path string, c cursor) {
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	b, _ := json.Marshal(c)
	_ = os.WriteFile(path, b, 0o644)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
