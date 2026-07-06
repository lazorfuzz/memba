package postgres

import (
	"context"
	"time"

	"github.com/lazorfuzz/memba/internal/store"
	"github.com/lazorfuzz/memba/pkg/memory"
)

// --- discovery ----------------------------------------------------------------

func (p *PG) ListTenants(ctx context.Context) ([]string, error) {
	rows, err := p.pool.Query(ctx, `SELECT DISTINCT tenant_id FROM namespaces ORDER BY tenant_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (p *PG) ListNamespaces(ctx context.Context, tenantID string) ([]memory.Namespace, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id, tenant_id, coalesce(parent_id,''), kind, policy, created_at
		FROM namespaces WHERE tenant_id=$1 ORDER BY id`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []memory.Namespace
	for rows.Next() {
		var ns memory.Namespace
		var policy []byte
		if err := rows.Scan(&ns.ID, &ns.TenantID, &ns.ParentID, &ns.Kind, &policy, &ns.CreatedAt); err != nil {
			return nil, err
		}
		_ = jsonUnmarshal(policy, &ns.Policy)
		out = append(out, ns)
	}
	return out, rows.Err()
}

// --- extraction caps -----------------------------------------------------------

func (p *PG) CountCardsCreatedSince(ctx context.Context, tenantID, namespaceID, createdFrom string, since time.Time) (int, error) {
	var n int
	err := p.pool.QueryRow(ctx, `
		SELECT count(*) FROM memory_cards
		WHERE tenant_id=$1 AND namespace_id=$2 AND created_from=$3 AND created_at >= $4`,
		tenantID, namespaceID, createdFrom, since).Scan(&n)
	return n, err
}

// --- consolidation C1: near-duplicate active card pairs -------------------------

func (p *PG) NearDuplicateCardPairs(ctx context.Context, tenantID, namespaceID string, minCosine float64, limit int) ([]store.CardPair, error) {
	// Pairwise over active cards with the same subject (spec §10.8/§11 C1);
	// a.id < b.id avoids mirrored pairs.
	rows, err := p.pool.Query(ctx, `
		SELECT a.id, b.id, 1 - (a.embedding <=> b.embedding) AS sim
		FROM memory_cards a
		JOIN memory_cards b
		  ON a.tenant_id = b.tenant_id
		 AND a.namespace_id = b.namespace_id
		 AND coalesce(a.subject,'') = coalesce(b.subject,'')
		 AND a.id < b.id
		WHERE a.tenant_id=$1 AND a.namespace_id=$2
		  AND a.status='active' AND b.status='active'
		  AND a.embedding IS NOT NULL AND b.embedding IS NOT NULL
		  AND 1 - (a.embedding <=> b.embedding) >= $3
		ORDER BY sim DESC
		LIMIT $4`, tenantID, namespaceID, minCosine, limit)
	if err != nil {
		return nil, err
	}
	type pairID struct {
		a, b string
		sim  float64
	}
	var ids []pairID
	for rows.Next() {
		var pi pairID
		if err := rows.Scan(&pi.a, &pi.b, &pi.sim); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, pi)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []store.CardPair
	for _, pi := range ids {
		a, err := p.GetCard(ctx, tenantID, pi.a)
		if err != nil {
			continue
		}
		b, err := p.GetCard(ctx, tenantID, pi.b)
		if err != nil {
			continue
		}
		out = append(out, store.CardPair{A: a, B: b, Score: pi.sim})
	}
	return out, nil
}

func (p *PG) CardHasSuperseder(ctx context.Context, cardID string) (bool, error) {
	var exists bool
	err := p.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM memory_card_links
		               WHERE dst_card_id=$1 AND link_type='supersedes')`, cardID).Scan(&exists)
	return exists, err
}

// --- consolidation C4: co-citation link inference --------------------------------

func (p *PG) CoCitedCardPairs(ctx context.Context, tenantID, namespaceID string, minShared, limit int) ([]store.CardPair, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT sa.card_id, sb.card_id, count(*) AS shared
		FROM memory_card_sources sa
		JOIN memory_card_sources sb
		  ON sa.raw_id = sb.raw_id AND sa.raw_id IS NOT NULL AND sa.card_id < sb.card_id
		JOIN memory_cards ca ON ca.id = sa.card_id
		JOIN memory_cards cb ON cb.id = sb.card_id
		WHERE ca.tenant_id=$1 AND ca.namespace_id=$2
		  AND cb.tenant_id=$1 AND cb.namespace_id=$2
		  AND ca.status='active' AND cb.status='active'
		  -- only propose links that don't already exist
		  AND NOT EXISTS (SELECT 1 FROM memory_card_links l
		                  WHERE (l.src_card_id=sa.card_id AND l.dst_card_id=sb.card_id)
		                     OR (l.src_card_id=sb.card_id AND l.dst_card_id=sa.card_id))
		GROUP BY sa.card_id, sb.card_id
		HAVING count(*) >= $3
		ORDER BY shared DESC
		LIMIT $4`, tenantID, namespaceID, minShared, limit)
	if err != nil {
		return nil, err
	}
	type pairID struct {
		a, b   string
		shared int
	}
	var ids []pairID
	for rows.Next() {
		var pi pairID
		if err := rows.Scan(&pi.a, &pi.b, &pi.shared); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, pi)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []store.CardPair
	for _, pi := range ids {
		out = append(out, store.CardPair{
			A: memory.Card{ID: pi.a}, B: memory.Card{ID: pi.b}, Score: float64(pi.shared),
		})
	}
	return out, nil
}

