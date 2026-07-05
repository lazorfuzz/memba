package cards

import (
	"testing"
	"time"

	"github.com/lazorfuzz/memba/pkg/memory"
)

var now = time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

func gitRef(uri string) memory.SourceRef { return memory.SourceRef{SourceURI: uri} }

func baseInput() PromotionInput {
	return PromotionInput{
		Card: memory.Card{Status: memory.StatusProposed},
		Sources: []memory.SourceRef{
			gitRef("git://billing-api/src/invoice/status.go"),
		},
		MinIndependent: 2,
		Now:            now,
	}
}

func TestB4_ZeroCitationsBlocks(t *testing.T) {
	in := baseInput()
	in.Sources = nil
	d := Evaluate(in)
	if d.Promote || d.Blocked != "B4" {
		t.Fatalf("expected B4 block, got %+v", d)
	}
}

func TestB5_SecurityFlagsBlock(t *testing.T) {
	in := baseInput()
	in.Card.Metadata = map[string]any{"injection_suspect": true}
	if d := Evaluate(in); d.Blocked != "B5" {
		t.Fatalf("expected B5, got %+v", d)
	}
}

func TestB1_FailedRunOnlyBlocks(t *testing.T) {
	in := baseInput()
	in.Sources = []memory.SourceRef{gitRef("agentrun://run-42/log")}
	in.Card.Metadata = map[string]any{"run_outcome": "failed"}
	if d := Evaluate(in); d.Blocked != "B1" {
		t.Fatalf("expected B1, got %+v", d)
	}
}

func TestB2_ChatOnlyBlocks(t *testing.T) {
	in := baseInput()
	in.Sources = []memory.SourceRef{gitRef("slack://C123/p456"), gitRef("slack://C999/p111")}
	d := Evaluate(in)
	if d.Blocked != "B2" {
		t.Fatalf("expected B2, got %+v", d)
	}
	// Config can open the gate (§4.4 chat_only_can_promote) — then P4 applies?
	// chat is authority 7 > 4, so P4 still shouldn't fire; stays proposed.
	in.ChatOnlyCanPromote = true
	d = Evaluate(in)
	if d.Promote || d.Blocked != "" {
		t.Fatalf("chat-only with override should stay proposed without blocking, got %+v", d)
	}
}

func TestB3_ContradictsBranchVerified(t *testing.T) {
	in := baseInput()
	in.Card.Metadata = map[string]any{"contradicts_branch_verified": true}
	if d := Evaluate(in); d.Blocked != "B3" {
		t.Fatalf("expected B3, got %+v", d)
	}
}

func TestP1_HumanApproval(t *testing.T) {
	in := baseInput()
	in.HumanApproved = true
	d := Evaluate(in)
	if !d.Promote || d.Rule != "P1" || d.Confidence != 0.95 {
		t.Fatalf("expected P1@0.95, got %+v", d)
	}
}

func TestP2_RuntimeValidation(t *testing.T) {
	in := baseInput()
	in.Verifications = []memory.VerificationResult{{
		Type: memory.VerifyCommandSucceeded, Result: memory.VerifyPassed,
		VerifiedAt: now.Add(-24 * time.Hour),
	}}
	d := Evaluate(in)
	if !d.Promote || d.Rule != "P2" || d.Confidence != 0.90 {
		t.Fatalf("expected P2@0.90, got %+v", d)
	}
	// Older than 7 days does not count.
	in.Verifications[0].VerifiedAt = now.Add(-8 * 24 * time.Hour)
	if d := Evaluate(in); d.Promote {
		t.Fatalf("stale runtime validation must not promote, got %+v", d)
	}
}

func TestP2_AllowedForFailedRunReflections(t *testing.T) {
	// §10.5: failed-run reflections CAN be promoted by later runtime validation.
	in := baseInput()
	in.Sources = []memory.SourceRef{gitRef("agentrun://run-42/log"), gitRef("git://x/y.go")}
	in.Card.Metadata = map[string]any{"run_outcome": "failed"}
	in.Verifications = []memory.VerificationResult{{
		Type: memory.VerifyTestPassed, Result: memory.VerifyPassed, VerifiedAt: now,
	}}
	d := Evaluate(in)
	if !d.Promote || d.Rule != "P2" {
		t.Fatalf("expected P2 for runtime-validated failed-run lesson, got %+v", d)
	}
}

func TestP3_BranchVerification(t *testing.T) {
	in := baseInput()
	in.Verifications = []memory.VerificationResult{{
		Type: memory.VerifyCodeBranchCheck, Result: memory.VerifyPassed, VerifiedAt: now,
	}}
	d := Evaluate(in)
	if !d.Promote || d.Rule != "P3" || d.Confidence != 0.85 {
		t.Fatalf("expected P3@0.85, got %+v", d)
	}
}

func TestP4_TwoIndependentSources(t *testing.T) {
	in := baseInput()
	in.Sources = []memory.SourceRef{
		gitRef("git://billing-api/src/a.go"),
		gitRef("doc://runbooks/billing.md"),
	}
	d := Evaluate(in)
	if !d.Promote || d.Rule != "P4" || d.Confidence != 0.75 {
		t.Fatalf("expected P4@0.75, got %+v", d)
	}
	// Same root twice is NOT independent.
	in.Sources = []memory.SourceRef{
		gitRef("git://billing-api/src/a.go"),
		gitRef("git://billing-api/src/b.go"),
	}
	if d := Evaluate(in); d.Promote {
		t.Fatalf("same-root sources must not satisfy P4, got %+v", d)
	}
}

func TestP4_ExcludedForFailedRuns(t *testing.T) {
	in := baseInput()
	in.Sources = []memory.SourceRef{
		gitRef("git://billing-api/src/a.go"),
		gitRef("doc://runbooks/billing.md"),
	}
	in.Card.Metadata = map[string]any{"run_outcome": "failed"}
	d := Evaluate(in)
	if d.Promote {
		t.Fatalf("failed-run reflections are excluded from P4 (§10.5), got %+v", d)
	}
}

func TestP5_TrustedSource(t *testing.T) {
	in := baseInput()
	in.Sources = []memory.SourceRef{gitRef("incident://INC-42")}
	in.NamespacePolicy = memory.NamespacePolicy{TrustedSources: []string{"incident"}}
	d := Evaluate(in)
	if !d.Promote || d.Rule != "P5" || d.Confidence != 0.70 {
		t.Fatalf("expected P5@0.70, got %+v", d)
	}
}

func TestAncestorNamespaceRequiresP1(t *testing.T) {
	in := baseInput()
	in.AncestorPromotion = true
	in.Sources = []memory.SourceRef{
		gitRef("git://billing-api/src/a.go"),
		gitRef("doc://runbooks/billing.md"),
	}
	if d := Evaluate(in); d.Promote {
		t.Fatalf("org-wide promotion without P1 must not promote, got %+v", d)
	}
	in.HumanApproved = true
	if d := Evaluate(in); !d.Promote || d.Rule != "P1" {
		t.Fatalf("org-wide with approval should promote via P1, got %+v", d)
	}
}

func TestNoRuleStaysProposedWithHint(t *testing.T) {
	d := Evaluate(baseInput()) // one git source, no verification, no review
	if d.Promote || d.Blocked != "" || d.Hint == "" {
		t.Fatalf("expected stay-proposed with hint, got %+v", d)
	}
}
