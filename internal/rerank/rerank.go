// Package rerank defines the Reranker interface (spec §14.2) and a local
// lexical-overlap default. The production cross-encoder (bge-reranker-v2-m3
// or a hosted reranker) plugs in behind the same interface; deterministic
// multipliers from spec §8.3 are applied after scoring.
package rerank

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/lazorfuzz/memba/internal/store"
	"github.com/lazorfuzz/memba/pkg/memory"
)

// Reranker scores (query, doc) pairs; higher is more relevant.
type Reranker interface {
	Score(ctx context.Context, query string, docs []string) ([]float64, error)
	ModelID() string
}

// LexicalReranker is the dev/CI default: normalized weighted term-overlap
// between query and candidate text. Deterministic and dependency-free.
type LexicalReranker struct{}

func (LexicalReranker) ModelID() string { return "local/lexical-overlap-v1" }

var tokRe = regexp.MustCompile(`[a-z0-9_]+`)

func (LexicalReranker) Score(_ context.Context, query string, docs []string) ([]float64, error) {
	q := map[string]float64{}
	for _, t := range tokRe.FindAllString(strings.ToLower(query), -1) {
		if len(t) < 3 {
			continue
		}
		q[t] += 1
	}
	out := make([]float64, len(docs))
	for i, d := range docs {
		toks := tokRe.FindAllString(strings.ToLower(d), -1)
		if len(toks) == 0 {
			continue
		}
		seen := map[string]struct{}{}
		var hit float64
		for _, t := range toks {
			if _, dup := seen[t]; dup {
				continue
			}
			seen[t] = struct{}{}
			if _, ok := q[t]; ok {
				hit++
			}
		}
		if len(q) > 0 {
			out[i] = hit / float64(len(q))
		}
	}
	return out, nil
}

// CandidateText builds the rerank text: title+body for cards, path+body for
// chunks (spec §8.2 step 6).
func CandidateText(c store.Candidate) string {
	switch c.Kind {
	case "card":
		return c.Title + "\n" + c.Body
	case "fact":
		return c.Subject + " " + c.Predicate + " " + c.Object
	default:
		return c.Path + "\n" + c.Body
	}
}

// Rerank scores the top `top` fused candidates and re-orders them, applying
// the two deterministic post-rerank multipliers of spec §8.3:
// ×0.8 when the item's only provenance is chat/agent_run, ×1.1 when
// runtime-validated within TTL. The remaining candidates keep their fused
// order below the reranked head.
func Rerank(ctx context.Context, rr Reranker, query string, cands []store.Candidate, top int, now time.Time) ([]store.Candidate, error) {
	if len(cands) == 0 {
		return cands, nil
	}
	if top > len(cands) {
		top = len(cands)
	}
	head := make([]store.Candidate, top)
	copy(head, cands[:top])
	docs := make([]string, top)
	for i, c := range head {
		docs[i] = CandidateText(c)
	}
	scores, err := rr.Score(ctx, query, docs)
	if err != nil {
		return cands, err
	}
	for i := range head {
		s := scores[i]
		// Source authority multiplier: chat/agent_run-only provenance.
		if head[i].Kind == "chunk" &&
			(head[i].SourceType == memory.SourceSlack || head[i].SourceType == memory.SourceAgentRun) {
			s *= 0.8
		}
		// Runtime-validated within TTL.
		if head[i].LastVerifiedAt != nil && head[i].VerifyBy != nil && now.Before(*head[i].VerifyBy) {
			s *= 1.1
		}
		head[i].Score = s
	}
	sort.SliceStable(head, func(i, j int) bool { return head[i].Score > head[j].Score })
	out := append(head, cands[top:]...)
	return out, nil
}
