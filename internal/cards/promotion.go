// Package cards implements the card lifecycle: proposal, deterministic
// promotion (spec §10.6 — the ONLY path to active), dedup-at-propose
// (§10.8), decay clocks and dormancy (§10.7).
package cards

import (
	"strings"
	"time"

	"github.com/lazorfuzz/memba/pkg/memory"
)

// PromotionDecision is the outcome of evaluating §10.6 for one proposal.
type PromotionDecision struct {
	Promote    bool
	Rule       string  // "P1".."P5" when promoting
	Confidence float64 // assigned confidence per rule table
	Blocked    string  // "B1".."B5" when hard-blocked
	Hint       string  // human/agent-readable promotion hint
}

// Evidence the evaluator needs, gathered by the service layer.
type PromotionInput struct {
	Card            memory.Card
	Sources         []memory.SourceRef
	HumanApproved   bool                      // P1: latest review decision == approve
	Verifications   []memory.VerificationResult // recent verifications for this card
	NamespacePolicy memory.NamespacePolicy
	// AncestorPromotion: the card's namespace is an ancestor scope (org/team)
	// relative to where its evidence lives — requires P1 (spec §10.6).
	AncestorPromotion  bool
	MinIndependent     int  // P4 threshold (default 2)
	ChatOnlyCanPromote bool // config override (default false)
	Now                time.Time
}

// SourceTypeOf infers the evidence source type from a citation URI.
func SourceTypeOf(ref memory.SourceRef) string {
	uri := ref.SourceURI
	switch {
	case strings.HasPrefix(uri, "git://"):
		return memory.SourceGit
	case strings.HasPrefix(uri, "ci://"):
		return memory.SourceCI
	case strings.HasPrefix(uri, "doc://"), strings.HasPrefix(uri, "docs://"):
		return memory.SourceDoc
	case strings.HasPrefix(uri, "slack://"):
		return memory.SourceSlack
	case strings.HasPrefix(uri, "pr://"), strings.Contains(uri, "github.com") && strings.Contains(uri, "/pull/"):
		return memory.SourceGithubPR
	case strings.HasPrefix(uri, "incident://"):
		return memory.SourceIncident
	case strings.HasPrefix(uri, "ticket://"), strings.HasPrefix(uri, "jira://"):
		return memory.SourceTicket
	case strings.HasPrefix(uri, "agentrun://"), strings.HasPrefix(uri, "run://"):
		return memory.SourceAgentRun
	default:
		return memory.SourceManual
	}
}

// sourceRoot groups URIs for the P4 independence test ("different
// source_uri roots"): scheme + first path segment.
func sourceRoot(uri string) string {
	rest := uri
	scheme := ""
	if i := strings.Index(uri, "://"); i >= 0 {
		scheme = uri[:i]
		rest = uri[i+3:]
	}
	if j := strings.IndexByte(rest, '/'); j >= 0 {
		rest = rest[:j]
	}
	return scheme + "://" + rest
}

