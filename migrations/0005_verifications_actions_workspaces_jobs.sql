-- +goose Up

-- Verifications (spec §6.7)
CREATE TABLE verifications (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         text NOT NULL,
    namespace_id      text NOT NULL,
    target_type       text NOT NULL CHECK (target_type IN ('card','fact')),
    target_id         uuid NOT NULL,
    verification_type text NOT NULL CHECK (verification_type IN
                        ('code_branch_check','test_passed','command_succeeded',
                         'source_still_exists','human_review','multi_source_support')),
    repo       text,
    branch     text,
    commit_sha text,
    head_sha   text,
    result            text NOT NULL CHECK (result IN ('passed','failed','inconclusive')),
    detail            jsonb NOT NULL DEFAULT '{}',
    verified_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX verif_target ON verifications(target_id, verified_at DESC);
CREATE INDEX verif_cache  ON verifications(target_id, repo, head_sha);

-- Memory actions (instrumentation; spec §6.9)
CREATE TABLE memory_actions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id       uuid,
    tenant_id    text NOT NULL,
    namespace_id text NOT NULL,
    principal    text NOT NULL,
    action_type  text NOT NULL CHECK (action_type IN
                   ('search','open','workspace_mount','log','propose','verify',
                    'invalidate','cite','ignore','promote_request')),
    input        jsonb NOT NULL,
    output       jsonb,
    success      bool,
    latency_ms   int,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX actions_run ON memory_actions(run_id, created_at);

-- Workspaces (spec §6.10)
CREATE TABLE workspaces (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    text NOT NULL,
    namespace_id text NOT NULL,
    principal    text NOT NULL,
    query        text NOT NULL,
    mode         text NOT NULL,
    manifest     jsonb NOT NULL,
    object_key   text NOT NULL,
    token_total  int NOT NULL,
    expires_at   timestamptz NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX workspaces_expiry ON workspaces(expires_at);

-- Background jobs. The spec pins riverqueue/river; this table implements the
-- same FOR UPDATE SKIP LOCKED semantics with the §6.10 job kinds so multiple
-- memd replicas coordinate safely. Swapping in river later is an adapter
-- change behind internal/jobs.Queue (see README "deviations").
CREATE TABLE jobs (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind         text NOT NULL CHECK (kind IN
                   ('ingest_chunk','embed','extract_cards','verify_sweep','decay_sweep',
                    'consolidate_ns','recompute_acl','reembed_model_migration','workspace_gc')),
    tenant_id    text NOT NULL,
    payload      jsonb NOT NULL DEFAULT '{}',
    state        text NOT NULL DEFAULT 'available' CHECK (state IN
                   ('available','running','completed','failed','discarded')),
    attempts     int NOT NULL DEFAULT 0,
    max_attempts int NOT NULL DEFAULT 5,
    last_error   text,
    scheduled_at timestamptz NOT NULL DEFAULT now(),
    started_at   timestamptz,
    finished_at  timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX jobs_poll ON jobs(state, scheduled_at) WHERE state = 'available';

-- +goose Down
DROP TABLE IF EXISTS jobs;
DROP TABLE IF EXISTS workspaces;
DROP TABLE IF EXISTS memory_actions;
DROP TABLE IF EXISTS verifications;
