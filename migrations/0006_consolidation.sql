-- +goose Up

-- Consolidation run stats (spec §11: "All consolidation runs write a
-- consolidation_runs stats row; regressions in card count/quality are
-- observable").
CREATE TABLE consolidation_runs (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    text NOT NULL,
    namespace_id text NOT NULL,
    started_at   timestamptz NOT NULL DEFAULT now(),
    finished_at  timestamptz,
    stats        jsonb NOT NULL DEFAULT '{}',   -- per-job counters C1..C7
    error        text
);
CREATE INDEX consolidation_ns ON consolidation_runs(tenant_id, namespace_id, started_at DESC);

-- +goose Down
DROP TABLE IF EXISTS consolidation_runs;
