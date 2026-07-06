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
	"github.com/lazorfuzz/memba/internal/consolidate"
	"github.com/lazorfuzz/memba/internal/extract"
	"github.com/lazorfuzz/memba/internal/store"
	"github.com/lazorfuzz/memba/internal/verify"
	"github.com/lazorfuzz/memba/internal/workspace"
)

// Worker consumes jobs and runs periodic sweeps.
type Worker struct {
	Store        store.Store
	Cards        *cards.Service
	Verifier     *verify.Verifier
	Workspace    *workspace.Writer
	Extractor    *extract.Extractor        // nil-safe: skips when no LLM configured
	Consolidator *consolidate.Consolidator // runs consolidate_ns (C1–C7)
	Log          *slog.Logger
	// SweepEvery is the cadence for self-scheduling verify/decay/gc sweeps and
	// consolidation (nightly in production; short in dev/benchmarks).
	SweepEvery time.Duration
	// Tenants to sweep; empty = discover from the namespaces table.
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
	tenants := w.Tenants
	if len(tenants) == 0 {
		if discovered, err := w.Store.ListTenants(ctx); err == nil {
			tenants = discovered
		}
	}
	for _, t := range tenants {
		_ = w.Store.EnqueueJob(ctx, t, "verify_sweep", map[string]any{})
		_ = w.Store.EnqueueJob(ctx, t, "decay_sweep", map[string]any{})
		// Sleep-time consolidation per namespace (spec §11).
		if namespaces, err := w.Store.ListNamespaces(ctx, t); err == nil {
			for _, ns := range namespaces {
				_ = w.Store.EnqueueJob(ctx, t, "consolidate_ns", map[string]any{"namespace_id": ns.ID})
			}
		}
	}
	if len(tenants) > 0 {
		_ = w.Store.EnqueueJob(ctx, tenants[0], "workspace_gc", map[string]any{})
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
		var payload struct {
			RawID string `json:"raw_id"`
		}
		_ = json.Unmarshal(job.Payload, &payload)
		if w.Extractor == nil || w.Extractor.LLM == nil {
			w.Log.Debug("extract_cards skipped: no extractor provider configured", "raw_id", payload.RawID)
			break
		}
		var res extract.Result
		res, err = w.Extractor.ExtractFromEvidence(ctx, job.TenantID, payload.RawID)
		if err == nil {
			w.Log.Info("extract_cards", "tenant", job.TenantID, "raw_id", payload.RawID,
				"candidates", res.Candidates, "cards", res.CardsProposed, "promoted", res.CardsPromoted,
				"facts", res.FactsProposed, "discarded_no_quote", res.DiscardedNoQuote, "skipped", res.SkippedReason)
		}
	case "consolidate_ns":
		var payload struct {
			NamespaceID string `json:"namespace_id"`
		}
		_ = json.Unmarshal(job.Payload, &payload)
		if w.Consolidator == nil || payload.NamespaceID == "" {
			break
		}
		var stats consolidate.Stats
		stats, err = w.Consolidator.Run(ctx, job.TenantID, payload.NamespaceID)
		w.Log.Info("consolidate_ns", "tenant", job.TenantID, "namespace", payload.NamespaceID,
			"merged", stats.C1MergedProposals, "profile_updated", stats.C2ProfileUpdated,
			"conflicts", stats.C3Conflicts, "links", stats.C4LinksProposed,
			"gaps", stats.C5Gaps, "failure_gotchas", stats.C6GotchaProposals,
			"verified", stats.C7Verified, "dormant", stats.C7Dormant)
	case "ingest_chunk", "embed":
		// Chunk+embed run inline at ingest in this build (read-your-writes
		// for benchmarks, I5); these kinds exist for async migration.
	case "recompute_acl", "reembed_model_migration":
		w.Log.Debug("job kind not yet implemented; recorded as no-op", "kind", job.Kind)
	}
	if err != nil {
		w.Log.Warn("job failed", "kind", job.Kind, "id", job.ID, "err", err)
		_ = w.Store.FailJob(ctx, job.ID, err.Error())
		return
	}
	_ = w.Store.CompleteJob(ctx, job.ID)
}
