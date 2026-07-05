package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/lazorfuzz/memba/internal/store"
	"github.com/lazorfuzz/memba/pkg/memory"
)

// --- verifications ----------------------------------------------------------

func (p *PG) InsertVerification(ctx context.Context, tenantID, namespaceID string, v memory.VerificationResult) (string, error) {
	detail, _ := json.Marshal(map[string]any{"per_ref": v.PerRef})
	var id string
	err := p.pool.QueryRow(ctx, `
		INSERT INTO verifications (tenant_id, namespace_id, target_type, target_id,
		    verification_type, repo, branch, head_sha, result, detail)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		RETURNING id`,
		tenantID, namespaceID, v.TargetType, v.TargetID, v.Type,
		nullStr(v.Repo), nullStr(v.Branch), nullStr(v.HeadSHA), v.Result, detail).Scan(&id)
	return id, err
}

func scanVerification(row pgx.Row) (memory.VerificationResult, error) {
	var v memory.VerificationResult
	var repo, branch, headSHA = nullStr(""), nullStr(""), nullStr("")
	var detail []byte
	err := row.Scan(&v.VerificationID, &v.TargetType, &v.TargetID, &v.Type,
		&repo, &branch, &headSHA, &v.Result, &detail, &v.VerifiedAt)
	if err != nil {
		return v, err
	}
	v.Repo, v.Branch, v.HeadSHA = repo.String, branch.String, headSHA.String
	var d struct {
		PerRef []memory.RefOutcome `json:"per_ref"`
	}
	_ = json.Unmarshal(detail, &d)
	v.PerRef = d.PerRef
	return v, nil
}

// CachedVerification implements the JIT cache lookup keyed on
// (target_id, repo, head_sha) — spec §9.4.
func (p *PG) CachedVerification(ctx context.Context, targetID, repo, headSHA string) (memory.VerificationResult, bool, error) {
	v, err := scanVerification(p.pool.QueryRow(ctx, `
		SELECT id, target_type, target_id, verification_type, repo, branch, head_sha,
		       result, detail, verified_at
		FROM verifications
		WHERE target_id=$1 AND repo=$2 AND head_sha=$3
		ORDER BY verified_at DESC LIMIT 1`, targetID, repo, headSHA))
	if errors.Is(err, pgx.ErrNoRows) {
		return v, false, nil
	}
	return v, err == nil, err
}

func (p *PG) LatestVerification(ctx context.Context, targetID string) (memory.VerificationResult, bool, error) {
	v, err := scanVerification(p.pool.QueryRow(ctx, `
		SELECT id, target_type, target_id, verification_type, repo, branch, head_sha,
		       result, detail, verified_at
		FROM verifications WHERE target_id=$1
		ORDER BY verified_at DESC LIMIT 1`, targetID))
	if errors.Is(err, pgx.ErrNoRows) {
		return v, false, nil
	}
	return v, err == nil, err
}

// --- actions -----------------------------------------------------------------

func (p *PG) InsertAction(ctx context.Context, a memory.MemoryAction) error {
	var runID any
	if a.RunID != "" {
		runID = a.RunID
	}
	_, err := p.pool.Exec(ctx, `
		INSERT INTO memory_actions (run_id, tenant_id, namespace_id, principal,
		    action_type, input, output, success, latency_ms)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		runID, a.TenantID, a.NamespaceID, a.Principal, a.ActionType,
		jsonb(a.Input), jsonb(a.Output), a.Success, nullInt(a.LatencyMs))
	return err
}

// --- workspaces ----------------------------------------------------------------

func (p *PG) InsertWorkspace(ctx context.Context, tenantID, namespaceID, principal, query, mode, objectKey string, manifest memory.WorkspaceManifest, tokenTotal int, expiresAt time.Time) (string, error) {
	var id string
	err := p.pool.QueryRow(ctx, `
		INSERT INTO workspaces (tenant_id, namespace_id, principal, query, mode,
		    manifest, object_key, token_total, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING id`,
		tenantID, namespaceID, principal, query, mode, jsonb(manifest), objectKey,
		tokenTotal, expiresAt).Scan(&id)
	return id, err
}

func (p *PG) GetWorkspace(ctx context.Context, tenantID, id string) (memory.WorkspaceManifest, string, error) {
	var manifest []byte
	var objectKey string
	err := p.pool.QueryRow(ctx, `
		SELECT manifest, object_key FROM workspaces
		WHERE tenant_id=$1 AND id=$2 AND expires_at > now()`, tenantID, id).Scan(&manifest, &objectKey)
	var m memory.WorkspaceManifest
	if err != nil {
		return m, "", err
	}
	err = json.Unmarshal(manifest, &m)
	return m, objectKey, err
}

func (p *PG) ExpiredWorkspaces(ctx context.Context, now time.Time, limit int) ([]struct{ ID, TenantID, ObjectKey string }, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id, tenant_id, object_key FROM workspaces
		WHERE expires_at <= $1 LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct{ ID, TenantID, ObjectKey string }
	for rows.Next() {
		var w struct{ ID, TenantID, ObjectKey string }
		if err := rows.Scan(&w.ID, &w.TenantID, &w.ObjectKey); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (p *PG) DeleteWorkspace(ctx context.Context, tenantID, id string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM workspaces WHERE tenant_id=$1 AND id=$2`, tenantID, id)
	return err
}

// --- jobs (FOR UPDATE SKIP LOCKED queue; river-compatible semantics) -----------

func (p *PG) EnqueueJob(ctx context.Context, tenantID, kind string, payload any) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO jobs (kind, tenant_id, payload) VALUES ($1,$2,$3)`,
		kind, tenantID, jsonb(payload))
	return err
}

func (p *PG) DequeueJob(ctx context.Context, kinds []string) (*store.Job, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var j store.Job
	err = tx.QueryRow(ctx, `
		SELECT id, kind, tenant_id, payload, attempts, max_attempts
		FROM jobs
		WHERE state='available' AND scheduled_at <= now() AND kind = ANY($1)
		ORDER BY scheduled_at
		FOR UPDATE SKIP LOCKED
		LIMIT 1`, kinds).Scan(&j.ID, &j.Kind, &j.TenantID, &j.Payload, &j.Attempts, &j.MaxAttempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE jobs SET state='running', attempts=attempts+1, started_at=now()
		WHERE id=$1`, j.ID); err != nil {
		return nil, err
	}
	return &j, tx.Commit(ctx)
}

func (p *PG) CompleteJob(ctx context.Context, id int64) error {
	_, err := p.pool.Exec(ctx, `
		UPDATE jobs SET state='completed', finished_at=now() WHERE id=$1`, id)
	return err
}

func (p *PG) FailJob(ctx context.Context, id int64, errMsg string) error {
	_, err := p.pool.Exec(ctx, `
		UPDATE jobs SET
		  state = CASE WHEN attempts >= max_attempts THEN 'discarded' ELSE 'available' END,
		  scheduled_at = now() + (interval '2 seconds') * power(2, attempts),
		  last_error = $2,
		  finished_at = CASE WHEN attempts >= max_attempts THEN now() ELSE NULL END
		WHERE id=$1`, id, errMsg)
	return err
}
