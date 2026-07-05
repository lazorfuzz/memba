// Package gates implements the deterministic post-rerank gates G1–G5 of
// spec §8.2 step 7 and the conflict-resolution order of §8.7. Gates move
// items between sections; they never re-score.
package gates

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/lazorfuzz/memba/internal/store"
	"github.com/lazorfuzz/memba/pkg/memory"
)

// JITVerifier runs just-in-time branch verification for gate G3 (spec §9).
type JITVerifier interface {
	JITVerify(ctx context.Context, tenantID, namespaceID, targetType, targetID, repo, branch string) (memory.VerificationResult, error)
}

// Gated is the sectioned output of the gate chain.
type Gated struct {
	Kept         []store.Candidate               // survive to pack assembly
	Superseded   []store.Candidate               // G2
	StaleFlagged []StaleItem                     // G3 failed
	Unverified   map[string]bool                 // candidate key → served verified:false (G3 inconclusive)
	Verified     map[string]bool                 // candidate key → verified:true (G3 passed)
	Conflicts    []memory.ConflictGroup          // G4
	DroppedByG5  int                             // diversity-cap drops (observability)
	G3Failures   []string                        // human-readable staleness gap notes (§8.2 step 9)
}

// StaleItem pairs a failed candidate with its verification detail.
type StaleItem struct {
	Candidate    store.Candidate
	Verification memory.VerificationResult
	Detail       string
}

func key(c store.Candidate) string { return c.Kind + ":" + c.ID }

// Options configures a gate run.
type Options struct {
	TenantID    string
	Repo        string
	Branch      string
	Verifier    JITVerifier
	MaxJITItems int // verify only the top-N code-touching items inline
	Now         time.Time
}

// Apply runs G2→G3→G5 over reranked candidates (G1 ACL filtering is pushed
// into SQL and re-asserted at pack assembly; G4 conflicts are computed from
// fact groups by Conflicts()).
func Apply(ctx context.Context, cands []store.Candidate, o Options) Gated {
	g := Gated{
		Unverified: map[string]bool{},
		Verified:   map[string]bool{},
	}
	if o.MaxJITItems <= 0 {
		o.MaxJITItems = 8
	}
	jitBudget := o.MaxJITItems

	perURI := map[string]int{}
	perRaw := map[string]int{}

	for _, c := range cands {
		// G2 temporal: superseded/retracted facts move to their own section.
		if c.Kind == "fact" && (c.Status == memory.FactSuperseded || c.Status == memory.FactRetracted) {
			g.Superseded = append(g.Superseded, c)
			continue
		}

		// G3 verification: JIT branch check for code-touching items when the
		// caller supplied repo+branch (spec §9.2 serving semantics).
		if o.Verifier != nil && o.Repo != "" && jitBudget > 0 && codeTouching(c, o.Repo) {
			jitBudget--
			vr, err := o.Verifier.JITVerify(ctx, o.TenantID, c.NamespaceID, c.Kind, c.ID, o.Repo, o.Branch)
			switch {
			case err != nil:
				g.Unverified[key(c)] = true
			case vr.Result == memory.VerifyFailed:
				g.StaleFlagged = append(g.StaleFlagged, StaleItem{
					Candidate: c, Verification: vr, Detail: staleDetail(c, vr),
				})
				g.G3Failures = append(g.G3Failures, staleDetail(c, vr))
				continue // excluded from Kept, served in stale_flagged
			case vr.Result == memory.VerifyPassed:
				g.Verified[key(c)] = true
			default:
				g.Unverified[key(c)] = true
			}
		} else if c.Kind == "card" {
			// No JIT run: within-TTL prior verification counts (I6).
			if c.LastVerifiedAt != nil && c.VerifyBy != nil && o.Now.Before(*c.VerifyBy) {
				g.Verified[key(c)] = true
			} else {
				g.Unverified[key(c)] = true
			}
		}

		// Stale cards that reached deep mode are always flagged.
		if c.Kind == "card" && c.Status == memory.StatusStale {
			g.StaleFlagged = append(g.StaleFlagged, StaleItem{
				Candidate: c,
				Detail:    "card status is stale: verification clock expired or last check failed (I6)",
			})
			continue
		}

		// G5 diversity: ≤ 3 items per source_uri, ≤ 5 per raw_id (§15.4 D5).
		if c.SourceURI != "" {
			if perURI[c.SourceURI] >= 3 {
				g.DroppedByG5++
				continue
			}
			perURI[c.SourceURI]++
		}
		if c.RawID != "" {
			if perRaw[c.RawID] >= 5 {
				g.DroppedByG5++
				continue
			}
			perRaw[c.RawID]++
		}

		g.Kept = append(g.Kept, c)
	}
	return g
}

