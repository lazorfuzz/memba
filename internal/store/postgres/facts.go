package postgres

import (
	"context"
	"strings"
	"time"

	"github.com/lazorfuzz/memba/internal/store"
	"github.com/lazorfuzz/memba/pkg/memory"
)

func (p *PG) InsertFact(ctx context.Context, f memory.Fact) (string, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var supersedes, cardID any
	if f.Supersedes != "" {
		supersedes = f.Supersedes
	}
	if f.CardID != "" {
		cardID = f.CardID
	}
	status := f.Status
	if status == "" {
		status = memory.FactProposed
	}
	validFrom := f.ValidFrom
	if validFrom.IsZero() {
		validFrom = time.Now().UTC()
	}
	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO facts (tenant_id, namespace_id, subject, predicate, object, object_type,
		                   valid_from, valid_to, supersedes, card_id, status, acl, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		RETURNING id`,
		f.TenantID, f.NamespaceID, f.Subject, f.Predicate, f.Object,
		orDefault(f.ObjectType, "string"), validFrom, nullTime(f.ValidTo),
		supersedes, cardID, status, aclJSON(f.ACL), f.CreatedBy).Scan(&id)
	if err != nil {
		return "", err
	}
	for _, s := range f.Sources {
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
		if _, err := tx.Exec(ctx, `
			INSERT INTO fact_sources (fact_id, raw_id, chunk_id, source_uri, source_version,
			                          path, line_start, line_end, quote, support_type)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT DO NOTHING`,
			id, rawID, chunkID, s.SourceURI, nullStr(s.SourceVersion), nullStr(s.Path),
			nullInt(s.LineStart), nullInt(s.LineEnd), nullStr(s.Quote), st); err != nil {
			return "", err
		}
	}
	return id, tx.Commit(ctx)
}

// SearchFacts is retriever R5: fact_key match on (subject, predicate)
// guesses plus term match over subject/object, with the bitemporal filter
// (spec §6.6, §8.2). asOf=nil means current state.
func (p *PG) SearchFacts(ctx context.Context, f store.ScopeFilter, terms []string, asOf *time.Time, n int) ([]store.Candidate, error) {
	if len(terms) == 0 {
		return nil, nil
	}
	patterns := make([]string, 0, len(terms))
	for _, t := range terms {
		t = strings.TrimSpace(strings.ToLower(t))
		if len(t) < 3 {
			continue
		}
		patterns = append(patterns, "%"+t+"%")
	}
	if len(patterns) == 0 {
		return nil, nil
	}
	temporal := `f.valid_to IS NULL AND f.retracted_at IS NULL AND f.status='active'`
	args := []any{f.TenantID, f.Namespaces, f.ACLSubjects, patterns, n}
	if asOf != nil {
		// Point-in-time: what did we believe at t (spec §6.6)?
		temporal = `f.asserted_at <= $6 AND coalesce(f.retracted_at,'infinity') > $6
		            AND f.valid_from <= $6 AND coalesce(f.valid_to,'infinity') > $6
		            AND f.status IN ('active','superseded')`
		args = append(args, *asOf)
	}
	rows, err := p.pool.Query(ctx, `
		SELECT f.id, f.namespace_id, f.subject, f.predicate, f.object, f.status,
		       f.valid_from, f.valid_to,
		       (SELECT count(*) FROM unnest($4::text[]) pat
		        WHERE lower(f.subject) LIKE pat OR lower(f.predicate) LIKE pat OR lower(f.object) LIKE pat)::float AS score
		FROM facts f
		WHERE f.tenant_id=$1 AND f.namespace_id = ANY($2)
		  AND f.acl->'read' ?| $3
		  AND `+temporal+`
		  AND EXISTS (SELECT 1 FROM unnest($4::text[]) pat
		              WHERE lower(f.subject) LIKE pat OR lower(f.predicate) LIKE pat OR lower(f.object) LIKE pat)
		ORDER BY score DESC
		LIMIT $5`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Candidate
	for rows.Next() {
		var c store.Candidate
		c.Kind = "fact"
		var validFrom time.Time
		var validTo = nullTime(nil)
		if err := rows.Scan(&c.ID, &c.NamespaceID, &c.Subject, &c.Predicate, &c.Object,
			&c.Status, &validFrom, &validTo, &c.Score); err != nil {
			return nil, err
		}
		c.ValidFrom = &validFrom
		c.ValidTo = timePtr(validTo)
		c.Body = c.Subject + " " + c.Predicate + " = " + c.Object
		out = append(out, c)
	}
	return out, rows.Err()
}

// ActiveFactConflicts finds groups of active facts sharing fact_key with
// overlapping validity and different objects (spec §8.7).
func (p *PG) ActiveFactConflicts(ctx context.Context, f store.ScopeFilter) ([][]memory.Fact, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT f.id, f.namespace_id, f.subject, f.predicate, f.object, f.status,
		       f.valid_from, f.valid_to, f.asserted_at, f.created_by, f.fact_key
		FROM facts f
		WHERE f.tenant_id=$1 AND f.namespace_id = ANY($2) AND f.acl->'read' ?| $3
		  AND f.status='active' AND f.retracted_at IS NULL
		  AND EXISTS (
		    SELECT 1 FROM facts g
		    WHERE g.tenant_id=f.tenant_id AND g.namespace_id = ANY($2)
		      AND g.fact_key=f.fact_key AND g.id <> f.id
		      AND g.status='active' AND g.retracted_at IS NULL
		      AND g.object <> f.object
		      AND tstzrange(f.valid_from, coalesce(f.valid_to,'infinity')) &&
		          tstzrange(g.valid_from, coalesce(g.valid_to,'infinity'))
		  )
		ORDER BY f.fact_key, f.valid_from`, f.TenantID, f.Namespaces, f.ACLSubjects)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := map[string][]memory.Fact{}
	var order []string
	for rows.Next() {
		var fa memory.Fact
		var key string
		var validTo = nullTime(nil)
		if err := rows.Scan(&fa.ID, &fa.NamespaceID, &fa.Subject, &fa.Predicate, &fa.Object,
			&fa.Status, &fa.ValidFrom, &validTo, &fa.AssertedAt, &fa.CreatedBy, &key); err != nil {
			return nil, err
		}
		fa.ValidTo = timePtr(validTo)
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], fa)
	}
	var out [][]memory.Fact
	for _, k := range order {
		if len(groups[k]) > 1 {
			out = append(out, groups[k])
		}
	}
	return out, rows.Err()
}

