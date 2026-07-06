// Package consolidate implements the sleep-time consolidation jobs C1–C7 of
// spec §11. Every output is a proposal or a link subject to the §10.6
// promotion gate — consolidation never silently rewrites active memory.
package consolidate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/lazorfuzz/memba/internal/cards"
	"github.com/lazorfuzz/memba/internal/config"
	"github.com/lazorfuzz/memba/internal/embed"
	"github.com/lazorfuzz/memba/internal/objstore"
	"github.com/lazorfuzz/memba/internal/store"
	"github.com/lazorfuzz/memba/pkg/memory"
)

// Consolidator runs consolidate_ns for one (tenant, namespace).
type Consolidator struct {
	Store store.Store
	Cards *cards.Service
	Blobs objstore.Store
	Cfg   config.Config
	// Checker runs C7's cheap re-verifications (usually *verify.Verifier).
	Checker cards.SourceChecker
}

// Stats are the per-job counters written to consolidation_runs (§11).
type Stats struct {
	C1MergedProposals int  `json:"c1_merged_proposals"`
	C1Promoted        int  `json:"c1_promoted"`
	C2ProfileUpdated  bool `json:"c2_profile_updated"`
	C3Conflicts       int  `json:"c3_conflicts"`
	C4LinksProposed   int  `json:"c4_links_proposed"`
	C5Gaps            int  `json:"c5_gaps"`
	C6GotchaProposals int  `json:"c6_gotcha_proposals"`
	C7Verified        int  `json:"c7_verified"`
	C7Dormant         int  `json:"c7_dormant"`
}

func (c *Consolidator) principal(tenantID string) memory.Principal {
	return memory.Principal{
		TenantID:    tenantID,
		ID:          "svc:consolidation",
		ACLSubjects: []string{"svc:consolidation", "tenant:" + tenantID},
	}
}

