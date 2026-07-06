// Package pack assembles EvidencePacks from gated candidates under mode
// budgets (spec §8.2 steps 8–9, §8.6, §8.8): greedy by rerank score with
// per-section floors (gotcha floor, conflicts always), missing-evidence
// generation, and answerability scoring.
package pack

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lazorfuzz/memba/internal/authz"
	"github.com/lazorfuzz/memba/internal/gates"
	"github.com/lazorfuzz/memba/internal/retrieve"
	"github.com/lazorfuzz/memba/internal/store"
	"github.com/lazorfuzz/memba/internal/tokens"
	"github.com/lazorfuzz/memba/pkg/memory"
)

// Assembler builds evidence packs.
type Assembler struct {
	Store store.Store
}

// Input is everything needed to assemble one pack.
type Input struct {
	QueryID    string
	Mode       string
	Budget     int
	ConfigHash string
	Principal  memory.Principal
	Analyzed   retrieve.AnalyzedQuery
	Gated      gates.Gated
	Degraded   []string
	Namespaces []string
	Now        time.Time
}

// Assemble builds the pack. Cards are re-fetched in full (citations
// included) and their ACLs re-asserted — gate G1 defense in depth.
func (a *Assembler) Assemble(ctx context.Context, in Input) (memory.EvidencePack, error) {
	p := memory.EvidencePack{
		QueryID:         in.QueryID,
		Mode:            in.Mode,
		ConfigHash:      in.ConfigHash,
		Cards:           []memory.PackCard{},
		Facts:           []memory.PackFact{},
		Superseded:      []memory.PackFact{},
		Conflicts:       in.Gated.Conflicts,
		StaleFlagged:    []memory.StaleFlagged{},
		RawSpans:        []memory.RawSpan{},
		MissingEvidence: []string{},
		Degraded:        in.Degraded,
	}
	if p.Conflicts == nil {
		p.Conflicts = []memory.ConflictGroup{}
	}
	budget := in.Budget
	spend := func(n int) bool {
		if p.TokenTotal+n > budget {
			return false
		}
		p.TokenTotal += n
		return true
	}

	// Conflicts are always included (floor, §8.2 step 8).
	for _, cg := range p.Conflicts {
		p.TokenTotal += tokens.Estimate(fmt.Sprintf("%v", cg))
	}

	// Profile excerpt floor.
	if excerpt := a.profileExcerpt(ctx, in); excerpt != "" {
		if spend(tokens.Estimate(excerpt)) {
			p.ProfileExcerpt = excerpt
		}
	}

	// Split kept candidates by kind, preserving rerank order.
	var cardCands, factCands, chunkCands []store.Candidate
	for _, c := range in.Gated.Kept {
		switch c.Kind {
		case "card":
			cardCands = append(cardCands, c)
		case "fact":
			factCands = append(factCands, c)
		default:
			chunkCands = append(chunkCands, c)
		}
	}

	// Gotcha floor: every gotcha whose structured.trigger matches the query's
	// touched paths/terms is force-included first (spec §8.2 step 8).
	sort.SliceStable(cardCands, func(i, j int) bool {
		gi := isTriggeredGotcha(cardCands[i], in.Analyzed)
		gj := isTriggeredGotcha(cardCands[j], in.Analyzed)
		if gi != gj {
			return gi
		}
		return false // otherwise keep rerank order
	})

	seenCards := map[string]struct{}{}
	for _, c := range cardCands {
		if _, dup := seenCards[c.ID]; dup {
			continue
		}
		seenCards[c.ID] = struct{}{}
		full, err := a.Store.GetCard(ctx, in.Principal.TenantID, c.ID)
		if err != nil {
			continue
		}
		// G1 re-assert (defense in depth): drop on ACL mismatch.
		if !authz.Allowed(in.Principal, full.ACL) {
			continue
		}
		pc := toPackCard(full, in.Gated.Verified[key(c)])
		cost := tokens.Estimate(pc.Title + pc.Body)
		forced := isTriggeredGotcha(c, in.Analyzed)
		if !spend(cost) && !forced {
			continue
		}
		if forced {
			p.TokenTotal += max(0, cost) // floor items count against budget but are never dropped
		}
		p.Cards = append(p.Cards, pc)
	}

	for _, c := range factCands {
		full, err := a.Store.FactByID(ctx, in.Principal.TenantID, c.ID)
		if err != nil {
			continue
		}
		if !authz.Allowed(in.Principal, full.ACL) {
			continue
		}
		pf := toPackFact(full, in.Gated.Verified[key(c)])
		if !spend(tokens.Estimate(pf.Subject + pf.Predicate + pf.Object)) {
			continue
		}
		p.Facts = append(p.Facts, pf)
	}

	for _, c := range in.Gated.Superseded {
		full, err := a.Store.FactByID(ctx, in.Principal.TenantID, c.ID)
		if err != nil {
			continue
		}
		if !authz.Allowed(in.Principal, full.ACL) {
			continue
		}
		pf := toPackFact(full, false)
		if !spend(tokens.Estimate(pf.Subject + pf.Predicate + pf.Object)) {
			continue
		}
		p.Superseded = append(p.Superseded, pf)
	}

	// Stale-flagged: served flagged, never silently dropped (spec §9.2).
	for _, s := range in.Gated.StaleFlagged {
		sf := memory.StaleFlagged{Detail: s.Detail}
		if s.Candidate.Kind == "card" {
			full, err := a.Store.GetCard(ctx, in.Principal.TenantID, s.Candidate.ID)
			if err != nil || !authz.Allowed(in.Principal, full.ACL) {
				continue
			}
			pc := toPackCard(full, false)
			sf.Card = &pc
		}
		if s.Verification.VerificationID != "" || s.Verification.Result != "" {
			v := s.Verification
			sf.Verification = &v
		}
		if !spend(tokens.Estimate(sf.Detail)) {
			continue
		}
		p.StaleFlagged = append(p.StaleFlagged, sf)
	}

	// Raw spans: remainder of budget, rerank order (already authority-mixed
	// by fusion; G5 has capped per-source counts).
	for _, c := range chunkCands {
		span := memory.RawSpan{
			RawID:     c.RawID,
			ChunkID:   c.ID,
			SourceURI: c.SourceURI,
			Authority: memory.SourceAuthority(c.SourceType),
			Path:      c.Path,
			EventTime: c.EventTime,
			Excerpt:   c.Body,
		}
		if c.LineStart > 0 {
			span.Lines = fmt.Sprintf("%d-%d", c.LineStart, c.LineEnd)
		}
		if !spend(tokens.Estimate(span.Excerpt)) {
			// Try a truncated excerpt before giving up on the section.
			remaining := budget - p.TokenTotal
			if remaining > 120 {
				span.Excerpt = truncateTokens(span.Excerpt, remaining-20)
				if spend(tokens.Estimate(span.Excerpt)) {
					p.RawSpans = append(p.RawSpans, span)
				}
			}
			break
		}
		p.RawSpans = append(p.RawSpans, span)
	}

	coverage, uncovered := termCoverage(in.Analyzed.Terms, packHaystack(p))
	p.MissingEvidence = missingEvidence(in, p)
	// Coverage gap → explicit missing-evidence note (§8.2 step 9): the pack
	// matched *something*, but not the facets the question is about.
	if len(in.Analyzed.Terms) >= 2 && coverage < 0.6 && len(uncovered) >= 2 {
		p.MissingEvidence = append(p.MissingEvidence,
			"institutional memory does not mention: "+strings.Join(uncovered, ", "))
	}
	p.Answerability = answerability(in, p, coverage)
	return p, nil
}

