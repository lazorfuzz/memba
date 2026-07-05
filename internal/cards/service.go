package cards

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/lazorfuzz/memba/internal/authz"
	"github.com/lazorfuzz/memba/internal/config"
	"github.com/lazorfuzz/memba/internal/embed"
	"github.com/lazorfuzz/memba/internal/store"
	"github.com/lazorfuzz/memba/pkg/memory"
)

// Service implements propose / review / invalidate / promotion evaluation /
// decay sweeps over the store.
type Service struct {
	Store    store.Store
	Embedder embed.Embedder
	Cfg      config.Config
}

// structuredSchemas: minimal required-field validation per card_type
// (spec §6.4.1 — full JSON Schema validation can replace this check without
// changing callers).
var structuredRequired = map[string][]string{
	memory.CardProcedure: {"steps"},
	memory.CardGotcha:    {"trigger", "consequence"},
	memory.CardOwnership: {"owner"},
	memory.CardSkill:     {"applies_when", "recipe"},
}

func validCardType(t string) bool {
	for _, ct := range memory.CardTypes {
		if t == ct {
			return true
		}
	}
	return false
}

// ValidateProposal enforces schema + citation requirements (422 on failure).
func ValidateProposal(req memory.ProposalRequest) error {
	if !validCardType(req.CardType) {
		return fmt.Errorf("unknown card_type %q", req.CardType)
	}
	if strings.TrimSpace(req.Title) == "" || strings.TrimSpace(req.Body) == "" {
		return fmt.Errorf("title and body are required")
	}
	if len(req.SourceRefs) == 0 {
		return fmt.Errorf("source_refs required: a memory without citations cannot leave proposed (I2/B4)")
	}
	for _, s := range req.SourceRefs {
		if strings.TrimSpace(s.SourceURI) == "" {
			return fmt.Errorf("every source_ref needs a source_uri")
		}
	}
	if reqd, ok := structuredRequired[req.CardType]; ok && req.Structured != nil {
		for _, k := range reqd {
			if _, present := req.Structured[k]; !present {
				return fmt.Errorf("structured payload for %s requires field %q (§6.4.1)", req.CardType, k)
			}
		}
	}
	return nil
}

// Propose stores a proposal (status=proposed), applying dedup-at-propose
// (spec §10.8) and computing the derived ACL from cited raw evidence
// (spec §15.2), then immediately evaluates promotion (§10.1).
func (s *Service) Propose(ctx context.Context, p memory.Principal, req memory.ProposalRequest) (memory.ProposalResponse, error) {
	if err := ValidateProposal(req); err != nil {
		return memory.ProposalResponse{}, err
	}
	ttlClass := req.TTLClass
	if ttlClass == "" {
		ttlClass = memory.TTLCode
	}

	// Derived ACL = intersection of cited raw-evidence read sets; citations
	// without stored evidence contribute the proposer's tenant scope.
	var sourceACLs []memory.ACL
	for _, ref := range req.SourceRefs {
		if ref.RawID == "" {
			continue
		}
		ev, err := s.Store.GetEvidence(ctx, p.TenantID, ref.RawID)
		if err == nil {
			sourceACLs = append(sourceACLs, ev.ACL)
		}
	}
	var acl memory.ACL
	if len(sourceACLs) > 0 {
		acl = authz.DeriveACL(sourceACLs)
	} else {
		acl = memory.ACL{Read: []string{"tenant:" + p.TenantID}}
	}

	// Embed for dedup + retrieval.
	var vecLit string
	var vec []float32
	if s.Embedder != nil {
		if vecs, err := s.Embedder.Embed(ctx, []string{req.Title + "\n" + req.Body}); err == nil && len(vecs) == 1 {
			vec = vecs[0]
			vecLit = embed.VectorLiteral(vec)
		}
	}

	meta := map[string]any{}
	for k, v := range req.Metadata {
		meta[k] = v
	}
	if req.RunID != "" {
		meta["run_id"] = req.RunID
	}

	card := memory.Card{
		TenantID:    p.TenantID,
		NamespaceID: req.NamespaceID,
		CardType:    req.CardType,
		Title:       req.Title,
		Body:        req.Body,
		Structured:  req.Structured,
		Subject:     req.Subject,
		Tags:        req.Tags,
		Status:      memory.StatusProposed,
		Confidence:  0.5,
		Importance:  0.5,
		TTLClass:    ttlClass,
		CreatedFrom: createdFrom(p),
		CreatedBy:   p.ID,
		ACL:         acl,
		Metadata:    meta,
		Sources:     req.SourceRefs,
	}

	// Dedup-at-propose (spec §10.8).
	var dupOf string
	if vecLit != "" && req.Subject != "" {
		near, sim, found, err := s.Store.NearestActiveCard(ctx, p.TenantID, req.NamespaceID, req.Subject, vecLit)
		if err == nil && found && sim >= s.Cfg.Lifecycle.DedupCosine {
			if equivalentBodies(near.Body, req.Body) {
				// (a) equivalent: the write becomes a new supporting citation.
				for _, ref := range req.SourceRefs {
					_ = s.Store.AddCardSource(ctx, near.ID, ref)
				}
				return memory.ProposalResponse{
					CardID: near.ID, Status: near.Status, DuplicateOf: near.ID,
					PromotionHint: "existing card gained your citations (dedup §10.8a)",
				}, nil
			}
			// (b) near-duplicate: linked proposal, flagged for consolidation.
			dupOf = near.ID
			meta["near_duplicate_of"] = near.ID
		}
	}

	id, err := s.Store.InsertCard(ctx, card, vecLit)
	if err != nil {
		return memory.ProposalResponse{}, err
	}
	if dupOf != "" {
		_ = s.Store.InsertCardLink(ctx, id, dupOf, "related_to", "dedup", 1.0)
	}

	// Immediate promotion evaluation (spec §10.1 last step).
	decision, err := s.EvaluatePromotion(ctx, p.TenantID, id)
	status := memory.StatusProposed
	hint := decision.Hint
	if err == nil && decision.Promote {
		if err := s.applyPromotion(ctx, p.TenantID, id, decision); err == nil {
			status = memory.StatusActive
			hint = "promoted via " + decision.Rule
		}
	} else if decision.Blocked != "" {
		hint = decision.Blocked + ": " + decision.Hint
	}
	return memory.ProposalResponse{CardID: id, Status: status, PromotionHint: hint, DuplicateOf: dupOf}, nil
}

