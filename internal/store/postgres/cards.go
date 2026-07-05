package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/lazorfuzz/memba/internal/store"
	"github.com/lazorfuzz/memba/pkg/memory"
)

const cardCols = `
	id, tenant_id, namespace_id, card_type, title, body, structured,
	coalesce(subject,''), tags, entities, status, confidence, importance,
	valid_from, valid_to, ttl_class, last_verified_at, verify_by,
	last_retrieved_at, created_from, created_by, acl, metadata, created_at, updated_at`

func scanCard(row pgx.Row) (memory.Card, error) {
	var c memory.Card
	var structured, acl, meta []byte
	var validFrom, validTo, lastVerified, verifyBy, lastRetrieved = nullTime(nil), nullTime(nil), nullTime(nil), nullTime(nil), nullTime(nil)
	err := row.Scan(&c.ID, &c.TenantID, &c.NamespaceID, &c.CardType, &c.Title, &c.Body,
		&structured, &c.Subject, &c.Tags, &c.Entities, &c.Status, &c.Confidence,
		&c.Importance, &validFrom, &validTo, &c.TTLClass, &lastVerified, &verifyBy,
		&lastRetrieved, &c.CreatedFrom, &c.CreatedBy, &acl, &meta, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return c, err
	}
	c.Structured = parseMeta(structured)
	c.ACL = parseACL(acl)
	c.Metadata = parseMeta(meta)
	c.ValidFrom, c.ValidTo = timePtr(validFrom), timePtr(validTo)
	c.LastVerifiedAt, c.VerifyBy, c.LastRetrievedAt = timePtr(lastVerified), timePtr(verifyBy), timePtr(lastRetrieved)
	return c, nil
}

func (p *PG) InsertCard(ctx context.Context, card memory.Card, vectorLiteral string) (string, error) {
	var vec any
	if vectorLiteral != "" {
		vec = vectorLiteral
	}
	if card.Tags == nil {
		card.Tags = []string{}
	}
	if card.Entities == nil {
		card.Entities = []string{}
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO memory_cards
		  (tenant_id, namespace_id, card_type, title, body, structured, subject, tags,
		   entities, status, confidence, importance, valid_from, valid_to, ttl_class,
		   created_from, created_by, embedding, acl, metadata)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
		RETURNING id`,
		card.TenantID, card.NamespaceID, card.CardType, card.Title, card.Body,
		jsonb(card.Structured), nullStr(card.Subject), card.Tags, card.Entities,
		card.Status, card.Confidence, card.Importance, nullTime(card.ValidFrom),
		nullTime(card.ValidTo), card.TTLClass, card.CreatedFrom, card.CreatedBy,
		vec, aclJSON(card.ACL), jsonb(card.Metadata)).Scan(&id)
	if err != nil {
		return "", err
	}
	for _, s := range card.Sources {
		if err := insertCardSourceTx(ctx, tx, id, s); err != nil {
			return "", err
		}
	}
	return id, tx.Commit(ctx)
}

func insertCardSourceTx(ctx context.Context, tx pgx.Tx, cardID string, s memory.SourceRef) error {
	st := s.SupportType
	if st == "" {
		st = "supports"
	}
	var rawID, chunkID any
	if s.RawID != "" {
		rawID = s.RawID
	}
	if s.ChunkID != "" {
		chunkID = s.ChunkID
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO memory_card_sources
		  (card_id, raw_id, chunk_id, source_uri, source_version, path, line_start, line_end, quote, support_type)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT DO NOTHING`,
		cardID, rawID, chunkID, s.SourceURI, nullStr(s.SourceVersion), nullStr(s.Path),
		nullInt(s.LineStart), nullInt(s.LineEnd), nullStr(s.Quote), st)
	return err
}

