package retrieve

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lazorfuzz/memba/internal/config"
	"github.com/lazorfuzz/memba/internal/embed"
	"github.com/lazorfuzz/memba/internal/store"
	"github.com/lazorfuzz/memba/pkg/memory"
)

// Engine fans out to the R1–R6 retrievers concurrently and fuses their
// rankings with RRF (spec §8.2 steps 4–5). Retriever failures degrade
// gracefully to partial results (spec §14.5), reported in Degraded.
type Engine struct {
	Store    store.Store
	Embedder embed.Embedder
	Cfg      config.Retrieval
}

// Result is the fused candidate pool.
type Result struct {
	Candidates []store.Candidate // fused, best-first, ≤ FusedPool
	Degraded   []string          // names of retrievers that errored
}

type ranked struct {
	name string
	hits []store.Candidate
	err  error
}

// activeStatuses are card statuses eligible for default recall; stale cards
// are included in deep mode and flagged by gate G3 (spec §6.4).
func cardStatuses(mode string) []string {
	if mode == memory.ModeDeep || mode == memory.ModeWorkspace || mode == memory.ModeBenchmark {
		return []string{memory.StatusActive, memory.StatusStale}
	}
	return []string{memory.StatusActive}
}

// Retrieve runs candidate generation for an analyzed query.
func (e *Engine) Retrieve(ctx context.Context, aq AnalyzedQuery, f store.ScopeFilter, mode string) (Result, error) {
	n := e.Cfg.PerRetrieverN
	if n <= 0 {
		n = 50
	}
	statuses := cardStatuses(mode)

	type retrieverFn struct {
		name string
		run  func(context.Context) ([]store.Candidate, error)
	}
	retrievers := []retrieverFn{
		{"r1_lexical", func(ctx context.Context) ([]store.Candidate, error) {
			chunks, err := e.Store.SearchChunksFTS(ctx, f, aq.Raw, n)
			if err != nil {
				return nil, err
			}
			cards, err := e.Store.SearchCardsFTS(ctx, f, aq.Raw, statuses, n)
			if err != nil {
				return chunks, err
			}
			return append(chunks, cards...), nil
		}},
		{"r5_facts", func(ctx context.Context) ([]store.Candidate, error) {
			return e.Store.SearchFacts(ctx, f, aq.Terms, aq.AsOf, n)
		}},
	}
	if len(aq.Identifiers) > 0 {
		identQuery := strings.Join(aq.Identifiers, " ")
		retrievers = append(retrievers, retrieverFn{"r2_identifier", func(ctx context.Context) ([]store.Candidate, error) {
			return e.Store.SearchChunksTrgm(ctx, f, identQuery, n)
		}})
	}
	if len(aq.Symbols) > 0 {
		retrievers = append(retrievers, retrieverFn{"r4_symbol", func(ctx context.Context) ([]store.Candidate, error) {
			return e.Store.SearchChunksSymbols(ctx, f, aq.Symbols, n)
		}})
	}
	if e.Embedder != nil {
		retrievers = append(retrievers, retrieverFn{"r3_vector", func(ctx context.Context) ([]store.Candidate, error) {
			vecs, err := e.Embedder.Embed(ctx, []string{aq.Raw})
			if err != nil || len(vecs) == 0 {
				return nil, err
			}
			lit := embed.VectorLiteral(vecs[0])
			chunks, err := e.Store.SearchChunksVector(ctx, f, lit, n)
			if err != nil {
				return nil, err
			}
			cards, err := e.Store.SearchCardsVector(ctx, f, lit, statuses, n)
			if err != nil {
				return chunks, err
			}
			return append(chunks, cards...), nil
		}})
	}

	results := make([]ranked, len(retrievers))
	var wg sync.WaitGroup
	for i, r := range retrievers {
		wg.Add(1)
		go func(i int, r retrieverFn) {
			defer wg.Done()
			rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			hits, err := r.run(rctx)
			results[i] = ranked{name: r.name, hits: hits, err: err}
		}(i, r)
	}
	wg.Wait()

	var res Result
	var lists []ranked
	for _, r := range results {
		if r.err != nil {
			res.Degraded = append(res.Degraded, r.name)
			continue
		}
		lists = append(lists, r)
	}

	// R6: link expansion seeded by the top cards from R1–R5 (spec §8.2).
	seedIDs := topCardIDs(lists, 10)
	if len(seedIDs) > 0 {
		hits, err := e.Store.ExpandCardLinks(ctx, f, seedIDs, 2, 25)
		if err != nil {
			res.Degraded = append(res.Degraded, "r6_links")
		} else if len(hits) > 0 {
			lists = append(lists, ranked{name: "r6_links", hits: hits})
		}
	}

	pool := e.Cfg.FusedPool
	if pool <= 0 {
		pool = 150
	}
	k := e.Cfg.RRFK
	if k <= 0 {
		k = 60
	}
	res.Candidates = FuseRRF(lists, k, pool)
	return res, nil
}

func topCardIDs(lists []ranked, max int) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, l := range lists {
		for i, c := range l.hits {
			if i >= 10 {
				break
			}
			if c.Kind != "card" {
				continue
			}
			if _, dup := seen[c.ID]; dup {
				continue
			}
			seen[c.ID] = struct{}{}
			out = append(out, c.ID)
			if len(out) >= max {
				return out
			}
		}
	}
	return out
}

// FuseRRF applies Reciprocal Rank Fusion: rrf(d) = Σ_r w_r/(k + rank_r(d)),
// w_r = 1.0 (spec §8.2 step 5). Items are keyed by kind+id.
func FuseRRF(lists []ranked, k, pool int) []store.Candidate {
	type acc struct {
		cand  store.Candidate
		score float64
	}
	byKey := map[string]*acc{}
	var order []string
	for _, l := range lists {
		for rank, c := range l.hits {
			key := c.Kind + ":" + c.ID
			a, ok := byKey[key]
			if !ok {
				a = &acc{cand: c}
				byKey[key] = a
				order = append(order, key)
			}
			a.score += 1.0 / float64(k+rank+1)
		}
	}
	out := make([]store.Candidate, 0, len(order))
	for _, key := range order {
		a := byKey[key]
		a.cand.Score = a.score
		out = append(out, a.cand)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > pool {
		out = out[:pool]
	}
	return out
}

// FuseLists is a test-friendly wrapper over FuseRRF taking plain slices.
func FuseLists(lists [][]store.Candidate, k, pool int) []store.Candidate {
	rs := make([]ranked, len(lists))
	for i, l := range lists {
		rs[i] = ranked{hits: l}
	}
	return FuseRRF(rs, k, pool)
}