func (p *PG) FactByID(ctx context.Context, tenantID, id string) (memory.Fact, error) {
	var f memory.Fact
	var acl []byte
	var validTo, retractedAt = nullTime(nil), nullTime(nil)
	var supersedes, cardID = nullStr(""), nullStr("")
	err := p.pool.QueryRow(ctx, `
		SELECT id, tenant_id, namespace_id, subject, predicate, object, object_type,
		       valid_from, valid_to, asserted_at, retracted_at, supersedes::text,
		       card_id::text, status, acl, created_by
		FROM facts WHERE tenant_id=$1 AND id=$2`, tenantID, id).Scan(
		&f.ID, &f.TenantID, &f.NamespaceID, &f.Subject, &f.Predicate, &f.Object,
		&f.ObjectType, &f.ValidFrom, &validTo, &f.AssertedAt, &retractedAt,
		&supersedes, &cardID, &f.Status, &acl, &f.CreatedBy)
	if err != nil {
		return f, err
	}
	f.ValidTo, f.RetractedAt = timePtr(validTo), timePtr(retractedAt)
	f.Supersedes, f.CardID = supersedes.String, cardID.String
	f.ACL = parseACL(acl)
	f.Sources, err = p.FactSources(ctx, id)
	return f, err
}

// SupersedeFact closes the old fact's validity and links the new one.
func (p *PG) SupersedeFact(ctx context.Context, tenantID, oldID, newID string) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		UPDATE facts SET status='superseded', valid_to=coalesce(valid_to, now())
		WHERE tenant_id=$1 AND id=$2`, tenantID, oldID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE facts SET supersedes=$3 WHERE tenant_id=$1 AND id=$2`, tenantID, newID, oldID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *PG) RetractFact(ctx context.Context, tenantID, id string) error {
	_, err := p.pool.Exec(ctx, `
		UPDATE facts SET status='retracted', retracted_at=now()
		WHERE tenant_id=$1 AND id=$2`, tenantID, id)
	return err
}

func (p *PG) FactSources(ctx context.Context, factID string) ([]memory.SourceRef, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT coalesce(raw_id::text,''), coalesce(chunk_id::text,''), source_uri,
		       coalesce(source_version,''), coalesce(path,''), coalesce(line_start,0),
		       coalesce(line_end,0), coalesce(quote,''), support_type
		FROM fact_sources WHERE fact_id=$1`, factID)
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

func (p *PG) AddFactSource(ctx context.Context, factID string, ref memory.SourceRef) error {
	st := ref.SupportType
	if st == "" {
		st = "supports"
	}
	var rawID, chunkID any
	if ref.RawID != "" {
		rawID = ref.RawID
	}
	if ref.ChunkID != "" {
		chunkID = ref.ChunkID
	}
	_, err := p.pool.Exec(ctx, `
		INSERT INTO fact_sources (fact_id, raw_id, chunk_id, source_uri, source_version,
		                          path, line_start, line_end, quote, support_type)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT DO NOTHING`,
		factID, rawID, chunkID, ref.SourceURI, nullStr(ref.SourceVersion), nullStr(ref.Path),
		nullInt(ref.LineStart), nullInt(ref.LineEnd), nullStr(ref.Quote), st)
	return err
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

