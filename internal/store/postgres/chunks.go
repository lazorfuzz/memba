package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/lazorfuzz/memba/internal/store"
	"github.com/lazorfuzz/memba/pkg/memory"
)

func (p *PG) InsertChunks(ctx context.Context, tenantID, namespaceID string, acl memory.ACL, rows []store.ChunkRow) error {
	if len(rows) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, r := range rows {
		var vec any
		if r.VectorLiteral != "" {
			vec = r.VectorLiteral
		}
		if r.Symbols == nil {
			r.Symbols = []string{}
		}
		batch.Queue(`
			INSERT INTO chunks
			  (tenant_id, namespace_id, raw_id, ordinal, kind, path, line_start, line_end,
			   token_count, body, ident_text, symbols, embedding, embedding_model, acl, quarantined)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
			ON CONFLICT (raw_id, ordinal) DO NOTHING`,
			tenantID, namespaceID, r.RawID, r.Ordinal, r.Kind, nullStr(r.Path),
			nullInt(r.LineStart), nullInt(r.LineEnd), r.TokenCount, r.Body,
			nullStr(r.IdentText), r.Symbols, vec, nullStr(r.EmbeddingModel),
			aclJSON(acl), r.Quarantined)
	}
	br := p.pool.SendBatch(ctx, batch)
	defer br.Close()
	for range rows {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("insert chunk: %w", err)
		}
	}
	return nil
}

// scopeWhere builds the mandatory tenant/namespace/ACL/quarantine predicate
// (spec §8.2 step 4: filters pushed into SQL). Args start at $1.
const chunkSelect = `
	SELECT c.id, c.namespace_id, c.raw_id, r.source_uri, r.source_type,
	       coalesce(c.path,''), coalesce(c.line_start,0), coalesce(c.line_end,0),
	       c.token_count, c.body, r.event_time, %s AS score
	FROM chunks c JOIN raw_evidence r ON r.id = c.raw_id
	WHERE c.tenant_id = $1
	  AND c.namespace_id = ANY($2)
	  AND NOT c.quarantined
	  AND c.acl->'read' ?| $3
	  AND %s
	ORDER BY score DESC
	LIMIT $4`

func (p *PG) scanChunkCandidates(rows pgx.Rows) ([]store.Candidate, error) {
	defer rows.Close()
	var out []store.Candidate
	for rows.Next() {
		var c store.Candidate
		c.Kind = "chunk"
		var eventTime = nullTime(nil)
		if err := rows.Scan(&c.ID, &c.NamespaceID, &c.RawID, &c.SourceURI, &c.SourceType,
			&c.Path, &c.LineStart, &c.LineEnd, &c.TokenCount, &c.Body, &eventTime, &c.Score); err != nil {
			return nil, err
		}
		c.EventTime = timePtr(eventTime)
		out = append(out, c)
	}
	return out, rows.Err()
}

// SearchChunksFTS is retriever R1 (lexical) over chunks.
func (p *PG) SearchChunksFTS(ctx context.Context, f store.ScopeFilter, query string, n int) ([]store.Candidate, error) {
	q := fmt.Sprintf(chunkSelect,
		`ts_rank_cd(c.tsv, websearch_to_tsquery('simple', $5))`,
		`c.tsv @@ websearch_to_tsquery('simple', $5)`)
	rows, err := p.pool.Query(ctx, q, f.TenantID, f.Namespaces, f.ACLSubjects, n, query)
	if err != nil {
		return nil, err
	}
	return p.scanChunkCandidates(rows)
}

// SearchChunksTrgm is retriever R2 (identifier trigram).
func (p *PG) SearchChunksTrgm(ctx context.Context, f store.ScopeFilter, identQuery string, n int) ([]store.Candidate, error) {
	q := fmt.Sprintf(chunkSelect,
		`similarity(c.ident_text, $5)`,
		`c.ident_text IS NOT NULL AND similarity(c.ident_text, $5) > 0.3`)
	rows, err := p.pool.Query(ctx, q, f.TenantID, f.Namespaces, f.ACLSubjects, n, identQuery)
	if err != nil {
		return nil, err
	}
	return p.scanChunkCandidates(rows)
}

// SearchChunksVector is retriever R3 (HNSW cosine). Iterative index scans
// keep recall under ACL/namespace filters (pgvector ≥ 0.8, spec §6.3).
func (p *PG) SearchChunksVector(ctx context.Context, f store.ScopeFilter, vectorLiteral string, n int) ([]store.Candidate, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	_, _ = tx.Exec(ctx, `SET LOCAL hnsw.ef_search = 80`)
	_, _ = tx.Exec(ctx, `SET LOCAL hnsw.iterative_scan = relaxed_order`)
	q := fmt.Sprintf(chunkSelect,
		`1 - (c.embedding <=> $5::halfvec)`,
		`c.embedding IS NOT NULL`)
	// ORDER BY score DESC over cosine distance is equivalent to ASC distance.
	rows, err := tx.Query(ctx, q, f.TenantID, f.Namespaces, f.ACLSubjects, n, vectorLiteral)
	if err != nil {
		return nil, err
	}
	out, err := p.scanChunkCandidates(rows)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit(ctx)
}

// SearchChunksSymbols is retriever R4 (exact/prefix symbol match).
func (p *PG) SearchChunksSymbols(ctx context.Context, f store.ScopeFilter, symbols []string, n int) ([]store.Candidate, error) {
	if len(symbols) == 0 {
		return nil, nil
	}
	q := fmt.Sprintf(chunkSelect,
		`(SELECT count(*) FROM unnest(c.symbols) s WHERE s = ANY($5))::float`,
		`c.symbols && $5`)
	rows, err := p.pool.Query(ctx, q, f.TenantID, f.Namespaces, f.ACLSubjects, n, symbols)
	if err != nil {
		return nil, err
	}
	return p.scanChunkCandidates(rows)
}