func createdFrom(p memory.Principal) string {
	switch {
	case strings.HasPrefix(p.ID, "agent:"):
		return memory.FromAgentRun
	case strings.HasPrefix(p.ID, "svc:"):
		return memory.FromExtractor
	default:
		return memory.FromHuman
	}
}

var wsRe = regexp.MustCompile(`\s+`)

func equivalentBodies(a, b string) bool {
	na := strings.ToLower(wsRe.ReplaceAllString(strings.TrimSpace(a), " "))
	nb := strings.ToLower(wsRe.ReplaceAllString(strings.TrimSpace(b), " "))
	return na == nb
}

// EvaluatePromotion gathers evidence and applies §10.6.
func (s *Service) EvaluatePromotion(ctx context.Context, tenantID, cardID string) (PromotionDecision, error) {
	card, err := s.Store.GetCard(ctx, tenantID, cardID)
	if err != nil {
		return PromotionDecision{}, err
	}
	if card.Status != memory.StatusProposed {
		return PromotionDecision{Hint: "card is not in proposed status"}, nil
	}
	approved, err := s.Store.LatestApproval(ctx, cardID)
	if err != nil {
		return PromotionDecision{}, err
	}
	var verifs []memory.VerificationResult
	if v, ok, err := s.Store.LatestVerification(ctx, cardID); err == nil && ok {
		verifs = append(verifs, v)
	}
	var policy memory.NamespacePolicy
	if ns, err := s.Store.GetNamespace(ctx, tenantID, card.NamespaceID); err == nil {
		policy = ns.Policy
	}
	// Ancestor promotion: the card sits in an org/team namespace but cites
	// repo-scoped evidence from a descendant scope.
	ancestor := false
	if ns, err := s.Store.GetNamespace(ctx, tenantID, card.NamespaceID); err == nil {
		ancestor = ns.Kind == "org" || ns.Kind == "team"
	}
	return Evaluate(PromotionInput{
		Card:               card,
		Sources:            card.Sources,
		HumanApproved:      approved,
		Verifications:      verifs,
		NamespacePolicy:    policy,
		AncestorPromotion:  ancestor,
		MinIndependent:     s.Cfg.Promotion.MinSourcesForMultiSourceRule,
		ChatOnlyCanPromote: s.Cfg.Promotion.ChatOnlyCanPromote,
		Now:                time.Now().UTC(),
	}), nil
}