// packHaystack concatenates the served content for coverage checks.
func packHaystack(p memory.EvidencePack) string {
	b := &strings.Builder{}
	b.WriteString(p.ProfileExcerpt)
	for _, c := range p.Cards {
		b.WriteString(" " + c.Title + " " + c.Body)
	}
	for _, f := range p.Facts {
		b.WriteString(" " + f.Subject + " " + f.Predicate + " " + f.Object)
	}
	for _, s := range p.RawSpans {
		b.WriteString(" " + s.Path + " " + s.Excerpt)
	}
	return strings.ToLower(b.String())
}

// termCoverage measures what fraction of the query's content terms the pack
// actually mentions (light stemming so "tests" matches "test").
func termCoverage(terms []string, hay string) (float64, []string) {
	if len(terms) == 0 {
		return 1, nil
	}
	var uncovered []string
	covered := 0
	for _, t := range terms {
		if strings.Contains(hay, stem(strings.ToLower(t))) {
			covered++
		} else {
			uncovered = append(uncovered, t)
		}
	}
	return float64(covered) / float64(len(terms)), uncovered
}

func stem(t string) string {
	for _, suffix := range []string{"ing", "es", "ed", "s"} {
		if strings.HasSuffix(t, suffix) && len(t)-len(suffix) >= 4 {
			return t[:len(t)-len(suffix)]
		}
	}
	return t
}

func key(c store.Candidate) string { return c.Kind + ":" + c.ID }

func toPackCard(c memory.Card, verified bool) memory.PackCard {
	return memory.PackCard{
		ID: c.ID, CardType: c.CardType, Title: c.Title, Body: c.Body,
		Structured: c.Structured, Status: c.Status, Confidence: c.Confidence,
		Verified: verified, LastVerifiedAt: c.LastVerifiedAt, Sources: c.Sources,
	}
}

func toPackFact(f memory.Fact, verified bool) memory.PackFact {
	return memory.PackFact{
		ID: f.ID, Subject: f.Subject, Predicate: f.Predicate, Object: f.Object,
		ValidFrom: f.ValidFrom, ValidTo: f.ValidTo, Verified: verified, Sources: f.Sources,
	}
}

