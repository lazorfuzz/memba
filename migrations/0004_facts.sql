-- +goose Up

-- Bitemporal facts (spec §6.6)
CREATE TABLE facts (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     text NOT NULL,
    namespace_id  text NOT NULL REFERENCES namespaces(id),
    subject       text NOT NULL,
    predicate     text NOT NULL,
    object        text NOT NULL,
    object_type   text NOT NULL DEFAULT 'string',
    fact_key      text GENERATED ALWAYS AS (subject || '|' || predicate) STORED,
    valid_from    timestamptz NOT NULL,
    valid_to      timestamptz,
    asserted_at   timestamptz NOT NULL DEFAULT now(),
    retracted_at  timestamptz,
    supersedes    uuid REFERENCES facts(id),
    card_id       uuid REFERENCES memory_cards(id),
    status        text NOT NULL DEFAULT 'proposed' CHECK (status IN
                    ('proposed','active','superseded','retracted')),
    acl           jsonb NOT NULL,
    created_by    text NOT NULL
);
CREATE INDEX facts_key ON facts(tenant_id, namespace_id, fact_key, valid_from);

-- Fact citations: same shape as memory_card_sources with fact_id (spec §6.6).
CREATE TABLE fact_sources (
    fact_id        uuid NOT NULL REFERENCES facts(id) ON DELETE CASCADE,
    raw_id         uuid REFERENCES raw_evidence(id),
    chunk_id       uuid REFERENCES chunks(id),
    source_uri     text NOT NULL,
    source_version text,
    path           text,
    line_start     int,
    line_end       int,
    quote          text,
    support_type   text NOT NULL DEFAULT 'supports' CHECK (support_type IN
                     ('supports','contradicts','supersedes','validates'))
);
CREATE UNIQUE INDEX fact_sources_uniq ON fact_sources
    (fact_id, source_uri, coalesce(line_start,-1), coalesce(line_end,-1));
CREATE INDEX fact_sources_fact ON fact_sources(fact_id);

-- +goose Down
DROP TABLE IF EXISTS fact_sources;
DROP TABLE IF EXISTS facts;