func (p *PG) AddCardSource(ctx context.Context, cardID string, ref memory.SourceRef) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := insertCardSourceTx(ctx, tx, cardID, ref); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *PG) GetCard(ctx context.Context, tenantID, id string) (memory.Card, error) {
	card, err := scanCard(p.pool.QueryRow(ctx,
		`SELECT `+cardCols+` FROM memory_cards WHERE tenant_id=$1 AND id=$2`, tenantID, id))
	if err != nil {
		return card, err
	}
	card.Sources, err = p.CardSources(ctx, id)
	return card, err
}

func (p *PG) CardSources(ctx context.Context, cardID string) ([]memory.SourceRef, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT coalesce(raw_id::text,''), coalesce(chunk_id::text,''), source_uri,
		       coalesce(source_version,''), coalesce(path,''), coalesce(line_start,0),
		       coalesce(line_end,0), coalesce(quote,''), support_type
		FROM memory_card_sources WHERE card_id=$1`, cardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []memory.SourceRef
	for rows.Next() {
		var s memory.SourceRef
		if err := rows.Scan(&s.RawID, &s.ChunkID, &s.SourceURI, &s.SourceVersion,
			&s.Path, &s.LineStart, &s.LineEnd, &s.Quote, &s.SupportType); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (p *PG) ListCards(ctx context.Context, tenantID string, f store.CardFilter) ([]memory.Card, error) {
	q := `SELECT ` + cardCols + ` FROM memory_cards WHERE tenant_id=$1`
	args := []any{tenantID}
	if f.NamespaceID != "" {
		args = append(args, f.NamespaceID)
		q += fmt.Sprintf(" AND namespace_id=$%d", len(args))
	}
	if f.Status != "" {
		args = append(args, f.Status)
		q += fmt.Sprintf(" AND status=$%d", len(args))
	}
	if f.CardType != "" {
		args = append(args, f.CardType)
		q += fmt.Sprintf(" AND card_type=$%d", len(args))
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	args = append(args, limit)
	q += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d", len(args))
	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []memory.Card
	for rows.Next() {
		c, err := scanCard(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (p *PG) UpdateCardStatus(ctx context.Context, tenantID, id, status, reason string) error {
	tag, err := p.pool.Exec(ctx, `
		UPDATE memory_cards
		SET status=$3, updated_at=now(),
		    metadata = metadata || jsonb_build_object('status_reason', $4::text)
		WHERE tenant_id=$1 AND id=$2`, tenantID, id, status, reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (p *PG) SetCardVerified(ctx context.Context, tenantID, id string, verifiedAt, verifyBy time.Time, confidence float64) error {
	_, err := p.pool.Exec(ctx, `
		UPDATE memory_cards
		SET last_verified_at=$3, verify_by=$4,
		    confidence=GREATEST(confidence,$5),
		    status = CASE WHEN status='stale' THEN 'active' ELSE status END,
		    updated_at=now()
		WHERE tenant_id=$1 AND id=$2`, tenantID, id, verifiedAt, verifyBy, confidence)
	return err
}

func (p *PG) PromoteCard(ctx context.Context, tenantID, id string, confidence float64) error {
	_, err := p.pool.Exec(ctx, `
		UPDATE memory_cards
		SET status='active', confidence=$3, valid_from=coalesce(valid_from, now()), updated_at=now()
		WHERE tenant_id=$1 AND id=$2 AND status='proposed'`, tenantID, id, confidence)
	return err
}

func (p *PG) TouchCardsRetrieved(ctx context.Context, tenantID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := p.pool.Exec(ctx, `
		UPDATE memory_cards SET last_retrieved_at=now()
		WHERE tenant_id=$1 AND id = ANY($2::uuid[])`, tenantID, ids)
	return err
}

const cardCandidateSelect = `
	SELECT c.id, c.namespace_id, c.card_type, c.title, c.body, c.structured,
	       coalesce(c.subject,''), c.status, c.confidence, c.ttl_class,
	       c.last_verified_at, c.verify_by, %s AS score
	FROM memory_cards c
	WHERE c.tenant_id = $1
	  AND c.namespace_id = ANY($2)
	  AND c.acl->'read' ?| $3
	  AND c.status = ANY($4)
	  AND %s
	ORDER BY score DESC
	LIMIT $5`

func scanCardCandidates(rows pgx.Rows) ([]store.Candidate, error) {
	defer rows.Close()
	var out []store.Candidate
	for rows.Next() {
		var c store.Candidate
		c.Kind = "card"
		var structured []byte
		var lastVerified, verifyBy = nullTime(nil), nullTime(nil)
		if err := rows.Scan(&c.ID, &c.NamespaceID, &c.CardType, &c.Title, &c.Body,
			&structured, &c.Subject, &c.Status, &c.Confidence, &c.TTLClass,
			&lastVerified, &verifyBy, &c.Score); err != nil {
			return nil, err
		}
		c.Structured = parseMeta(structured)
		c.LastVerifiedAt, c.VerifyBy = timePtr(lastVerified), timePtr(verifyBy)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (p *PG) SearchCardsFTS(ctx context.Context, f store.ScopeFilter, query string, statuses []string, n int) ([]store.Candidate, error) {
	q := fmt.Sprintf(cardCandidateSelect,
		`ts_rank_cd(c.tsv, websearch_to_tsquery('simple', $6))`,
		`c.tsv @@ websearch_to_tsquery('simple', $6)`)
	rows, err := p.pool.Query(ctx, q, f.TenantID, f.Namespaces, f.ACLSubjects, statuses, n, query)
	if err != nil {
		return nil, err
	}
	return scanCardCandidates(rows)
}

func (p *PG) SearchCardsVector(ctx context.Context, f store.ScopeFilter, vectorLiteral string, statuses []string, n int) ([]store.Candidate, error) {
	q := fmt.Sprintf(cardCandidateSelect,
		`1 - (c.embedding <=> $6::halfvec)`,
		`c.embedding IS NOT NULL`)
	rows, err := p.pool.Query(ctx, q, f.TenantID, f.Namespaces, f.ACLSubjects, statuses, n, vectorLiteral)
	if err != nil {
		return nil, err
	}
	return scanCardCandidates(rows)
}

// NearestActiveCard supports dedup-at-propose (spec §10.8): nearest
// non-dormant card in scope with the same subject.
func (p *PG) NearestActiveCard(ctx context.Context, tenantID, namespaceID, subject, vectorLiteral string) (memory.Card, float64, bool, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT `+cardCols+`, 1 - (embedding <=> $4::halfvec) AS sim
		FROM memory_cards
		WHERE tenant_id=$1 AND namespace_id=$2 AND coalesce(subject,'')=$3
		  AND status NOT IN ('dormant','invalidated','quarantined')
		  AND embedding IS NOT NULL
		ORDER BY embedding <=> $4::halfvec
		LIMIT 1`, tenantID, namespaceID, subject, vectorLiteral)
	var c memory.Card
	var structured, acl, meta []byte
	var validFrom, validTo, lastVerified, verifyBy, lastRetrieved = nullTime(nil), nullTime(nil), nullTime(nil), nullTime(nil), nullTime(nil)
	var sim float64
	err := row.Scan(&c.ID, &c.TenantID, &c.NamespaceID, &c.CardType, &c.Title, &c.Body,
		&structured, &c.Subject, &c.Tags, &c.Entities, &c.Status, &c.Confidence,
		&c.Importance, &validFrom, &validTo, &c.TTLClass, &lastVerified, &verifyBy,
		&lastRetrieved, &c.CreatedFrom, &c.CreatedBy, &acl, &meta, &c.CreatedAt, &c.UpdatedAt, &sim)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, 0, false, nil
	}
	if err != nil {
		return c, 0, false, err
	}
	c.Structured, c.ACL, c.Metadata = parseMeta(structured), parseACL(acl), parseMeta(meta)
	return c, sim, true, nil
}

