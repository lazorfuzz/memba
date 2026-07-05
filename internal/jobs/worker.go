// Package jobs runs the background job loop (spec §6.10 job kinds) over the
// Postgres FOR UPDATE SKIP LOCKED queue, so multiple memd replicas
// coordinate safely (spec §4.2).
package jobs

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/lazorfuzz/memba/internal/cards"
	"github.com/lazorfuzz/memba/internal/store"
	"github.com/lazorfuzz/memba/internal/verify"
	"github.com/lazorfuzz/memba/internal/workspace"
)

// Worker consumes jobs and runs periodic sweeps.
type Worker struct {
	Store     store.Store
	Cards     *cards.Service
	Verifier  *verify.Verifier
	Workspace *workspace.Writer
	Log       *slog.Logger
	// SweepEvery is the cadence for self-scheduling verify/decay/gc sweeps
	// (nightly in production; short in dev/benchmarks).
	SweepEvery time.Duration
	// Tenants to sweep. Sweeps run per-tenant; the API layer enqueues
	// tenant-scoped jobs, this list covers cron self-scheduling.
	Tenants []string
}

var handledKinds = []string{
	"ingest_chunk", "embed", "extract_cards", "verify_sweep", "decay_sweep",
	"consolidate_ns", "recompute_acl", "reembed_model_migration", "workspace_gc",
}

// Run blocks until ctx is done.
func (w *Worker) Run(ctx context.Context) {
	if w.Log == nil {
		w.Log = slog.Default()
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var sweepTicker *time.Ticker
	if w.SweepEvery > 0 {
		sweepTicker = time.NewTicker(w.SweepEvery)
		defer sweepTicker.Stop()
	} else {
		sweepTicker = time.NewTicker(24 * time.Hour)
		defer sweepTicker.Stop()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-sweepTicker.C:
			w.scheduleSweeps(ctx)
		case <-ticker.C:
			for {
				job, err := w.Store.DequeueJob(ctx, handledKinds)
				if err != nil || job == nil {
					break
				}
				w.handle(ctx, job)
			}
		}
	}
}

func (w *Worker) scheduleSweeps(ctx context.Context) {
	for _, t := range w.Tenants {
		_ = w.Store.EnqueueJob(ctx, t, "verify_sweep", map[string]any{})
		_ = w.Store.EnqueueJob(ctx, t, "decay_sweep", map[string]any{})
	}
	if len(w.Tenants) > 0 {
		_ = w.Store.EnqueueJob(ctx, w.Tenants[0], "workspace_gc", map[string]any{})
	}
}

func (w *Worker) handle(ctx context.Context, job *store.Job) {
	var err error
	switch job.Kind {
	case "verify_sweep":
		var n int
		n, err = w.Cards.VerifySweep(ctx, job.TenantID, w.Verifier, 200)
		if err == nil && n > 0 {
			w.Log.Info("verify_sweep", "tenant", job.TenantID, "cards", n)
		}
	case "decay_sweep":
		var n int
		n, err = w.Cards.DecaySweep(ctx, job.TenantID, 500)
		if err == nil && n > 0 {
			w.Log.Info("decay_sweep", "tenant", job.TenantID, "dormant", n)
		}
	case "workspace_gc":
		var n int
		n, err = w.Workspace.GC(ctx, time.Now().UTC())
		if err == nil && n > 0 {
			w.Log.Info("workspace_gc", "deleted", n)
		}
	case "extract_cards":
		// LLM extraction (§10.4) requires a configured extractor provider;
		// without one the job is a recorded no-op so the queue drains.
		var payload map[string]any
		_ = json.Unmarshal(job.Payload, &payload)
		w.Log.Debug("extract_cards skipped: no extractor provider configured", "payload", payload)
	case "ingest_chunk", "embed":
		// Chunk+embed run inline at ingest in this build (read-your-writes
		// for benchmarks, I5); these kinds exist for async migration.
	case "consolidate_ns", "recompute_acl", "reembed_model_migration":
		w.Log.Debug("job kind not yet implemented; recorded as no-op", "kind", job.Kind)
	}
	if err != nil {
		w.Log.Warn("job failed", "kind", job.Kind, "id", job.ID, "err", err)
		_ = w.Store.FailJob(ctx, job.ID, err.Error())
		return
	}
	_ = w.Store.CompleteJob(ctx, job.ID)
}
