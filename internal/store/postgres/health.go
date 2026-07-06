package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/lazorfuzz/memba/internal/store"
	"github.com/lazorfuzz/memba/pkg/memory"
)

func (p *PG) VerificationHistory(ctx context.Context, targetID string, limit int) ([]memory.VerificationResult, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := p.pool.Query(ctx, `
		SELECT id, target_type, target_id, verification_type, repo, branch, head_sha,
		       result, detail, verified_at
		FROM verifications WHERE target_id=$1
		ORDER BY verified_at DESC LIMIT $2`, targetID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []memory.VerificationResult
	for rows.Next() {
		v, err := scanVerification(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (p *PG) ListReviews(ctx context.Context, cardID string) ([]store.Review, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT reviewer, decision, coalesce(reason,''), coalesce(edited_body,''), created_at
		FROM card_reviews WHERE card_id=$1 ORDER BY created_at DESC`, cardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Review
	for rows.Next() {
		var r store.Review
		if err := rows.Scan(&r.Reviewer, &r.Decision, &r.Reason, &r.EditedBody, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Health gathers the §16 dashboard counters for one tenant.
func (p *PG) Health(ctx context.Context, tenantID string) (store.HealthStats, error) {
	h := store.HealthStats{CardsByStatus: map[string]int{}}

	rows, err := p.pool.Query(ctx, `
		SELECT status, count(*) FROM memory_cards WHERE tenant_id=$1 GROUP BY status`, tenantID)
	if err != nil {
		return h, err
	}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			rows.Close()
			return h, err
		}
		h.CardsByStatus[status] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return h, err
	}
	h.ProposalQueueDepth = h.CardsByStatus[memory.StatusProposed]

	weekAgo := time.Now().UTC().AddDate(0, 0, -7)
	batch := &pgx.Batch{}
	batch.Queue(`SELECT count(*) FROM memory_cards WHERE tenant_id=$1 AND status='active' AND verify_by IS NOT NULL AND verify_by <= now()`, tenantID)
	batch.Queue(`SELECT count(*) FROM raw_evidence WHERE tenant_id=$1 AND quarantined`, tenantID)
	batch.Queue(`SELECT count(*) FROM raw_evidence WHERE tenant_id=$1`, tenantID)
	batch.Queue(`SELECT count(*) FROM chunks WHERE tenant_id=$1`, tenantID)
	batch.Queue(`SELECT count(*) FROM facts WHERE tenant_id=$1 AND status='active' AND retracted_at IS NULL`, tenantID)
	// Median time-to-promotion (§10.6 anti-starvation metric): promotion sets
	// valid_from; created_at is proposal time.
	batch.Queue(`SELECT coalesce(percentile_cont(0.5) WITHIN GROUP (
	                 ORDER BY EXTRACT(EPOCH FROM (valid_from - created_at))), 0)
	             FROM memory_cards
	             WHERE tenant_id=$1 AND valid_from IS NOT NULL AND valid_from > created_at`, tenantID)
	batch.Queue(`SELECT count(*) FROM memory_actions WHERE tenant_id=$1 AND created_at >= $2 AND output->>'answerability'='low'`, tenantID, weekAgo)
	batch.Queue(`SELECT count(*) FROM consolidation_runs WHERE tenant_id=$1 AND started_at >= $2`, tenantID, weekAgo)

	br := p.pool.SendBatch(ctx, batch)
	defer br.Close()
	targets := []any{
		&h.VerifyOverdue, &h.QuarantinedRaw, &h.EvidenceTotal, &h.ChunksTotal,
		&h.FactsActive, &h.MedianTimeToPromoteSeconds, &h.LowAnswerability7d, &h.ConsolidationRuns7d,
	}
	for _, target := range targets {
		if err := br.QueryRow().Scan(target); err != nil {
			return h, err
		}
	}
	return h, nil
}