func (p *PG) InsertCardLink(ctx context.Context, srcID, dstID, linkType, createdBy string, weight float64) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO memory_card_links (src_card_id, dst_card_id, link_type, weight, created_by)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (src_card_id, dst_card_id, link_type) DO NOTHING`,
		srcID, dstID, linkType, weight, createdBy)
	return err
}

// ExpandCardLinks is retriever R6: 1–2-hop link expansion via recursive CTE,
// depth ≤ 2, fan-out cap (spec §8.2 R6). Links route search, never truth.
func (p *PG) ExpandCardLinks(ctx context.Context, f store.ScopeFilter, seedIDs []string, maxDepth, fanoutCap int) ([]store.Candidate, error) {
	if len(seedIDs) == 0 {
		return nil, nil
	}
	rows, err := p.pool.Query(ctx, `
		WITH RECURSIVE walk(card_id, depth) AS (
		  SELECT unnest($4::uuid[]), 0
		  UNION
		  SELECT l.dst_card_id, w.depth + 1
		  FROM memory_card_links l JOIN walk w ON l.src_card_id = w.card_id
		  WHERE w.depth < $5
		)
		SELECT c.id, c.namespace_id, c.card_type, c.title, c.body, c.structured,
		       coalesce(c.subject,''), c.status, c.confidence, c.ttl_class,
		       c.last_verified_at, c.verify_by, 1.0/(1+min(w.depth)) AS score
		FROM walk w
		JOIN memory_cards c ON c.id = w.card_id AND w.depth > 0
		WHERE c.tenant_id=$1 AND c.namespace_id = ANY($2)
		  AND c.acl->'read' ?| $3
		  AND c.status IN ('active','stale')
		GROUP BY c.id, c.namespace_id, c.card_type, c.title, c.body, c.structured,
		         c.subject, c.status, c.confidence, c.ttl_class, c.last_verified_at, c.verify_by
		ORDER BY score DESC
		LIMIT $6`,
		f.TenantID, f.Namespaces, f.ACLSubjects, seedIDs, maxDepth, fanoutCap)
	if err != nil {
		return nil, err
	}
	return scanCardCandidates(rows)
}

func (p *PG) InsertReview(ctx context.Context, cardID, reviewer, decision, reason, editedBody string) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO card_reviews (card_id, reviewer, decision, reason, edited_body)
		VALUES ($1,$2,$3,$4,$5)`, cardID, reviewer, decision, nullStr(reason), nullStr(editedBody))
	return err
}