func codeTouching(c store.Candidate, repo string) bool {
	if c.Kind == "chunk" {
		return strings.HasPrefix(c.SourceURI, "git://"+repo)
	}
	if c.Kind == "card" {
		return c.TTLClass == memory.TTLCode
	}
	return false
}

func staleDetail(c store.Candidate, vr memory.VerificationResult) string {
	var b strings.Builder
	b.WriteString("This memory may be stale: ")
	title := c.Title
	if title == "" {
		title = c.SourceURI
	}
	b.WriteString(title)
	for _, r := range vr.PerRef {
		if r.Result == memory.VerifyFailed {
			b.WriteString("; " + r.Check + " failed for " + r.SourceURI)
			if r.Detail != "" {
				b.WriteString(" (" + r.Detail + ")")
			}
		}
	}
	if vr.Branch != "" {
		b.WriteString(" [checked against branch " + vr.Branch + "]")
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// G4 — deterministic conflict resolution (spec §8.7)
// ---------------------------------------------------------------------------

// ClaimInfo carries the attributes the §8.7 winner rules need.
type ClaimInfo struct {
	ID              string
	Fact            *memory.Fact
	BranchVerified  bool // branch-verified passed, current
	RuntimeValidTTL bool // runtime-validated within TTL
	Authority       int  // best (lowest) source authority, §8.4
	ValidFrom       time.Time
	AssertedAt      time.Time
}

// ResolveConflict returns the winner annotation for a conflicting group:
// branch-verified > runtime-validated within TTL > higher source authority >
// later valid_from > later asserted_at. If the top two tie at authority
// level ≤ 3, the group is unresolved (spec §8.7).
func ResolveConflict(claims []ClaimInfo) memory.ConflictGroup {
	group := memory.ConflictGroup{}
	for _, c := range claims {
		if c.Fact != nil {
			group.Claims = append(group.Claims, c.Fact)
		} else {
			group.Claims = append(group.Claims, map[string]any{"id": c.ID})
		}
	}
	if len(claims) == 0 {
		group.Rule = "empty"
		return group
	}
	ordered := append([]ClaimInfo(nil), claims...)
	sort.SliceStable(ordered, func(i, j int) bool { return claimLess(ordered[j], ordered[i]) })
	winner := ordered[0]

	switch {
	case winner.BranchVerified && !ordered[min(1, len(ordered)-1)].BranchVerified:
		group.Rule = "branch-verified"
	case winner.RuntimeValidTTL && !ordered[min(1, len(ordered)-1)].RuntimeValidTTL:
		group.Rule = "runtime-validated"
	case len(ordered) > 1 && winner.Authority < ordered[1].Authority:
		group.Rule = "source-authority"
	case len(ordered) > 1 && winner.ValidFrom.After(ordered[1].ValidFrom):
		group.Rule = "later-valid-from"
	case len(ordered) > 1 && winner.AssertedAt.After(ordered[1].AssertedAt):
		group.Rule = "later-asserted-at"
	default:
		group.Rule = "tie"
	}

	// Tie at authority ≤ 3 with nothing else deciding → unresolved.
	if group.Rule == "tie" && winner.Authority <= 3 {
		group.Unresolved = true
		return group
	}
	group.Winner = winner.ID
	return group
}

// claimLess reports whether a loses to b under §8.7 ordering.
func claimLess(a, b ClaimInfo) bool {
	if a.BranchVerified != b.BranchVerified {
		return !a.BranchVerified
	}
	if a.RuntimeValidTTL != b.RuntimeValidTTL {
		return !a.RuntimeValidTTL
	}
	if a.Authority != b.Authority {
		return a.Authority > b.Authority // lower authority number wins
	}
	if !a.ValidFrom.Equal(b.ValidFrom) {
		return a.ValidFrom.Before(b.ValidFrom)
	}
	return a.AssertedAt.Before(b.AssertedAt)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