func (s *Service) applyPromotion(ctx context.Context, tenantID, cardID string, d PromotionDecision) error {
	if err := s.Store.PromoteCard(ctx, tenantID, cardID, d.Confidence); err != nil {
		return err
	}
	// Start the verification clock (I6): promotion counts as the initial
	// verification event for TTL purposes.
	card, err := s.Store.GetCard(ctx, tenantID, cardID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	return s.Store.SetCardVerified(ctx, tenantID, cardID, now, now.Add(s.Cfg.TTL(card.TTLClass)), d.Confidence)
}

// Review records a human decision and applies its effect (spec §6.8, §13.1).
func (s *Service) Review(ctx context.Context, p memory.Principal, cardID string, req memory.ReviewRequest) (memory.ReviewResponse, error) {
	card, err := s.Store.GetCard(ctx, p.TenantID, cardID)
	if err != nil {
		return memory.ReviewResponse{}, err
	}
	if err := s.Store.InsertReview(ctx, cardID, p.ID, req.Decision, req.Reason, req.EditedBody); err != nil {
		return memory.ReviewResponse{}, err
	}
	switch req.Decision {
	case "approve":
		d := PromotionDecision{Promote: true, Rule: "P1", Confidence: 0.95}
		if card.Status == memory.StatusProposed {
			if err := s.applyPromotion(ctx, p.TenantID, cardID, d); err != nil {
				return memory.ReviewResponse{}, err
			}
		}
		return memory.ReviewResponse{CardID: cardID, Status: memory.StatusActive}, nil
	case "reject":
		err = s.Store.UpdateCardStatus(ctx, p.TenantID, cardID, memory.StatusDeprecated, "rejected by "+p.ID+": "+req.Reason)
		return memory.ReviewResponse{CardID: cardID, Status: memory.StatusDeprecated}, err
	case "invalidate":
		err = s.Store.UpdateCardStatus(ctx, p.TenantID, cardID, memory.StatusInvalidated, "invalidated by "+p.ID+": "+req.Reason)
		return memory.ReviewResponse{CardID: cardID, Status: memory.StatusInvalidated}, err
	case "edit":
		// Edits keep lifecycle simple: record the review row (audit) and leave
		// application of edited_body to a human-driven re-proposal.
		return memory.ReviewResponse{CardID: cardID, Status: card.Status}, nil
	default:
		return memory.ReviewResponse{}, fmt.Errorf("unknown decision %q", req.Decision)
	}
}

// Invalidate is mem.invalidate: counter-evidence logged, re-verification
// queued; it does NOT flip status by itself (spec §7.1).
func (s *Service) Invalidate(ctx context.Context, p memory.Principal, cardID string, req memory.InvalidateRequest) error {
	card, err := s.Store.GetCard(ctx, p.TenantID, cardID)
	if err != nil {
		return err
	}
	if err := s.Store.InsertReview(ctx, cardID, p.ID, "invalidate", req.Reason, ""); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"card_id": cardID, "namespace_id": card.NamespaceID, "reason": req.Reason})
	return s.Store.EnqueueJob(ctx, p.TenantID, "verify_sweep", json.RawMessage(payload))
}

// ---------------------------------------------------------------------------
// Sweeps (spec §10.7): verify_sweep and decay_sweep bodies, invoked by the
// jobs worker.
// ---------------------------------------------------------------------------

// SourceChecker re-runs the cheapest applicable check for a card.
type SourceChecker interface {
	CheapCheck(ctx context.Context, tenantID string, card memory.Card) (memory.VerificationResult, error)
}

// VerifySweep re-verifies cards near/past verify_by; pass → clock reset,
// fail → stale.
func (s *Service) VerifySweep(ctx context.Context, tenantID string, checker SourceChecker, limit int) (int, error) {
	due, err := s.Store.CardsVerifyDue(ctx, tenantID, time.Now().UTC(), limit)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, card := range due {
		vr, err := checker.CheapCheck(ctx, tenantID, card)
		if err != nil {
			continue
		}
		vr.TargetType, vr.TargetID = "card", card.ID
		_, _ = s.Store.InsertVerification(ctx, tenantID, card.NamespaceID, vr)
		now := time.Now().UTC()
		switch vr.Result {
		case memory.VerifyPassed:
			_ = s.Store.SetCardVerified(ctx, tenantID, card.ID, now, now.Add(s.Cfg.TTL(card.TTLClass)), card.Confidence)
		case memory.VerifyFailed:
			_ = s.Store.UpdateCardStatus(ctx, tenantID, card.ID, memory.StatusStale, "verify_sweep failed: "+summary(vr))
		default:
			// inconclusive past the clock → stale (nothing stays trusted
			// forever by default, I6)
			_ = s.Store.UpdateCardStatus(ctx, tenantID, card.ID, memory.StatusStale, "verify_by expired; re-check inconclusive")
		}
		n++
	}
	return n, nil
}

// DecaySweep moves stale/unretrieved cards past dormantMultiple×TTL to
// dormant (excluded everywhere, retained in DB).
func (s *Service) DecaySweep(ctx context.Context, tenantID string, limit int) (int, error) {
	due, err := s.Store.CardsDecayDue(ctx, tenantID, s.Cfg.Lifecycle.DormantAfterTTLMultiple, limit)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, card := range due {
		if err := s.Store.UpdateCardStatus(ctx, tenantID, card.ID, memory.StatusDormant, "decay_sweep: 2×TTL with no retrieval and no verification"); err == nil {
			n++
		}
	}
	return n, nil
}

func summary(v memory.VerificationResult) string {
	for _, r := range v.PerRef {
		if r.Result == memory.VerifyFailed {
			return r.Check + " failed for " + r.SourceURI
		}
	}
	return v.Result
}