// Evaluate applies blockers B1–B5 then rules P1–P5 (spec §10.6).
func Evaluate(in PromotionInput) PromotionDecision {
	card := in.Card
	if in.MinIndependent <= 0 {
		in.MinIndependent = 2
	}

	// ---- Blockers -----------------------------------------------------------
	// B4: zero citations (I2).
	if len(in.Sources) == 0 {
		return PromotionDecision{Blocked: "B4", Hint: "add source_refs: a memory without citations cannot leave proposed (I2)"}
	}
	// B5: secret/PII/injection flags.
	if card.Status == memory.StatusQuarantined || flagTrue(card.Metadata, "injection_suspect") || flagTrue(card.Metadata, "secret_flag") {
		return PromotionDecision{Blocked: "B5", Hint: "card carries security flags; requires secops review"}
	}
	// B1: sole provenance is a failed run.
	failedRunOnly := true
	chatOnly := true
	for _, s := range in.Sources {
		st := SourceTypeOf(s)
		if st != memory.SourceAgentRun {
			failedRunOnly = false
		}
		if st != memory.SourceSlack && st != memory.SourceAgentRun {
			chatOnly = false
		}
	}
	runFailed := flagEquals(card.Metadata, "run_outcome", "failed")
	if failedRunOnly && runFailed {
		return PromotionDecision{Blocked: "B1", Hint: "sole provenance is a failed run: promotable only by human review or later runtime validation (§10.5)"}
	}
	// B3: contradicts a branch-verified active memory.
	if flagTrue(card.Metadata, "contradicts_branch_verified") {
		return PromotionDecision{Blocked: "B3", Hint: "contradicts a branch-verified active memory; filed as counter-evidence instead"}
	}

	// ---- Rules (first match wins; deterministic order P1→P5) ----------------
	// P1: human approval.
	if in.HumanApproved {
		return PromotionDecision{Promote: true, Rule: "P1", Confidence: 0.95}
	}
	// Ancestor-namespace promotion requires P1 (already handled) — everything
	// below is blocked for ancestor scopes (spec §10.6).
	if in.AncestorPromotion {
		return PromotionDecision{Hint: "org-wide promotion requires human review (P1) or independent promotion in ≥2 child namespaces"}
	}
	// P2: runtime validation green within 7 days. Failed-run reflections are
	// still eligible here — runtime validation IS the later validation (§10.5).
	for _, v := range in.Verifications {
		if v.Result != memory.VerifyPassed {
			continue
		}
		if (v.Type == memory.VerifyCommandSucceeded || v.Type == memory.VerifyTestPassed) &&
			in.Now.Sub(v.VerifiedAt) <= 7*24*time.Hour {
			return PromotionDecision{Promote: true, Rule: "P2", Confidence: 0.90}
		}
	}
	// P3: branch verification passed against the default branch.
	for _, v := range in.Verifications {
		if v.Type == memory.VerifyCodeBranchCheck && v.Result == memory.VerifyPassed {
			return PromotionDecision{Promote: true, Rule: "P3", Confidence: 0.85}
		}
	}
	// B2 gate on P4/P5: chat-only support cannot promote without review.
	if chatOnly && !in.ChatOnlyCanPromote {
		return PromotionDecision{Blocked: "B2", Hint: "chat/agent-run-only support: needs a code, CI, doc, or PR citation, or human review"}
	}
	// P4: ≥ N independent sources at authority ≤ 4 (different URI roots),
	// excluded entirely for failed-run reflections (§10.5).
	if !runFailed {
		roots := map[string]struct{}{}
		for _, s := range in.Sources {
			if memory.SourceAuthority(SourceTypeOf(s)) <= 4 {
				roots[sourceRoot(s.SourceURI)] = struct{}{}
			}
		}
		if len(roots) >= in.MinIndependent {
			return PromotionDecision{Promote: true, Rule: "P4", Confidence: 0.75}
		}
	}
	// P5: namespace policy whitelists the source type for auto-promotion.
	for _, s := range in.Sources {
		st := SourceTypeOf(s)
		for _, trusted := range in.NamespacePolicy.TrustedSources {
			if st == trusted {
				return PromotionDecision{Promote: true, Rule: "P5", Confidence: 0.70}
			}
		}
	}

	return PromotionDecision{Hint: promotionHint(in)}
}

func promotionHint(in PromotionInput) string {
	hints := []string{
		"what would promote it: human approval (P1)",
		"a green run of its verify_command within 7 days (P2)",
		"a passing branch verification on the default branch (P3)",
	}
	if !flagEquals(in.Card.Metadata, "run_outcome", "failed") {
		hints = append(hints, "≥2 independent authority≤4 sources (P4)")
	}
	return strings.Join(hints, "; ")
}

func flagTrue(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	v, ok := m[key]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}

func flagEquals(m map[string]any, key, want string) bool {
	if m == nil {
		return false
	}
	v, ok := m[key].(string)
	return ok && v == want
}
