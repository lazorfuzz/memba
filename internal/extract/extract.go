package extract

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lazorfuzz/memba/internal/cards"
	"github.com/lazorfuzz/memba/internal/store"
	"github.com/lazorfuzz/memba/pkg/memory"
)

// Extractor runs the extract_cards job body.
type Extractor struct {
	Store         store.Store
	Cards         *cards.Service
	LLM           LLM
	SystemPrompt  string // versioned prompt text (configs/prompts, §17.7)
	UserPrompt    string
	MaxCandidates int // per-document cap (spec §10.4, default 8)
	DailyCap      int // extractor cards per namespace per day (cost bound)
}

// Result summarizes one extraction run.
type Result struct {
	Candidates       int `json:"candidates"`
	DiscardedNoQuote int `json:"discarded_no_quote"` // failed the citation gate (I2)
	CardsProposed    int `json:"cards_proposed"`
	CardsPromoted    int `json:"cards_promoted"`
	FactsProposed    int `json:"facts_proposed"`
	SkippedReason    string `json:"skipped_reason,omitempty"`
}

//go:generate true

// DefaultPrompts returns the embedded v1 prompt pair; LoadPrompts prefers
// on-disk versions so prompt evolution (§17.7) needs no rebuild.
func LoadPrompts(dir string) (system, user string) {
	system, user = defaultSystemPrompt, defaultUserPrompt
	if b, err := os.ReadFile(filepath.Join(dir, "extract_system_v1.md")); err == nil {
		system = string(b)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "extract_user_v1.md")); err == nil {
		user = string(b)
	}
	return system, user
}

// candidate mirrors the JSON contract in extract_system_v1.md.
type candidate struct {
	Kind       string         `json:"kind"`
	CardType   string         `json:"card_type"`
	Title      string         `json:"title"`
	Body       string         `json:"body"`
	Subject    string         `json:"subject"`
	TTLClass   string         `json:"ttl_class"`
	Structured map[string]any `json:"structured"`
	Fact       *struct {
		Subject   string `json:"subject"`
		Predicate string `json:"predicate"`
		Object    string `json:"object"`
	} `json:"fact"`
	Citations []struct {
		Quote string `json:"quote"`
	} `json:"citations"`
}

const evidenceBodyLimit = 24_000 // chars sent to the model per document

// ExtractFromEvidence runs §10.4 for one raw evidence row.
func (e *Extractor) ExtractFromEvidence(ctx context.Context, tenantID, rawID string) (Result, error) {
	var res Result
	if e.LLM == nil {
		res.SkippedReason = "no extractor model configured"
		return res, nil
	}
	ev, err := e.Store.GetEvidence(ctx, tenantID, rawID)
	if err != nil {
		return res, fmt.Errorf("load evidence: %w", err)
	}
	// Quarantined or benchmark-seed evidence is never extracted (§15.4 D2, §17.6).
	if ev.Quarantined {
		res.SkippedReason = "evidence quarantined"
		return res, nil
	}
	if seed, ok := ev.Metadata["benchmark_seed"].(bool); ok && seed {
		res.SkippedReason = "benchmark seed excluded from extraction"
		return res, nil
	}
	// Daily per-namespace cap (§10.4 cost bound).
	if e.DailyCap > 0 {
		n, err := e.Store.CountCardsCreatedSince(ctx, tenantID, ev.NamespaceID, memory.FromExtractor,
			time.Now().UTC().Add(-24*time.Hour))
		if err == nil && n >= e.DailyCap {
			res.SkippedReason = fmt.Sprintf("daily extraction cap reached (%d)", e.DailyCap)
			return res, nil
		}
	}

	maxCand := e.MaxCandidates
	if maxCand <= 0 {
		maxCand = 8
	}
	body := ev.Body
	if len(body) > evidenceBodyLimit {
		body = body[:evidenceBodyLimit] + "\n…[truncated]"
	}
	system := strings.ReplaceAll(e.SystemPrompt, "{{max_candidates}}", fmt.Sprint(maxCand))
	user := e.UserPrompt
	user = strings.ReplaceAll(user, "{{source_type}}", ev.SourceType)
	user = strings.ReplaceAll(user, "{{source_uri}}", ev.SourceURI)
	user = strings.ReplaceAll(user, "{{type_hint}}", typeHint(ev.SourceType))
	user = strings.ReplaceAll(user, "{{body}}", body)

	raw, err := e.LLM.Complete(ctx, system, user)
	if err != nil {
		return res, fmt.Errorf("extractor model: %w", err)
	}
	cands, err := ParseCandidates(raw)
	if err != nil {
		return res, fmt.Errorf("parse extractor output: %w", err)
	}
	if len(cands) > maxCand {
		cands = cands[:maxCand]
	}
	res.Candidates = len(cands)

	principal := memory.Principal{
		TenantID:    tenantID,
		ID:          "svc:extractor",
		ACLSubjects: []string{"svc:extractor", "tenant:" + tenantID},
	}
	for _, c := range cands {
		refs, ok := e.validateCitations(c, ev)
		if !ok {
			res.DiscardedNoQuote++ // uncitable → discarded, not stored (I2)
			continue
		}
		switch c.Kind {
		case "fact":
			if c.Fact == nil || c.Fact.Subject == "" || c.Fact.Predicate == "" || c.Fact.Object == "" {
				res.DiscardedNoQuote++
				continue
			}
			_, err := e.Store.InsertFact(ctx, memory.Fact{
				TenantID:    tenantID,
				NamespaceID: ev.NamespaceID,
				Subject:     c.Fact.Subject,
				Predicate:   c.Fact.Predicate,
				Object:      c.Fact.Object,
				Status:      memory.FactProposed,
				ACL:         ev.ACL,
				CreatedBy:   principal.ID,
				Sources:     refs,
			})
			if err == nil {
				res.FactsProposed++
			}
		default: // card
			if !validCardType(c.CardType) || c.Title == "" || c.Body == "" {
				res.DiscardedNoQuote++
				continue
			}
			resp, err := e.Cards.Propose(ctx, principal, memory.ProposalRequest{
				NamespaceID: ev.NamespaceID,
				CardType:    c.CardType,
				Title:       c.Title,
				Body:        c.Body,
				Structured:  c.Structured,
				Subject:     c.Subject,
				TTLClass:    ttlClassOr(c.TTLClass),
				SourceRefs:  refs,
				Metadata:    map[string]any{"extractor_model": e.LLM.ModelID()},
			})
			if err != nil {
				continue
			}
			res.CardsProposed++
			if resp.Status == memory.StatusActive {
				res.CardsPromoted++
			}
		}
	}
	return res, nil
}

