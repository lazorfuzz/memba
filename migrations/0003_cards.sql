-- +goose Up

-- Memory cards (spec §6.4). Proposals are cards with status='proposed'.
CREATE TABLE memory_cards (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        text NOT NULL,
    namespace_id     text NOT NULL REFERENCES namespaces(id),
    card_type        text NOT NULL CHECK (card_type IN
                       ('fact_note','convention','warning','gotcha','procedure','skill',
                        'design_decision','ownership','environment_note','debugging_hint')),
    title            text NOT NULL,
    body             text NOT NULL,
    structured       jsonb NOT NULL DEFAULT '{}',
    subject          text,
    tags             text[] NOT NULL DEFAULT '{}',
    entities         text[] NOT NULL DEFAULT '{}',
    status           text NOT NULL DEFAULT 'proposed' CHECK (status IN
                       ('proposed','active','stale','deprecated','invalidated','quarantined','dormant')),
    confidence       double precision NOT NULL DEFAULT 0.5,
    importance       double precision NOT NULL DEFAULT 0.5,
    valid_from       timestamptz,
    valid_to         timestamptz,
    ttl_class        text NOT NULL DEFAULT 'code' CHECK (ttl_class IN ('code','operational','design')),
    last_verified_at timestamptz,
    verify_by        timestamptz,
    last_retrieved_at timestamptz,
    created_from     text NOT NULL CHECK (created_from IN
                       ('human','extractor','agent_run','consolidation','bootstrap','benchmark_seed')),
    created_by       text NOT NULL,
    embedding        halfvec(1024),
    tsv              tsvector GENERATED ALWAYS AS
                       (to_tsvector('simple', title || ' ' || body)) STORED,
    acl              jsonb NOT NULL,
    metadata         jsonb NOT NULL DEFAULT '{}',
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX cards_scope_status ON memory_cards(tenant_id, namespace_id, status);
CREATE INDEX cards_tsv  ON memory_cards USING gin(tsv);
CREATE INDEX cards_vec  ON memory_cards USING hnsw (embedding halfvec_cosine_ops);
CREATE INDEX cards_verify_due ON memory_cards(verify_by) WHERE status = 'active';

-- Citations (spec §6.5). The spec's PK uses coalesce() expressions, which
-- Postgres cannot put in a PRIMARY KEY; a unique expression index enforces
-- the same constraint.
CREATE TABLE memory_card_sources (
    card_id        uuid NOT NULL REFERENCES memory_cards(id) ON DELETE CASCADE,
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
CREATE UNIQUE INDEX card_sources_uniq ON memory_card_sources
    (card_id, source_uri, coalesce(line_start,-1), coalesce(line_end,-1));
CREATE INDEX card_sources_card ON memory_card_sources(card_id);
CREATE INDEX card_sources_raw  ON memory_card_sources(raw_id);

CREATE TABLE memory_card_links (
    src_card_id uuid NOT NULL REFERENCES memory_cards(id) ON DELETE CASCADE,
    dst_card_id uuid NOT NULL REFERENCES memory_cards(id) ON DELETE CASCADE,
    link_type   text NOT NULL CHECK (link_type IN
                  ('related_to','supersedes','caused_by','prerequisite_for',
                   'warns_about','validates','contradicts')),
    weight      double precision NOT NULL DEFAULT 1.0,
    created_by  text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (src_card_id, dst_card_id, link_type)
);

-- Reviews (spec §6.8)
CREATE TABLE card_reviews (
    card_id   uuid NOT NULL REFERENCES memory_cards(id) ON DELETE CASCADE,
    reviewer  text NOT NULL,
    decision  text NOT NULL CHECK (decision IN ('approve','reject','invalidate','edit')),
    reason    text,
    edited_body text,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (card_id, reviewer, created_at)
);

-- +goose Down
DROP TABLE IF EXISTS card_reviews;
DROP TABLE IF EXISTS memory_card_links;
DROP TABLE IF EXISTS memory_card_sources;
DROP TABLE IF EXISTS memory_cards;
