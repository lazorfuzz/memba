package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/lazorfuzz/memba/pkg/memory"
)

// InsertEvidence stores raw evidence, idempotent on
// (tenant, source_uri, source_version, content_hash) — a re-post returns the
// existing row with deduplicated=true (spec §13.2).
func (p *PG) InsertEvidence(ctx context.Context, tenantID string, ev memory.RawEvidence) (memory.RawEvidence, bool, error) {
	row := p.pool.QueryRow(ctx, `
		INSERT INTO raw_evidence
		  (tenant_id, namespace_id, source_type, source_uri, source_external_id,
		   source_version, content_hash, title, body, body_object_key, metadata, acl,
		   event_time, quarantined, quarantine_reason, embedding_forbidden)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		ON CONFLICT (tenant_id, source_uri, source_version, content_hash) DO NOTHING
		RETURNING id, ingested_at`,
		tenantID, ev.NamespaceID, ev.SourceType, ev.SourceURI, nullStr(ev.SourceExternalID),
		ev.SourceVersion, ev.ContentHash, nullStr(ev.Title), ev.Body, nullStr(ev.BodyObjectKey),
		jsonb(ev.Metadata), aclJSON(ev.ACL), nullTime(ev.EventTime),
		ev.Quarantined, nullStr(ev.QuarantineReason), ev.EmbeddingForbidden)

	err := row.Scan(&ev.ID, &ev.IngestedAt)
	if err == nil {
		ev.TenantID = tenantID
		return ev, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ev, false, err
	}
	// Conflict: fetch existing.
	existing := memory.RawEvidence{}
	r2 := p.pool.QueryRow(ctx, `
		SELECT id, ingested_at, quarantined, coalesce(quarantine_reason,'')
		FROM raw_evidence
		WHERE tenant_id=$1 AND source_uri=$2 AND source_version=$3 AND content_hash=$4`,
		tenantID, ev.SourceURI, ev.SourceVersion, ev.ContentHash)
	if err := r2.Scan(&existing.ID, &existing.IngestedAt, &existing.Quarantined, &existing.QuarantineReason); err != nil {
		return ev, false, fmt.Errorf("fetch existing evidence: %w", err)
	}
	ev.ID = existing.ID
	ev.IngestedAt = existing.IngestedAt
	ev.Quarantined = existing.Quarantined
	ev.QuarantineReason = existing.QuarantineReason
	ev.TenantID = tenantID
	return ev, true, nil
}

func (p *PG) GetEvidence(ctx context.Context, tenantID, id string) (memory.RawEvidence, error) {
	var ev memory.RawEvidence
	var acl, meta []byte
	row := p.pool.QueryRow(ctx, `
		SELECT id, tenant_id, namespace_id, source_type, source_uri,
		       coalesce(source_external_id,''), source_version, content_hash,
		       coalesce(title,''), body, coalesce(body_object_key,''), metadata, acl,
		       event_time, ingested_at, quarantined, coalesce(quarantine_reason,''),
		       embedding_forbidden
		FROM raw_evidence WHERE tenant_id=$1 AND id=$2`, tenantID, id)
	var eventTime, ingestedAt = nullTime(nil), nullTime(nil)
	if err := row.Scan(&ev.ID, &ev.TenantID, &ev.NamespaceID, &ev.SourceType, &ev.SourceURI,
		&ev.SourceExternalID, &ev.SourceVersion, &ev.ContentHash, &ev.Title, &ev.Body,
		&ev.BodyObjectKey, &meta, &acl, &eventTime, &ingestedAt.Time,
		&ev.Quarantined, &ev.QuarantineReason, &ev.EmbeddingForbidden); err != nil {
		return ev, err
	}
	ev.Metadata = parseMeta(meta)
	ev.ACL = parseACL(acl)
	ev.EventTime = timePtr(eventTime)
	ev.IngestedAt = ingestedAt.Time
	return ev, nil
}

func (p *PG) UpsertNamespace(ctx context.Context, ns memory.Namespace) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO namespaces (id, tenant_id, parent_id, kind, policy)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (id) DO UPDATE SET policy = EXCLUDED.policy, kind = EXCLUDED.kind
		WHERE namespaces.tenant_id = EXCLUDED.tenant_id`,
		ns.ID, ns.TenantID, nullStr(ns.ParentID), ns.Kind, jsonb(ns.Policy))
	return err
}

func (p *PG) GetNamespace(ctx context.Context, tenantID, id string) (memory.Namespace, error) {
	var ns memory.Namespace
	var policy []byte
	var parent = nullStr("")
	row := p.pool.QueryRow(ctx, `
		SELECT id, tenant_id, parent_id, kind, policy, created_at
		FROM namespaces WHERE tenant_id=$1 AND id=$2`, tenantID, id)
	if err := row.Scan(&ns.ID, &ns.TenantID, &parent, &ns.Kind, &policy, &ns.CreatedAt); err != nil {
		return ns, err
	}
	ns.ParentID = parent.String
	_ = jsonUnmarshal(policy, &ns.Policy)
	return ns, nil
}

// ResolveScope expands hints with all ancestor namespaces (spec §4.3), using
// the parent chain where registered and path-prefix fallback otherwise.
func (p *PG) ResolveScope(ctx context.Context, tenantID string, hints []string) ([]string, error) {
	seen := map[string]struct{}{}
	var out []string
	add := func(s string) {
		if s == "" {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	for _, h := range hints {
		add(h)
		// Path-prefix ancestors: /a/b/c → /a/b, /a
		for i := len(h) - 1; i > 0; i-- {
			if h[i] == '/' {
				add(h[:i])
			}
		}
	}
	// Also follow registered parent links (covers non-path hierarchies).
	rows, err := p.pool.Query(ctx, `
		WITH RECURSIVE anc AS (
		  SELECT id, parent_id FROM namespaces WHERE tenant_id=$1 AND id = ANY($2)
		  UNION
		  SELECT n.id, n.parent_id FROM namespaces n JOIN anc ON n.id = anc.parent_id
		    WHERE n.tenant_id=$1
		)
		SELECT id FROM anc`, tenantID, hints)
	if err != nil {
		return out, nil // fallback to path-derived scope
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			add(id)
		}
	}
	return out, nil
}

func jsonUnmarshal(b []byte, v any) error {
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, v)
}
