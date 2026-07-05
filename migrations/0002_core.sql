-- +goose Up

-- Namespaces (spec §6.1)
CREATE TABLE namespaces (
    id          text PRIMARY KEY,          -- '/acme/platform/payments/repos/billing-api'
    tenant_id   text NOT NULL,
    parent_id   text REFERENCES namespaces(id),
    kind        text NOT NULL CHECK (kind IN ('org','team','service','repo','incident_family')),
    policy      jsonb NOT NULL DEFAULT '{}',
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ns_tenant ON namespaces(tenant_id);

-- Raw evidence (immutable, I1; spec §6.2)
CREATE TABLE raw_evidence (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          text NOT NULL,
    namespace_id       text NOT NULL REFERENCES namespaces(id),
    source_type        text NOT NULL CHECK (source_type IN
                         ('git','github_pr','doc','slack','ci','incident','ticket','agent_run','manual')),
    source_uri         text NOT NULL,
    source_external_id text,
    source_version     text NOT NULL DEFAULT '',
    content_hash       text NOT NULL,
    title              text,
    body               text NOT NULL,
    body_object_key    text,
    metadata           jsonb NOT NULL DEFAULT '{}',
    acl                jsonb NOT NULL,
    event_time         timestamptz,
    ingested_at        timestamptz NOT NULL DEFAULT now(),
    quarantined        bool NOT NULL DEFAULT false,
    quarantine_reason  text,
    embedding_forbidden bool NOT NULL DEFAULT false,
    UNIQUE (tenant_id, source_uri, source_version, content_hash)
);
CREATE INDEX raw_ns_time ON raw_evidence(tenant_id, namespace_id, event_time);

-- Chunks (retrieval substrate; spec §6.3)
CREATE TABLE chunks (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       text NOT NULL,
    namespace_id    text NOT NULL,
    raw_id          uuid NOT NULL REFERENCES raw_evidence(id),
    ordinal         int  NOT NULL,
    kind            text NOT NULL CHECK (kind IN ('code','prose','chat','diff','log','config')),
    path            text,
    line_start      int,
    line_end        int,
    token_count     int  NOT NULL,
    body            text NOT NULL,
    tsv             tsvector GENERATED ALWAYS AS
                      (to_tsvector('simple', coalesce(path,'') || ' ' || body)) STORED,
    ident_text      text,
    symbols         text[] NOT NULL DEFAULT '{}',
    embedding       halfvec(1024),
    embedding_model text,
    acl             jsonb NOT NULL,
    quarantined     bool NOT NULL DEFAULT false,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (raw_id, ordinal)
);
CREATE INDEX chunks_tsv   ON chunks USING gin(tsv);
CREATE INDEX chunks_trgm  ON chunks USING gin(ident_text gin_trgm_ops);
CREATE INDEX chunks_sym   ON chunks USING gin(symbols);
CREATE INDEX chunks_vec   ON chunks USING hnsw (embedding halfvec_cosine_ops)
    WITH (m = 16, ef_construction = 64);
CREATE INDEX chunks_scope ON chunks(tenant_id, namespace_id);

-- +goose Down
DROP TABLE IF EXISTS chunks;
DROP TABLE IF EXISTS raw_evidence;
DROP TABLE IF EXISTS namespaces;