// --- consolidation C5: gap mining -------------------------------------------------

func (p *PG) LowAnswerabilityQueries(ctx context.Context, tenantID, namespaceID string, since time.Time, limit int) ([]store.GapQuery, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT input->>'query' AS q, count(*) AS n
		FROM memory_actions
		WHERE tenant_id=$1 AND namespace_id=$2
		  AND action_type IN ('search','workspace_mount')
		  AND created_at >= $3
		  AND output->>'answerability' = 'low'
		  AND coalesce(input->>'query','') <> ''
		GROUP BY q ORDER BY n DESC, q
		LIMIT $4`, tenantID, namespaceID, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.GapQuery
	for rows.Next() {
		var g store.GapQuery
		if err := rows.Scan(&g.Query, &g.Count); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// --- consolidation C6: failure mining ----------------------------------------------

func (p *PG) FailedRunEvidence(ctx context.Context, tenantID, namespaceID string, since time.Time, limit int) ([]memory.RawEvidence, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id, source_uri, coalesce(source_version,''), coalesce(title,''), body, metadata, ingested_at
		FROM raw_evidence
		WHERE tenant_id=$1 AND namespace_id=$2
		  AND source_type='agent_run'
		  AND metadata->>'run_outcome' = 'failed'
		  AND NOT quarantined
		  AND ingested_at >= $3
		ORDER BY ingested_at DESC
		LIMIT $4`, tenantID, namespaceID, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []memory.RawEvidence
	for rows.Next() {
		var ev memory.RawEvidence
		var meta []byte
		if err := rows.Scan(&ev.ID, &ev.SourceURI, &ev.SourceVersion, &ev.Title, &ev.Body, &meta, &ev.IngestedAt); err != nil {
			return nil, err
		}
		ev.TenantID, ev.NamespaceID, ev.SourceType = tenantID, namespaceID, memory.SourceAgentRun
		ev.Metadata = parseMeta(meta)
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (p *PG) InsertConsolidationRun(ctx context.Context, tenantID, namespaceID string, stats map[string]any, runErr string) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO consolidation_runs (tenant_id, namespace_id, finished_at, stats, error)
		VALUES ($1,$2,now(),$3,$4)`, tenantID, namespaceID, jsonb(stats), nullStr(runErr))
	return err
}