// isTriggeredGotcha reports whether a gotcha card's structured.trigger
// matches the query's identifiers/paths (spec §8.2 step 8 floor).
func isTriggeredGotcha(c store.Candidate, aq retrieve.AnalyzedQuery) bool {
	if c.CardType != memory.CardGotcha {
		return false
	}
	trigger, _ := c.Structured["trigger"].(string)
	if trigger == "" {
		return false
	}
	pat := strings.ToLower(trigger)
	// Normalize glob-ish triggers ("editing files matching *_pb.go") to key fragments.
	frags := strings.FieldsFunc(pat, func(r rune) bool {
		return r == ' ' || r == '*' || r == '"' || r == '\''
	})
	hay := strings.ToLower(aq.Raw + " " + strings.Join(aq.Identifiers, " "))
	for _, f := range frags {
		if len(f) < 4 || !strings.ContainsAny(f, "._/-") {
			continue
		}
		if strings.Contains(hay, f) {
			return true
		}
	}
	return false
}

// missingEvidence writes explicit gaps: query facets with zero post-gate
// results plus G3-failure exclusions (spec §8.2 step 9).
func missingEvidence(in Input, p memory.EvidencePack) []string {
	out := []string{}
	if len(p.Cards) == 0 && len(p.Facts) == 0 && len(p.RawSpans) == 0 {
		out = append(out, "no institutional memory matched this query in scope "+strings.Join(in.Namespaces, ", "))
	}
	if len(in.Analyzed.Symbols) > 0 {
		found := false
		for _, s := range p.RawSpans {
			for _, sym := range in.Analyzed.Symbols {
				if strings.Contains(s.Excerpt, sym) || strings.Contains(s.Path, sym) {
					found = true
					break
				}
			}
		}
		if !found && len(p.RawSpans) > 0 {
			out = append(out, "no code evidence found mentioning: "+strings.Join(in.Analyzed.Symbols, ", "))
		}
	}
	if in.Analyzed.Temporal && len(p.Facts) == 0 && len(p.Superseded) == 0 {
		out = append(out, "temporal question, but no dated facts found — answer may not reflect historical state")
	}
	out = append(out, in.Gated.G3Failures...)
	return out
}

// answerability computes high|partial|low from top rerank score, verified
// item count, unresolved conflicts, and query-term coverage (spec §8.6):
// matching the entity name alone is not answering the question.
func answerability(in Input, p memory.EvidencePack, coverage float64) string {
	var top float64
	if len(in.Gated.Kept) > 0 {
		top = in.Gated.Kept[0].Score
	}
	verified := 0
	for _, c := range p.Cards {
		if c.Verified {
			verified++
		}
	}
	for _, f := range p.Facts {
		if f.Verified {
			verified++
		}
	}
	unresolved := false
	for _, cg := range p.Conflicts {
		if cg.Unresolved {
			unresolved = true
		}
	}
	itemCount := len(p.Cards) + len(p.Facts) + len(p.RawSpans)
	switch {
	case itemCount == 0:
		return "low"
	case len(in.Analyzed.Terms) >= 2 && coverage < 0.35 && verified == 0:
		return "low"
	case unresolved || coverage < 0.6:
		return "partial"
	case top >= 0.3 && (verified > 0 || itemCount >= 3):
		return "high"
	case itemCount >= 1:
		return "partial"
	default:
		return "low"
	}
}

// profileExcerpt renders a minimal deterministic L1 slice: the namespace's
// highest-importance active convention/ownership/environment cards with
// [card:id] markers (spec §5 L1; full C2 profile generation is a
// consolidation job).
func (a *Assembler) profileExcerpt(ctx context.Context, in Input) string {
	if len(in.Namespaces) == 0 {
		return ""
	}
	cards, err := a.Store.ListCards(ctx, in.Principal.TenantID, store.CardFilter{
		NamespaceID: in.Namespaces[0], Status: memory.StatusActive, Limit: 50,
	})
	if err != nil || len(cards) == 0 {
		return ""
	}
	var lines []string
	for _, c := range cards {
		switch c.CardType {
		case memory.CardConvention, memory.CardOwnership, memory.CardEnvironmentNote, memory.CardProcedure:
			if !authz.Allowed(in.Principal, c.ACL) {
				continue
			}
			lines = append(lines, fmt.Sprintf("- %s [card:%s]", c.Title, c.ID))
		}
		if len(lines) >= 5 {
			break
		}
	}
	if len(lines) == 0 {
		return ""
	}
	sort.Strings(lines) // byte-stable serialization (cache stability, §18.2)
	return "## " + in.Namespaces[0] + " — operating profile (excerpt)\n" + strings.Join(lines, "\n")
}

func truncateTokens(s string, maxTok int) string {
	maxChars := maxTok * 3
	if len(s) <= maxChars {
		return s
	}
	return s[:maxChars] + "\n…[truncated]"
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
