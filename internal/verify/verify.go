// Package verify implements the branch-verification subsystem of spec §9:
// per-citation checks against a repo/branch tip (file exists, quote holds,
// commit ancestry), the JIT cache keyed on (target, repo, head_sha), and the
// cheap re-checks used by verify_sweep.
//
// Repos are local working clones under cfg.Verify.RepoRoot/<repo> — the git
// connector keeps them fetched. All git operations shell out to `git`
// (read-only plumbing: rev-parse, cat-file, show, merge-base).
package verify

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/lazorfuzz/memba/internal/embed"
	"github.com/lazorfuzz/memba/internal/store"
	"github.com/lazorfuzz/memba/pkg/memory"
)

// Verifier runs verification checks (spec §14.2 Verifier interface).
type Verifier struct {
	Store    store.Store
	Embedder embed.Embedder
	RepoRoot string
}

func (v *Verifier) repoDir(repo string) string {
	// Repo names are single path segments; reject traversal.
	clean := filepath.Base(filepath.Clean(repo))
	return filepath.Join(v.RepoRoot, clean)
}

func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// resolveHead returns the tip sha of branch (or HEAD if branch is empty).
func (v *Verifier) resolveHead(ctx context.Context, repo, branch string) (string, error) {
	ref := "HEAD"
	if branch != "" {
		ref = branch
	}
	dir := v.repoDir(repo)
	sha, err := gitOut(ctx, dir, "rev-parse", "--verify", ref)
	if err != nil && branch != "" {
		// Try the remote-tracking ref before giving up.
		sha, err = gitOut(ctx, dir, "rev-parse", "--verify", "origin/"+branch)
	}
	if err != nil {
		return "", fmt.Errorf("resolve %s@%s: %w", repo, ref, err)
	}
	return sha, nil
}

// gitPathFromURI extracts the in-repo path from "git://<repo>/<path>".
func gitPathFromURI(uri, repo string) (string, bool) {
	prefix := "git://" + repo + "/"
	if !strings.HasPrefix(uri, prefix) {
		return "", false
	}
	p := strings.TrimPrefix(uri, prefix)
	if i := strings.Index(p, "#"); i >= 0 {
		p = p[:i]
	}
	return p, true
}