// validateCitations enforces the citation gate: every kept candidate needs
// ≥1 quote found VERBATIM in the evidence body. Line ranges are computed
// from the quote's position so branch verification (§9.1C) can re-check it.
func (e *Extractor) validateCitations(c candidate, ev memory.RawEvidence) ([]memory.SourceRef, bool) {
	var refs []memory.SourceRef
	for _, cit := range c.Citations {
		quote := strings.TrimSpace(cit.Quote)
		if quote == "" {
			continue
		}
		idx := strings.Index(ev.Body, quote)
		if idx < 0 {
			continue // not verbatim → not a citation
		}
		lineStart := 1 + strings.Count(ev.Body[:idx], "\n")
		lineEnd := lineStart + strings.Count(quote, "\n")
		refs = append(refs, memory.SourceRef{
			RawID:         ev.ID,
			SourceURI:     ev.SourceURI,
			SourceVersion: ev.SourceVersion,
			LineStart:     lineStart,
			LineEnd:       lineEnd,
			Quote:         quote,
			SupportType:   "supports",
		})
	}
	return refs, len(refs) > 0
}

// ParseCandidates parses the model's JSON array, tolerating markdown fences
// and surrounding prose (the array is located, then strictly decoded).
func ParseCandidates(raw string) ([]candidate, error) {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "["); i >= 0 {
		if j := strings.LastIndex(s, "]"); j > i {
			s = s[i : j+1]
		}
	}
	var out []candidate
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func typeHint(sourceType string) string {
	switch sourceType {
	case memory.SourceGit:
		return "hint: this is source code or a repo file — look for build/test commands, generated-code markers (DO NOT EDIT), config conventions."
	case memory.SourceGithubPR:
		return "hint: this is a pull request — look for conventions enforced in review and decisions with lasting effect."
	case memory.SourceCI:
		return "hint: this is a CI log — look for reproducible failure causes and required checks, not transient flakes."
	case memory.SourceIncident:
		return "hint: this is an incident report — look for causes, mitigations, and prevention rules."
	case memory.SourceSlack:
		return "hint: this is chat — be extra conservative; only extract claims a maintainer states as fact."
	case memory.SourceDoc:
		return "hint: this is documentation — extract procedures, commands, and ownership; note the doc may be stale."
	default:
		return ""
	}
}

func validCardType(t string) bool {
	for _, ct := range memory.CardTypes {
		if t == ct {
			return true
		}
	}
	return false
}

func ttlClassOr(t string) string {
	switch t {
	case memory.TTLCode, memory.TTLOperational, memory.TTLDesign:
		return t
	default:
		return memory.TTLCode
	}
}