// Run executes C1–C7 and records the stats row. Individual job failures are
// recorded, not fatal — a broken sweep must not block the others.
func (c *Consolidator) Run(ctx context.Context, tenantID, namespaceID string) (Stats, error) {
	var s Stats
	var errs []string
	fail := func(job string, err error) {
		if err != nil {
			errs = append(errs, job+": "+err.Error())
		}
	}

	fail("C1", c.dedupMerge(ctx, tenantID, namespaceID, &s))
	fail("C2", c.buildProfile(ctx, tenantID, namespaceID, &s))
	fail("C3", c.contradictionSweep(ctx, tenantID, namespaceID, &s))
	fail("C4", c.linkInference(ctx, tenantID, namespaceID, &s))
	fail("C5", c.gapMining(ctx, tenantID, namespaceID, &s))
	fail("C6", c.failureMining(ctx, tenantID, namespaceID, &s))
	fail("C7", c.decay(ctx, tenantID, &s))

	statsMap := map[string]any{
		"c1_merged_proposals": s.C1MergedProposals, "c1_promoted": s.C1Promoted,
		"c2_profile_updated": s.C2ProfileUpdated, "c3_conflicts": s.C3Conflicts,
		"c4_links_proposed": s.C4LinksProposed, "c5_gaps": s.C5Gaps,
		"c6_gotcha_proposals": s.C6GotchaProposals,
		"c7_verified":         s.C7Verified, "c7_dormant": s.C7Dormant,
	}
	errStr := strings.Join(errs, "; ")
	_ = c.Store.InsertConsolidationRun(ctx, tenantID, namespaceID, statsMap, errStr)
	if errStr != "" {
		return s, fmt.Errorf("consolidate %s: %s", namespaceID, errStr)
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// C1 — dedup/merge: cluster near-duplicate active cards (cosine ≥ dedup
// threshold, same subject); emit ONE merged proposal citing the union of
// sources + supersedes links. Auto-promotable via P4 (§11).
// ---------------------------------------------------------------------------

func (c *Consolidator) dedupMerge(ctx context.Context, tenantID, namespaceID string, s *Stats) error {
	pairs, err := c.Store.NearDuplicateCardPairs(ctx, tenantID, namespaceID, c.Cfg.Lifecycle.DedupCosine, 20)
	if err != nil {
		return err
	}
	for _, pair := range pairs {
		// Skip pairs already handled by a prior night's merge.
		if done, _ := c.Store.CardHasSuperseder(ctx, pair.A.ID); done {
			continue
		}
		if done, _ := c.Store.CardHasSuperseder(ctx, pair.B.ID); done {
			continue
		}
		canonical, other := pair.A, pair.B
		if other.Confidence > canonical.Confidence {
			canonical, other = other, canonical
		}
		// The merged card is inserted directly (NOT via Propose): the merge
		// intentionally duplicates the canonical content, so §10.8
		// dedup-at-propose would fold it away instead of superseding.
		refs := unionRefs(canonical.Sources, other.Sources)
		merged := memory.Card{
			TenantID:    tenantID,
			NamespaceID: namespaceID,
			CardType:    canonical.CardType,
			Title:       canonical.Title,
			Body:        canonical.Body,
			Structured:  canonical.Structured,
			Subject:     canonical.Subject,
			Tags:        canonical.Tags,
			Status:      memory.StatusProposed,
			Confidence:  0.5,
			Importance:  maxf(canonical.Importance, other.Importance),
			TTLClass:    canonical.TTLClass,
			CreatedFrom: memory.FromConsolidation,
			CreatedBy:   "svc:consolidation",
			ACL:         canonical.ACL,
			Metadata: map[string]any{
				"merged_from": []string{canonical.ID, other.ID},
				"merge_sim":   pair.Score,
			},
			Sources: refs,
		}
		var vecLit string
		if c.Cards.Embedder != nil {
			if vecs, err := c.Cards.Embedder.Embed(ctx, []string{merged.Title + "\n" + merged.Body}); err == nil && len(vecs) == 1 {
				vecLit = embed.VectorLiteral(vecs[0])
			}
		}
		mergedID, err := c.Store.InsertCard(ctx, merged, vecLit)
		if err != nil {
			continue
		}
		s.C1MergedProposals++
		_ = c.Store.InsertCardLink(ctx, mergedID, canonical.ID, "supersedes", "consolidation", 1.0)
		_ = c.Store.InsertCardLink(ctx, mergedID, other.ID, "supersedes", "consolidation", 1.0)

		// Auto-promotable via P4: the union of independent sources usually
		// satisfies the multi-source rule (§11 C1).
		if d, err := c.Cards.PromoteIfEligible(ctx, tenantID, mergedID); err == nil && d.Promote {
			s.C1Promoted++
			// Superseded originals leave default recall but keep history (§6.4).
			_ = c.Store.UpdateCardStatus(ctx, tenantID, canonical.ID, memory.StatusDeprecated, "superseded by merged card "+mergedID+" (C1)")
			_ = c.Store.UpdateCardStatus(ctx, tenantID, other.ID, memory.StatusDeprecated, "superseded by merged card "+mergedID+" (C1)")
		}
	}
	return nil
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func unionRefs(a, b []memory.SourceRef) []memory.SourceRef {
	seen := map[string]struct{}{}
	var out []memory.SourceRef
	for _, r := range append(append([]memory.SourceRef{}, a...), b...) {
		key := fmt.Sprintf("%s|%d|%d", r.SourceURI, r.LineStart, r.LineEnd)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, r)
	}
	return out
}

// ---------------------------------------------------------------------------
// C2 — profile build: regenerate the L1 operating profile from active cards.
// Byte-stable serialization (sorted, templated) so prompt caches only bust
// on real change (§11, §18.2). Written to the object store; served by
// GET /v1/profile?level=1.
// ---------------------------------------------------------------------------

// ProfileKey is the object-store key for a namespace's L1 profile.
func ProfileKey(tenantID, namespaceID string) string {
	slug := strings.NewReplacer("/", "_", ":", "_").Replace(strings.TrimPrefix(namespaceID, "/"))
	return "profiles/" + tenantID + "/" + slug + ".md"
}

func (c *Consolidator) buildProfile(ctx context.Context, tenantID, namespaceID string, s *Stats) error {
	active, err := c.Store.ListCards(ctx, tenantID, store.CardFilter{
		NamespaceID: namespaceID, Status: memory.StatusActive, Limit: 500,
	})
	if err != nil {
		return err
	}
	profile := RenderProfile(namespaceID, active)

	key := ProfileKey(tenantID, namespaceID)
	if rc, err := c.Blobs.Get(ctx, key); err == nil {
		existing, _ := io.ReadAll(rc)
		rc.Close()
		if bytes.Equal(existing, []byte(profile)) {
			return nil // byte-stable: no cache bust
		}
	}
	if err := c.Blobs.Put(ctx, key, strings.NewReader(profile)); err != nil {
		return err
	}
	s.C2ProfileUpdated = true
	return nil
}

// RenderProfile deterministically renders L1 (§5): every line backed by an
// active card and tagged [card:<id>].
func RenderProfile(namespaceID string, active []memory.Card) string {
	sections := map[string][]string{}
	line := func(c memory.Card) string { return fmt.Sprintf("- %s [card:%s]", c.Title, c.ID) }
	var gotchas []memory.Card
	for _, card := range active {
		switch card.CardType {
		case memory.CardProcedure:
			sections["Commands & procedures"] = append(sections["Commands & procedures"], line(card))
		case memory.CardConvention, memory.CardDesignDecision:
			sections["Conventions & constraints"] = append(sections["Conventions & constraints"], line(card))
		case memory.CardOwnership:
			sections["Ownership"] = append(sections["Ownership"], line(card))
		case memory.CardEnvironmentNote:
			sections["Environment"] = append(sections["Environment"], line(card))
		case memory.CardGotcha, memory.CardWarning:
			gotchas = append(gotchas, card)
		}
	}
	// Top 5 gotchas by importance, then confidence, then title (deterministic).
	sort.SliceStable(gotchas, func(i, j int) bool {
		if gotchas[i].Importance != gotchas[j].Importance {
			return gotchas[i].Importance > gotchas[j].Importance
		}
		if gotchas[i].Confidence != gotchas[j].Confidence {
			return gotchas[i].Confidence > gotchas[j].Confidence
		}
		return gotchas[i].Title < gotchas[j].Title
	})
	if len(gotchas) > 5 {
		gotchas = gotchas[:5]
	}

	b := &strings.Builder{}
	fmt.Fprintf(b, "# %s — operating profile (L1)\n\n", namespaceID)
	b.WriteString("Generated by memba consolidation (C2). Every line cites an active, verification-clocked card; open with mem.open(card:<id>).\n")
	for _, title := range []string{"Commands & procedures", "Conventions & constraints", "Ownership", "Environment"} {
		lines := sections[title]
		if len(lines) == 0 {
			continue
		}
		sort.Strings(lines)
		fmt.Fprintf(b, "\n## %s\n", title)
		for _, l := range lines {
			b.WriteString(l + "\n")
		}
	}
	if len(gotchas) > 0 {
		b.WriteString("\n## Top gotchas\n")
		for i, g := range gotchas {
			trigger, _ := g.Structured["trigger"].(string)
			if trigger != "" {
				trigger = " (trigger: " + trigger + ")"
			}
			fmt.Fprintf(b, "%d. %s%s [card:%s]\n", i+1, g.Title, trigger, g.ID)
		}
	}
	if len(sections) == 0 && len(gotchas) == 0 {
		b.WriteString("\nNo active profile cards yet — run bootstrap (§20) or review the proposal queue.\n")
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// C3 — contradiction sweep: facts sharing fact_key with overlapping validity;
// emit conflict records + re-verification work for both sides (§11).
// ---------------------------------------------------------------------------

func (c *Consolidator) contradictionSweep(ctx context.Context, tenantID, namespaceID string, s *Stats) error {
	filter := store.ScopeFilter{
		TenantID:    tenantID,
		Namespaces:  []string{namespaceID},
		ACLSubjects: []string{"tenant:" + tenantID, "svc:consolidation"},
	}
	groups, err := c.Store.ActiveFactConflicts(ctx, filter)
	if err != nil {
		return err
	}
	s.C3Conflicts = len(groups)
	if len(groups) > 0 {
		// Re-verification job for the namespace: verify_sweep re-checks cards
		// near their clocks; conflicting facts lower answerability (G4) until
		// resolved by supersession or retraction.
		_ = c.Store.EnqueueJob(ctx, tenantID, "verify_sweep", map[string]any{
			"namespace_id": namespaceID, "reason": "fact_conflicts", "groups": len(groups),
		})
	}
	return nil
}

// ---------------------------------------------------------------------------
// C4 — link inference: propose related_to links from co-citation (shared raw
// sources) (§11). Links route search only — never truth (§6.5).
// ---------------------------------------------------------------------------

func (c *Consolidator) linkInference(ctx context.Context, tenantID, namespaceID string, s *Stats) error {
	pairs, err := c.Store.CoCitedCardPairs(ctx, tenantID, namespaceID, 2, 50)
	if err != nil {
		return err
	}
	for _, pair := range pairs {
		weight := pair.Score / (pair.Score + 2) // 2 shared → 0.5, asymptote 1.0
		if err := c.Store.InsertCardLink(ctx, pair.A.ID, pair.B.ID, "related_to", "consolidation", weight); err == nil {
			s.C4LinksProposed++
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// C5 — gap mining: cluster low-answerability queries from memory_actions;
// emit a human-facing "missing knowledge" report per namespace (§11).
// ---------------------------------------------------------------------------

// GapReportKey is the object-store key for a namespace's gap report.
func GapReportKey(tenantID, namespaceID string) string {
	slug := strings.NewReplacer("/", "_", ":", "_").Replace(strings.TrimPrefix(namespaceID, "/"))
	return "reports/" + tenantID + "/" + slug + "/missing_knowledge.md"
}

func (c *Consolidator) gapMining(ctx context.Context, tenantID, namespaceID string, s *Stats) error {
	gaps, err := c.Store.LowAnswerabilityQueries(ctx, tenantID, namespaceID, time.Now().UTC().AddDate(0, 0, -7), 50)
	if err != nil {
		return err
	}
	s.C5Gaps = len(gaps)
	if len(gaps) == 0 {
		return nil
	}
	b := &strings.Builder{}
	fmt.Fprintf(b, "# Missing knowledge — %s\n\n", namespaceID)
	b.WriteString("Queries from the last 7 days that institutional memory could not answer (answerability: low). Each is a documentation/bootstrap candidate.\n\n")
	fmt.Fprintf(b, "| asked | query |\n|---|---|\n")
	for _, g := range gaps {
		fmt.Fprintf(b, "| %d× | %s |\n", g.Count, strings.ReplaceAll(g.Query, "|", "\\|"))
	}
	return c.Blobs.Put(ctx, GapReportKey(tenantID, namespaceID), strings.NewReader(b.String()))
}

// ---------------------------------------------------------------------------
// C6 — failure mining: cluster mem.log entries from failed runs; when the
// same failure signature appears ≥ 3 times across runs, emit a gotcha
// PROPOSAL citing the run logs. Still gated: sources are authority 8, so
// promotion needs P1 (human) or P2 (runtime validation) (§11).
// ---------------------------------------------------------------------------

var failureLineRe = regexp.MustCompile(`(?im)^.*\b(error|fail(?:ed|ure)?|panic|fatal|exception|traceback)\b.*$`)
var numRe = regexp.MustCompile(`0x[0-9a-f]+|\d+`)
var hexRunRe = regexp.MustCompile(`\b[0-9a-f]{6,}\b`) // bare ids like 7f3a2b1c
var uuidRe = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// FailureSignature normalizes the first failure line of a run log so the
// "same" failure clusters across runs (numbers/ids masked).
func FailureSignature(body string) (signature, quote string) {
	m := failureLineRe.FindString(body)
	if m == "" {
		return "", ""
	}
	quote = strings.TrimSpace(m)
	sig := strings.ToLower(quote)
	sig = uuidRe.ReplaceAllString(sig, "<id>")
	sig = hexRunRe.ReplaceAllString(sig, "<id>")
	sig = numRe.ReplaceAllString(sig, "<n>")
	sig = strings.Join(strings.Fields(sig), " ")
	if len(sig) > 200 {
		sig = sig[:200]
	}
	return sig, quote
}

const failureThreshold = 3 // same signature ≥ 3 times across runs (§11 C6)

func (c *Consolidator) failureMining(ctx context.Context, tenantID, namespaceID string, s *Stats) error {
	runs, err := c.Store.FailedRunEvidence(ctx, tenantID, namespaceID, time.Now().UTC().AddDate(0, 0, -30), 500)
	if err != nil {
		return err
	}
	type clusterItem struct {
		ev    memory.RawEvidence
		quote string
	}
	clusters := map[string][]clusterItem{}
	for _, ev := range runs {
		sig, quote := FailureSignature(ev.Body)
		if sig == "" {
			continue
		}
		clusters[sig] = append(clusters[sig], clusterItem{ev, quote})
	}
	// Deterministic order for stable nightly behavior.
	sigs := make([]string, 0, len(clusters))
	for sig := range clusters {
		if len(clusters[sig]) >= failureThreshold {
			sigs = append(sigs, sig)
		}
	}
	sort.Strings(sigs)

	// Existing failure-mining gotchas (any status) — don't re-propose nightly.
	existing := map[string]struct{}{}
	if cardsList, err := c.Store.ListCards(ctx, tenantID, store.CardFilter{
		NamespaceID: namespaceID, CardType: memory.CardGotcha, Limit: 500,
	}); err == nil {
		for _, card := range cardsList {
			if sig, ok := card.Metadata["failure_signature"].(string); ok {
				existing[sig] = struct{}{}
			}
		}
	}

	for _, sig := range sigs {
		if _, dup := existing[sig]; dup {
			continue
		}
		items := clusters[sig]
		var refs []memory.SourceRef
		for i, it := range items {
			if i >= 5 {
				break
			}
			refs = append(refs, memory.SourceRef{
				RawID:     it.ev.ID,
				SourceURI: it.ev.SourceURI,
				Quote:     it.quote,
			})
		}
		title := "Recurring agent-run failure: " + truncate(items[0].quote, 90)
		resp, err := c.Cards.Propose(ctx, c.principal(tenantID), memory.ProposalRequest{
			NamespaceID: namespaceID,
			CardType:    memory.CardGotcha,
			Title:       title,
			Body: fmt.Sprintf(
				"The same failure appeared in %d agent runs over the last 30 days:\n\n    %s\n\nInvestigate before retrying this class of task; if a workaround is confirmed, update this card and approve it.",
				len(items), items[0].quote),
			Subject: namespaceID,
			Structured: map[string]any{
				"trigger":     "agent runs matching: " + truncate(sig, 120),
				"severity":    "medium",
				"consequence": "repeated task failures",
			},
			SourceRefs: refs,
			Metadata:   map[string]any{"failure_signature": sig, "occurrences": len(items)},
		})
		if err == nil && resp.DuplicateOf == "" {
			s.C6GotchaProposals++
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------------------------------------------------------------------------
// C7 — decay/dormancy, as §10.7.
// ---------------------------------------------------------------------------

func (c *Consolidator) decay(ctx context.Context, tenantID string, s *Stats) error {
	if c.Checker != nil {
		n, err := c.Cards.VerifySweep(ctx, tenantID, c.Checker, 200)
		if err != nil {
			return err
		}
		s.C7Verified = n
	}
	n, err := c.Cards.DecaySweep(ctx, tenantID, 500)
	if err != nil {
		return err
	}
	s.C7Dormant = n
	return nil
}