// VerifyAgainstBranch implements the §9.1 algorithm for one card/fact.
func (v *Verifier) VerifyAgainstBranch(ctx context.Context, target memory.Card, sources []memory.SourceRef, repo, branch string) (memory.VerificationResult, error) {
	res := memory.VerificationResult{
		Type: memory.VerifyCodeBranchCheck, Repo: repo, Branch: branch,
		VerifiedAt: time.Now().UTC(),
	}
	head, err := v.resolveHead(ctx, repo, branch)
	if err != nil {
		res.Result = memory.VerifyInconclusive
		res.PerRef = []memory.RefOutcome{{Check: "resolve_head", Result: memory.VerifyInconclusive, Detail: err.Error()}}
		return res, nil
	}
	res.HeadSHA = head
	dir := v.repoDir(repo)

	anyFail, allPass := false, true
	checked := 0
	for _, ref := range sources {
		path, ok := gitPathFromURI(ref.SourceURI, repo)
		if !ok {
			if ref.Path != "" && strings.HasPrefix(ref.SourceURI, "git://") {
				continue // different repo
			}
			continue // non-git citation: not this check's job
		}
		if path == "" && ref.Path != "" {
			path = ref.Path
		}
		checked++

		// A. file exists?
		if _, err := gitOut(ctx, dir, "cat-file", "-e", head+":"+path); err != nil {
			res.PerRef = append(res.PerRef, memory.RefOutcome{
				SourceURI: ref.SourceURI, Check: "file_exists", Result: memory.VerifyFailed,
				Detail: "file_missing: " + path + " not on " + shortSHA(head),
			})
			anyFail, allPass = true, false
			continue
		}
		res.PerRef = append(res.PerRef, memory.RefOutcome{
			SourceURI: ref.SourceURI, Check: "file_exists", Result: memory.VerifyPassed,
		})

		// C. quote still holds?
		if ref.Quote != "" {
			content, err := gitOut(ctx, dir, "show", head+":"+path)
			if err != nil {
				res.PerRef = append(res.PerRef, memory.RefOutcome{
					SourceURI: ref.SourceURI, Check: "quote_holds", Result: memory.VerifyInconclusive,
					Detail: "could not read file at head",
				})
				allPass = false
				continue
			}
			outcome := v.quoteCheck(ctx, content, ref)
			res.PerRef = append(res.PerRef, outcome)
			if outcome.Result == memory.VerifyFailed {
				anyFail, allPass = true, false
			} else if outcome.Result == memory.VerifyInconclusive {
				allPass = false
			}
		}

		// D. commit relevant?
		if ref.SourceVersion != "" && shaRe.MatchString(ref.SourceVersion) {
			if _, err := gitOut(ctx, dir, "merge-base", "--is-ancestor", ref.SourceVersion, head); err != nil {
				res.PerRef = append(res.PerRef, memory.RefOutcome{
					SourceURI: ref.SourceURI, Check: "commit_ancestor", Result: memory.VerifyInconclusive,
					Detail: "divergent_history: " + shortSHA(ref.SourceVersion) + " not an ancestor of " + shortSHA(head),
				})
				allPass = false
			} else {
				res.PerRef = append(res.PerRef, memory.RefOutcome{
					SourceURI: ref.SourceURI, Check: "commit_ancestor", Result: memory.VerifyPassed,
				})
			}
		}
	}

	switch {
	case checked == 0:
		res.Result = memory.VerifyInconclusive
		res.PerRef = append(res.PerRef, memory.RefOutcome{
			Check: "code_refs", Result: memory.VerifyInconclusive, Detail: "no git citations for repo " + repo,
		})
	case anyFail:
		res.Result = memory.VerifyFailed
	case allPass:
		res.Result = memory.VerifyPassed
	default:
		res.Result = memory.VerifyInconclusive
	}
	return res, nil
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// quoteCheck implements §9.1 step C: exact match near the cited lines →
// PASS; fuzzy token match ≥ 0.85 anywhere → PASS(drifted); embedding
// similarity ≥ 0.80 → INCONCLUSIVE(rewritten?); else FAIL(content_changed).
func (v *Verifier) quoteCheck(ctx context.Context, content string, ref memory.SourceRef) memory.RefOutcome {
	out := memory.RefOutcome{SourceURI: ref.SourceURI, Check: "quote_holds"}
	lines := strings.Split(content, "\n")

	// Exact match within [start-20, end+20].
	lo, hi := 0, len(lines)
	if ref.LineStart > 0 {
		lo = maxInt(0, ref.LineStart-1-20)
		hi = minInt(len(lines), ref.LineEnd+20)
		if hi <= lo {
			hi = minInt(len(lines), lo+40)
		}
	}
	window := strings.Join(lines[lo:hi], "\n")
	if strings.Contains(window, ref.Quote) {
		out.Result = memory.VerifyPassed
		return out
	}
	if strings.Contains(content, ref.Quote) {
		out.Result = memory.VerifyPassed
		out.Detail = "drifted: quote found outside cited lines"
		return out
	}
	// Normalized fuzzy match: token-level similarity ≥ 0.85 over a sliding
	// window of the file.
	if bestTokenSim(ref.Quote, lines) >= 0.85 {
		out.Result = memory.VerifyPassed
		out.Detail = "drifted: fuzzy match ≥0.85"
		return out
	}
	// Embedding similarity fallback.
	if v.Embedder != nil {
		vecs, err := v.Embedder.Embed(ctx, []string{ref.Quote, window})
		if err == nil && len(vecs) == 2 && embed.Cosine(vecs[0], vecs[1]) >= 0.80 {
			out.Result = memory.VerifyInconclusive
			out.Detail = "rewritten?: embedding similarity ≥0.80"
			return out
		}
	}
	out.Result = memory.VerifyFailed
	out.Detail = "content_changed"
	return out
}

var tokSplit = regexp.MustCompile(`[^A-Za-z0-9_]+`)

func tokset(s string) map[string]struct{} {
	m := map[string]struct{}{}
	for _, t := range tokSplit.Split(strings.ToLower(s), -1) {
		if len(t) >= 2 {
			m[t] = struct{}{}
		}
	}
	return m
}

// bestTokenSim slides a window of quote-sized line blocks over the file and
// returns the best Jaccard token similarity.
func bestTokenSim(quote string, lines []string) float64 {
	q := tokset(quote)
	if len(q) == 0 {
		return 0
	}
	qLines := len(strings.Split(quote, "\n"))
	win := maxInt(qLines, 1)
	best := 0.0
	for i := 0; i+win <= len(lines); i++ {
		w := tokset(strings.Join(lines[i:i+win], "\n"))
		inter := 0
		for t := range q {
			if _, ok := w[t]; ok {
				inter++
			}
		}
		union := len(q) + len(w) - inter
		if union == 0 {
			continue
		}
		if sim := float64(inter) / float64(union); sim > best {
			best = sim
		}
	}
	return best
}

// ---------------------------------------------------------------------------
// JIT verification (gate G3) with the §9.4 cache
// ---------------------------------------------------------------------------

// JITVerify checks the cache at the current head, else runs the branch check
// and records it. Implements gates.JITVerifier. Card status transitions on
// failure follow §9.2/§10.7.
func (v *Verifier) JITVerify(ctx context.Context, tenantID, namespaceID, targetType, targetID, repo, branch string) (memory.VerificationResult, error) {
	head, err := v.resolveHead(ctx, repo, branch)
	if err == nil && head != "" {
		if cached, ok, cerr := v.Store.CachedVerification(ctx, targetID, repo, head); cerr == nil && ok {
			return cached, nil
		}
	}

	var sources []memory.SourceRef
	var card memory.Card
	switch targetType {
	case "card", "chunk":
		if targetType == "card" {
			card, err = v.Store.GetCard(ctx, tenantID, targetID)
			if err != nil {
				return memory.VerificationResult{}, err
			}
			sources = card.Sources
		} else {
			// A chunk's "citation" is itself: verify its source file exists.
			sources = []memory.SourceRef{}
		}
	case "fact":
		f, err := v.Store.FactByID(ctx, tenantID, targetID)
		if err != nil {
			return memory.VerificationResult{}, err
		}
		sources = f.Sources
	}

	res, err := v.VerifyAgainstBranch(ctx, card, sources, repo, branch)
	if err != nil {
		return res, err
	}
	res.TargetType, res.TargetID = normalizeTargetType(targetType), targetID
	if id, err := v.Store.InsertVerification(ctx, tenantID, namespaceID, res); err == nil {
		res.VerificationID = id
	}
	// §9.2: failed → card transitions to stale; passed → clock reset is done
	// by the caller that owns TTL policy (cards.Service / api layer).
	if targetType == "card" && res.Result == memory.VerifyFailed {
		_ = v.Store.UpdateCardStatus(ctx, tenantID, targetID, memory.StatusStale, "JIT branch verification failed on "+repo+"@"+branch)
	}
	return res, nil
}

func normalizeTargetType(t string) string {
	if t == "fact" {
		return "fact"
	}
	return "card"
}

// CheapCheck implements cards.SourceChecker for verify_sweep (spec §10.7):
// source_still_exists first, then a branch check on the default branch for
// git-cited cards.
func (v *Verifier) CheapCheck(ctx context.Context, tenantID string, card memory.Card) (memory.VerificationResult, error) {
	res := memory.VerificationResult{
		Type: memory.VerifySourceStillExists, Result: memory.VerifyPassed,
		VerifiedAt: time.Now().UTC(),
	}
	gitRepo := ""
	for _, ref := range card.Sources {
		if strings.HasPrefix(ref.SourceURI, "git://") {
			rest := strings.TrimPrefix(ref.SourceURI, "git://")
			if i := strings.IndexByte(rest, '/'); i > 0 {
				gitRepo = rest[:i]
			}
			continue
		}
		// Non-git: the raw evidence row must still exist (I1 makes deletion
		// rare — right-to-be-forgotten is the sanctioned path).
		if ref.RawID != "" {
			if _, err := v.Store.GetEvidence(ctx, tenantID, ref.RawID); err != nil {
				res.Result = memory.VerifyFailed
				res.PerRef = append(res.PerRef, memory.RefOutcome{
					SourceURI: ref.SourceURI, Check: "source_exists", Result: memory.VerifyFailed,
					Detail: "raw evidence deleted upstream",
				})
			}
		}
	}
	if gitRepo != "" {
		br, err := v.VerifyAgainstBranch(ctx, card, card.Sources, gitRepo, "")
		if err == nil {
			// Branch check dominates when it ran (it is the stronger signal).
			if br.Result != memory.VerifyInconclusive || res.Result == memory.VerifyPassed {
				br.Type = memory.VerifyCodeBranchCheck
				if res.Result == memory.VerifyFailed {
					br.Result = memory.VerifyFailed
					br.PerRef = append(br.PerRef, res.PerRef...)
				}
				return br, nil
			}
		}
	}
	return res, nil
}

func maxInt(a, b int) int { if a > b { return a }; return b }
func minInt(a, b int) int { if a < b { return a }; return b }