func (p *PG) LatestApproval(ctx context.Context, cardID string) (bool, error) {
	var decision string
	err := p.pool.QueryRow(ctx, `
		SELECT decision FROM card_reviews WHERE card_id=$1
		ORDER BY created_at DESC LIMIT 1`, cardID).Scan(&decision)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return decision == "approve", nil
}

func (p *PG) CardsVerifyDue(ctx context.Context, tenantID string, before time.Time, limit int) ([]memory.Card, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT `+cardCols+` FROM memory_cards
		WHERE tenant_id=$1 AND status='active' AND verify_by IS NOT NULL AND verify_by <= $2
		ORDER BY verify_by ASC LIMIT $3`, tenantID, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []memory.Card
	for rows.Next() {
		c, err := scanCard(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CardsDecayDue returns stale/unretrieved cards past dormantMultiple×TTL
// (spec §10.7 decay_sweep).
func (p *PG) CardsDecayDue(ctx context.Context, tenantID string, dormantMultiple int, limit int) ([]memory.Card, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT `+cardCols+` FROM memory_cards
		WHERE tenant_id=$1
		  AND status IN ('active','stale')
		  AND verify_by IS NOT NULL
		  AND now() > verify_by + (verify_by - coalesce(last_verified_at, created_at)) * ($2 - 1)
		  AND (last_retrieved_at IS NULL OR last_retrieved_at < verify_by)
		ORDER BY verify_by ASC LIMIT $3`, tenantID, dormantMultiple, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []memory.Card
	for rows.Next() {
		c, err := scanCard(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
