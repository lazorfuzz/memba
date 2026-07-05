// Package retrieve implements query analysis, the R1–R6 candidate
// generators, and RRF fusion (spec §8.2 steps 3–5).
package retrieve

import (
	"regexp"
	"strings"
	"time"

	"github.com/lazorfuzz/memba/internal/chunk"
)

// AnalyzedQuery is the output of the cheap, no-LLM query analysis
// (spec §8.2 step 3).
type AnalyzedQuery struct {
	Raw         string
	Identifiers []string // CamelCase/snake_case tokens, paths, error codes, TICKET-123
	Symbols     []string // identifier subset likely to be code symbols
	Terms       []string // lowercase content words for fact matching
	Temporal    bool     // "as of", "before the migration", …
	AsOf        *time.Time
}

var temporalRe = regexp.MustCompile(`(?i)\b(as of|before the|after the|back then|used to|previously|at the time|last (year|month|quarter)|in (19|20)\d\d)\b`)

var stopwords = map[string]struct{}{
	"the": {}, "a": {}, "an": {}, "and": {}, "or": {}, "of": {}, "to": {}, "in": {},
	"for": {}, "on": {}, "with": {}, "how": {}, "do": {}, "does": {}, "i": {}, "we": {},
	"is": {}, "are": {}, "was": {}, "what": {}, "which": {}, "when": {}, "where": {},
	"who": {}, "why": {}, "can": {}, "should": {}, "would": {}, "add": {}, "new": {},
	"safely": {}, "this": {}, "that": {}, "it": {}, "be": {}, "my": {}, "our": {},
}

var wordRe = regexp.MustCompile(`[A-Za-z0-9_./\-]+`)

// Analyze performs deterministic query analysis.
func Analyze(query string, asOf *time.Time) AnalyzedQuery {
	aq := AnalyzedQuery{Raw: query, AsOf: asOf}
	aq.Identifiers = chunk.ExtractIdentifiers(query)
	for _, id := range aq.Identifiers {
		// Symbols: identifiers without path separators.
		if !strings.ContainsAny(id, "/.") {
			aq.Symbols = append(aq.Symbols, id)
		}
	}
	seen := map[string]struct{}{}
	for _, w := range wordRe.FindAllString(strings.ToLower(query), -1) {
		if len(w) < 3 {
			continue
		}
		if _, stop := stopwords[w]; stop {
			continue
		}
		if _, dup := seen[w]; dup {
			continue
		}
		seen[w] = struct{}{}
		aq.Terms = append(aq.Terms, w)
	}
	aq.Temporal = temporalRe.MatchString(query) || asOf != nil
	return aq
}
