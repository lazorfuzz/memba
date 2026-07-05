# memba — Institutional Memory Kernel

## v3 Implementation Specification

Status: approved for implementation
Date: 2026-07-04
Audience: an implementing engineer or coding agent with **no prior context**. This document is self-contained; you do not need the v1 or v2 planning documents. A v2→v3 changelog is included at the end (§23) purely for the record.

---

## 0. How to read this document

memba is an institutional memory system for long-running AI coding agents operating inside an engineering organization. It ingests the organization's evidence (code, PRs, docs, CI runs, incidents, chat, prior agent runs), distills it into verified, citable memory, and serves that memory back to agents in the form of compact context blocks or mounted file workspaces.

Build order: read §1–§5 for the mental model, then implement in the phase order of §21. The normative sections are §6 (data model), §7 (agent tools), §8–§12 (read/write/verify/consolidate/workspace), §13 (API), and §15 (security). §17 (benchmarking) is not optional — the system is benchmark-first and the harness is built in Phase 0.

Conventions used below:

- `MUST` / `MUST NOT` are hard requirements. `SHOULD` is a strong default. `MAY` is optional.
- All defaults (token budgets, thresholds, TTLs) live in one config file (§4.4) and are tunable; the numbers given are shipping defaults.
- "Verified" in the references means the source was confirmed to exist and say what we claim during the July 2026 research pass. "Self-reported" means the numbers come from the system's own authors and were not independently reproduced.

### Glossary

| Term | Meaning |
|---|---|
| Raw evidence | A verbatim, immutable copy of a source artifact (file, PR, doc, chat thread, CI log, agent-run log). |
| Chunk | An indexed slice of raw evidence (embedded + full-text indexed). |
| Memory card | An atomic, human/agent-readable institutional note with citations, a lifecycle status, and a verification clock. The main derived memory unit. |
| Fact | A machine-queryable `(subject, predicate, object)` triple with bitemporal validity. |
| Verification | A recorded check that a memory still holds (against a branch, a source, a command run, or a human). |
| Proposal | A memory card in `proposed` status. Agents create proposals; only the service promotes them. |
| Evidence pack | The structured response of a query: profile + cards + facts + conflicts + missing-evidence notes. |
| Workspace | An evidence pack exported as a navigable file tree that a coding agent can `ls`, `grep`, and read. |
| Namespace | A hierarchical scope path, e.g. `/acme/platform/payments/repos/billing-api`. |
| Principal | The authenticated caller: `user:jdoe`, `agent:payments-coder-17`, `team:payments`. |
| memd | The single Go service that implements all of the above. |

---

## 1. Problem and design thesis

Long-running coding agents fail in organizations for a predictable reason: the knowledge they need is scattered, stale, permissioned, and contradictory. A retrieval layer that treats this as "RAG over a database" produces confident answers built on stale docs, chat rumors, and other agents' failed experiments.

**Thesis:** the memory system should be a small, strict, *verifiable institutional evidence engine* with a file-native interface — not an autonomous memory brain, and not a vector index with a REST wrapper.

```text
Institutional memory =
    raw verbatim evidence
  + validated, cited memory cards
  + bitemporal facts
  + branch-verified code claims
  + navigable evidence workspaces
  + recorded memory actions (so memory behavior itself can be measured and improved)
```

Three findings from the 2025–2026 literature and industry practice shape the design (details and citations in §3):

1. **Agentic file exploration beats one-shot retrieval on hard, long-horizon memory tasks.** The strongest published method on the hardest current memory benchmark stores histories on disk and lets a coding-agent-style reader gather evidence from a sandboxed file workspace. memba therefore makes the mounted workspace the *default* high-accuracy interface for coding agents, and differentiates by pre-curating that workspace with verified cards and structured facts rather than raw logs.
2. **Unverified memory is worse than no memory.** Append-only, open-gate memory stores accumulate "hallucinations of the past," and are an active attack surface (memory-injection attacks are demonstrated in the literature). memba therefore gates all writes through proposal → validation → promotion, verifies code claims against the target branch just-in-time, and puts every active memory on a decay clock.
3. **Long context does not replace memory.** Million-token contexts degrade on distractor-heavy inputs ("context rot"), cost linearly per task, and provide no permissions, verification, or curation. memba treats long context and prompt caching as delivery mechanisms to exploit (stable L0/L1 blocks), not as competitors.

**Non-goals:** memba is not a chat-personalization memory, not a general knowledge base UI for humans (though humans can review it), and not an agent framework. It is a memory kernel with a small API.

---

## 2. Invariants

These seven invariants are load-bearing. Any change that violates one requires a design review, not a code review.

```text
I1. Raw evidence is immutable and verbatim. Derived memory may be edited,
    superseded, or invalidated; the underlying evidence never is.

I2. Every durable derived memory (card, fact, procedure) cites raw evidence.
    A memory without citations cannot leave `proposed` status.

I3. Every code-related memory is verified against the target repo/branch
    before it is served for a coding change, or it is served with an
    explicit staleness flag.

I4. Agents and extractors PROPOSE memory. Only memd, applying deterministic
    promotion rules (§10.6), PROMOTES memory to active.

I5. Benchmarks exercise the same public API as production. No benchmark-only
    read or write paths.

I6. Every active memory has a verification clock (verify_by). Memory that
    cannot be re-verified within its TTL degrades to `stale` and leaves
    default recall. Nothing stays trusted forever by default.

I7. Retrieved evidence is data, not instructions. Content originating from
    low-authority or agent-generated sources is never compiled into the
    always-on context layers (L0/L1) and is visibly labeled in workspaces.
    (Defense against memory-poisoning; see §15.4.)
```

I6 and I7 are new relative to earlier plan iterations; they encode the decay-clock and poisoning-defense findings from the research pass.

---

## 3. Evidence base and prior art

This section records *why* the design looks the way it does, so future maintainers do not re-litigate settled questions without new evidence. Full citations in §24.

### 3.1 What the strongest current systems do

- **LongMemEval-V2 / AgentRunbook-C (2026, paper-reported SOTA ≈ 72.5%).** The benchmark reframes deep memory as a *context-gathering* problem over long agentic trajectories. Its strongest method stores trajectories on disk and lets a coding-agent-style reader explore a sandboxed file workspace to gather evidence, beating one-shot top-k retrieval pipelines. → Validates memba's workspace-as-default decision (L4, §5) and the benchmark-first Insert/Query framing (§17). memba's beat-SOTA hypothesis: a *curated* workspace (verified cards + facts + labeled raw spans + a `missing_evidence.md`) plus the ability to issue further `mem.search` calls from inside the workspace should outperform exploration over raw trajectories on both accuracy and token cost (§17.5).
- **GitHub Copilot's shipped memory system (verified engineering blog).** Stores repository-level facts with citations into code, scopes them by repo permissions, verifies memories against the current branch just-in-time before use, discards memories the code no longer substantiates, and expires unused/unverified memories on a rolling window measured in weeks. → Independently converges on memba's branch verification (§9), citation requirement (I2), and motivates the explicit decay clock (I6, §10.7) with a 28-day default TTL for code-scoped memory.
- **Zep/Graphiti (self-reported).** Temporal knowledge graph with bitemporal edge validity (event time vs. ingestion time), edge invalidation on contradiction, self-reported strong LongMemEval results. → Validates bitemporal facts (§6.6) and first-class supersession/invalidation, *without* requiring a graph database: memba's fact/edge volumes and 1–2-hop expansion needs are served by Postgres recursive CTEs (§19 gives the escape hatch).
- **Letta/MemGPT + sleep-time compute (verified).** OS-style memory hierarchy (small always-in-context core memory + paged archival memory) and the demonstrated value of *offline* consolidation work done between interactions. → Validates the L0–L4 layering (§5) and motivates memba's nightly consolidation jobs (§11).
- **Mem0 (self-reported), A-MEM, HippoRAG 1/2 (verified papers).** Extraction-into-atomic-memories with update/merge operations; Zettelkasten-style typed note linking; graph-assisted associative retrieval for multi-hop questions. → memba adopts atomic cards with typed links used for *expansion only* — graph links route search, they are never the source of truth (I1/I2).
- **Product memories in coding tools (Cursor Memories, Windsurf/Codeium Memories, Devin Knowledge, Claude Code CLAUDE.md/memory files — product docs).** All converge on: small curated always-loaded context per repo, plus user/agent-proposed knowledge items gated by review. → Validates L1 repo profiles and the proposal-review loop; memba's differentiator is verification and citations, which these products largely lack.

### 3.2 What the literature warns against

- **Memory contamination / open-gate writes.** Studies of self-generated memory show semantic drift and error compounding when agents write conclusions from their own (possibly failed) runs directly into long-term memory. → Proposal gating (I4), source-authority ranking (§8.6), and the rule that failed-run reflections can never auto-promote (§10.6).
- **Memory poisoning attacks.** MINJA (memory-injection via crafted interactions) and AgentPoison (backdooring RAG/memory stores so specific triggers retrieve malicious records) demonstrate practical attacks on exactly this class of system. → §15.4 defenses: provenance-gated promotion, injection scanning at ingest, quarantine, L0/L1 compiled only from validated cards, per-source caps in retrieval, and "evidence is data" framing (I7).
- **Context rot.** Long-context models degrade non-uniformly as input length and distractor density grow (NoLiMa; Chroma's context-rot report). → Justifies curation + budgets rather than "dump everything in the window," and keeps L0/L1 small and cache-stable (§18.2).
- **Benchmark quality.** LoCoMo has documented annotation-quality problems and is treated as a secondary, sanity-check benchmark only. Several vendor-reported LongMemEval/LoCoMo numbers are self-reported and have public reproduction disputes; memba's targets are therefore stated against paper-reported numbers with private-split contamination controls (§17.6).

### 3.3 Citation hygiene note (carried from the research pass)

One source cited in earlier planning iterations ("MemPalace", a memory system with an "AAAK" compression dialect) **could not be verified to exist** during the July 2026 research pass and MUST NOT be cited in memba materials. The useful ideas previously attributed to it (verbatim raw storage, small always-on layers over deeper search) are retained but re-grounded in the verified sources above. The AutoMem repository (memory operations as first-class file-system actions; scaffold-optimization outer loops) was verified to exist; its benchmark gains are self-reported and drawn from game environments, so memba borrows its *instrumentation* idea (memory-action logging, §6.9) but grounds scaffold optimization in the reflective prompt-optimization literature (GEPA, DSPy) instead (§17.7).

---

## 4. System architecture

### 4.1 Component overview

The operational footprint is deliberately boring and MUST stay this small until the escape-hatch thresholds in §19 are hit:

```text
One Go service        memd        (API, ingest, retrieval, verification,
                                   consolidation jobs, workspace export)
One database          PostgreSQL 16+ with pgvector ≥ 0.8 and pg_trgm
One object store      S3-compatible (MinIO in dev)   large blobs, workspaces
One workspace format  .memworkspace/                 file-tree export (§12)
One public API        /v1  Insert / Query / Verify / Propose / Actions (§13)
```

```text
                    ┌─────────────────────────────────────┐
                    │            Company sources           │
                    │ Git · GitHub PRs · Docs · Slack · CI │
                    │ Incidents · Tickets · Agent runs     │
                    └───────────────────┬─────────────────┘
                                        │ connectors (§14.4)
                                        ▼
┌───────────────────────────────────────────────────────────────────────┐
│                         memd (single Go binary)                        │
│                                                                       │
│  Ingest ──► Raw evidence (Postgres + object store)  [immutable, I1]    │
│    │            │                                                     │
│    │            ├─► Chunk + index (FTS, trigram, HNSW vectors,        │
│    │            │                  code symbols)                      │
│    │            └─► Extractors ──► PROPOSED cards / facts             │
│    │                                   │                              │
│    │                    Promotion rules (§10.6) ──► ACTIVE memory     │
│    │                                   ▲                              │
│  Agents ── mem.propose / mem.log ──────┘                              │
│                                                                       │
│  Query ─► authz ─► hybrid retrieval (RRF) ─► cross-encoder rerank      │
│        ─► deterministic gates (ACL, branch verification, temporal)    │
│        ─► evidence pack ─► optional L4 workspace export (object store)│
│                                                                       │
│  Background: verification sweeps, decay clock, sleep-time             │
│              consolidation (river job queue in Postgres)              │
└───────────────────────────────────────────────────────────────────────┘
                                        │
                                        ▼
                        Long-running coding agents (mem.* tools, §7)
```

Explicitly **not** in the stack at v3: Kafka, Elasticsearch/OpenSearch, Neo4j or any graph DB, a separate vector DB, Redis (Postgres does queueing and caching), microservices. Each has a defined escape hatch behind a Go interface (§19) so adding one later is an adapter, not a rewrite.

### 4.2 Deployment

- Single container image `memd` + Postgres + MinIO via `docker-compose.yml` for dev; any orchestrator in prod. memd is stateless (all state in Postgres/object store) and horizontally scalable; background jobs use `river` (Postgres-backed queue with `FOR UPDATE SKIP LOCKED`) so multiple replicas coordinate safely.
- Reference production sizing for the design-point load (§18.1): Postgres 8 vCPU / 64 GB / NVMe (vectors are memory-hungry), 2× memd replicas, object store unbounded.
- Model dependencies (embedder, reranker, extractor LLM) are external services behind interfaces (§14.3); memd itself runs no models in-process by default.

### 4.3 Multi-tenancy and namespaces

- `tenant_id` is a hard partition key on every table and every query. Cross-tenant reads are structurally impossible (enforced by a mandatory `tenant_id` predicate helper in the store layer + property tests, §14.6).
- Namespaces are hierarchical paths within a tenant: `/acme/platform/payments/repos/billing-api`. Scope resolution walks ancestors: a query scoped to a repo also sees team- and org-level memory attached to ancestor namespaces. Promotion of a card to an *ancestor* namespace (org-wide knowledge) requires stronger evidence than repo-local promotion (§10.6).

### 4.4 Configuration (single file, hot-reloadable where safe)

```yaml
# configs/memd.yaml — shipping defaults
server:   { http_addr: ":8080", max_body_mb: 32 }
postgres: { dsn_env: MEMD_PG_DSN, max_conns: 32 }
objstore: { endpoint_env: MEMD_S3_ENDPOINT, bucket: memba }

models:
  embedder:   { provider: voyage, model: voyage-code-3, dim: 1024, output: halfvec }
  reranker:   { provider: local, model: bge-reranker-v2-m3, max_pairs: 100 }
  extractor:  { provider: anthropic, model: claude-sonnet-latest }   # ingest-time card extraction
  judge:      { provider: anthropic, model: claude-sonnet-latest }   # benchmark grading only

retrieval:
  per_retriever_n: 50        # candidates from each retriever before fusion
  rrf_k: 60
  fused_pool: 150
  rerank_top: 100
  budgets: { boot: 700, scoped: 4000, deep: 12000, workspace_files: 64000 }

lifecycle:
  ttl_days: { code: 28, operational: 90, design: 180 }   # verify_by clocks (§10.7)
  dormant_after_ttl_multiple: 2
  dedup_cosine: 0.92

promotion:                      # §10.6
  min_sources_for_multi_source_rule: 2
  chat_only_can_promote: false

security:
  injection_scan: { enabled: true, action: quarantine }
  secret_scan:    { ruleset: gitleaks-default, entropy_threshold: 4.5 }
  workspace_ttl_hours: 72
```

Every query logs `config_hash = sha256(effective retrieval+model config)` for reproducibility (I5).

---

## 5. The memory stack (L0–L4)

Five layers, from always-loaded to on-demand. Token budgets assume a modern frontier model with prompt caching; L0/L1 are engineered for **cache stability** (byte-identical serialization between promotions) so their effective cost per call approaches zero (§18.2).

### L0 — Agent boot context (≤ 700 tokens, always included)

Compiled by memd, never free-text from sources (I7). Contents: memory rules of engagement, the principal and its permissions summary, target repo/branch, the `mem.*` tool cheat-sheet, and citation/verification requirements ("never act on a memory flagged stale without re-verifying"). Served by `GET /v1/profile?level=0` with an ETag; changes only when policy or principal scope changes.

### L1 — Repo/service operating profile (≤ 2,000 tokens, cached)

A generated document per (namespace, repo): purpose, owners, build/test/deploy commands, generated-code rules, top 5 gotchas, current architectural constraints — every line backed by an `active` card and rendered with `[card:<id>]` markers. Regenerated by the nightly consolidation job (§11) or on promotion events touching profile-tagged cards; byte-stable otherwise. This is the memba equivalent of a perfect, always-fresh CLAUDE.md, except every claim is cited and on a verification clock.

### L2 — Scoped memory cards (on demand, `scoped` mode)

Atomic cards for the current repo/service/team/incident family, e.g. "Do not edit generated `invoice_status.pb.go`; edit the proto and run `make proto`." Returned as an evidence pack within a 4k default budget.

### L3 — Deep hybrid retrieval (on demand, `deep` mode)

The full pipeline of §8: multi-retriever candidate generation → RRF fusion → cross-encoder rerank → deterministic gates (ACL, branch verification, temporal conflict resolution) → packed evidence.

### L4 — Mounted evidence workspace (the default for coding tasks)

L3's output exported as a file tree (§12) the agent can navigate with its native file tools, including labeled raw evidence spans, structured fact files, a `missing_evidence.md`, and a writable `scratch/`. **L4 is the default mode for any `goal_type: coding_change` or long-horizon task**; `scoped` is for quick lookups; `boot` is for session start. Rationale: §3.1 — agentic gathering over a curated file surface is the current SOTA pattern, and coding agents are already optimized to read files.

Mode selection is the caller's, but memd's tool descriptions (§7) steer agents: quick factual question → `mem.search`; anything that will end in a diff → `mem.workspace`.

---

## 6. Data model (PostgreSQL DDL)

Normative schema. Notes: statuses/enums are `text` + `CHECK` constraints (cheaper migrations than PG enums). All tables carry `tenant_id`; LIST-partition `raw_evidence` and `chunks` by `tenant_id` once any tenant exceeds ~20M chunks (§19). Timestamps are `timestamptz`. Extensions required: `vector`, `pg_trgm`, `pgcrypto` (for `gen_random_uuid()`).

### 6.1 Namespaces

```sql
CREATE TABLE namespaces (
    id          text PRIMARY KEY,          -- '/acme/platform/payments/repos/billing-api'
    tenant_id   text NOT NULL,
    parent_id   text REFERENCES namespaces(id),
    kind        text NOT NULL CHECK (kind IN ('org','team','service','repo','incident_family')),
    policy      jsonb NOT NULL DEFAULT '{}',  -- promotion overrides, trusted sources, TTL overrides
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ns_tenant ON namespaces(tenant_id);
```

### 6.2 Raw evidence (immutable, I1)

```sql
CREATE TABLE raw_evidence (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          text NOT NULL,
    namespace_id       text NOT NULL REFERENCES namespaces(id),
    source_type        text NOT NULL CHECK (source_type IN
                         ('git','github_pr','doc','slack','ci','incident','ticket','agent_run','manual')),
    source_uri         text NOT NULL,            -- 'git://billing-api/src/invoice/status.go'
    source_external_id text,
    source_version     text,                     -- commit sha, doc revision, message ts
    content_hash       text NOT NULL,            -- sha256(body)
    title              text,
    body               text NOT NULL,            -- verbatim; bodies > 256 KiB go to object store,
    body_object_key    text,                     --   body holds a truncated head + this key is set
    metadata           jsonb NOT NULL DEFAULT '{}',   -- repo, branch, language, author, event ids
    acl                jsonb NOT NULL,           -- {"read":["team:payments"],"write":["svc:ingest"]}
    event_time         timestamptz,              -- when it happened in the world
    ingested_at        timestamptz NOT NULL DEFAULT now(),
    -- security (§15)
    quarantined        bool NOT NULL DEFAULT false,
    quarantine_reason  text,                     -- 'secret','injection_suspect','policy'
    embedding_forbidden bool NOT NULL DEFAULT false,   -- secret-bearing: never embed
    UNIQUE (tenant_id, source_uri, source_version, content_hash)   -- ingest idempotency
);
CREATE INDEX raw_ns_time ON raw_evidence(tenant_id, namespace_id, event_time);
```

There is no `UPDATE` path for `body`; corrections arrive as new rows with new `source_version`.

### 6.3 Chunks (the retrieval substrate)

```sql
CREATE TABLE chunks (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       text NOT NULL,
    namespace_id    text NOT NULL,
    raw_id          uuid NOT NULL REFERENCES raw_evidence(id),
    ordinal         int  NOT NULL,
    kind            text NOT NULL CHECK (kind IN ('code','prose','chat','diff','log','config')),
    path            text,                    -- file path for code/docs
    line_start      int, line_end int,
    token_count     int  NOT NULL,
    body            text NOT NULL,
    tsv             tsvector GENERATED ALWAYS AS
                      (to_tsvector('simple', coalesce(path,'') || ' ' || body)) STORED,
    ident_text      text,                    -- space-joined identifiers/symbols for trigram search
    symbols         text[] NOT NULL DEFAULT '{}',   -- fully-qualified defs/refs in this chunk
    embedding       halfvec(1024),
    embedding_model text,
    acl             jsonb NOT NULL,          -- denormalized from raw_evidence at write time
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
```

pgvector notes (normative): use `halfvec` (fp16) to halve memory; set `hnsw.ef_search = 80` default and enable **iterative index scans** (`SET hnsw.iterative_scan = relaxed_order`) so ACL/namespace-filtered vector queries keep recall — this is the pgvector ≥ 0.8 fix for the filtered-HNSW recall problem. For any single namespace > 3M chunks, add a partial HNSW index on that namespace.

### 6.4 Memory cards (the main derived unit)

v3 collapses two earlier concepts into this table: *proposals are cards with `status='proposed'`*, and *procedures/gotchas/skills are card types* with a type-specific `structured` payload — one lifecycle, one index, one review surface.

```sql
CREATE TABLE memory_cards (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        text NOT NULL,
    namespace_id     text NOT NULL REFERENCES namespaces(id),
    card_type        text NOT NULL CHECK (card_type IN
                       ('fact_note','convention','warning','gotcha','procedure','skill',
                        'design_decision','ownership','environment_note','debugging_hint')),
    title            text NOT NULL,
    body             text NOT NULL,               -- ≤ ~200 tokens, imperative, self-contained
    structured       jsonb NOT NULL DEFAULT '{}', -- per-type schema, §6.4.1
    subject          text,                        -- primary entity: repo, service, file, symbol
    tags             text[] NOT NULL DEFAULT '{}',
    entities         text[] NOT NULL DEFAULT '{}',
    status           text NOT NULL DEFAULT 'proposed' CHECK (status IN
                       ('proposed','active','stale','deprecated','invalidated','quarantined','dormant')),
    confidence       double precision NOT NULL DEFAULT 0.5,
    importance       double precision NOT NULL DEFAULT 0.5,
    valid_from       timestamptz,
    valid_to         timestamptz,
    -- verification clock (I6)
    ttl_class        text NOT NULL DEFAULT 'code' CHECK (ttl_class IN ('code','operational','design')),
    last_verified_at timestamptz,
    verify_by        timestamptz,                 -- last_verified_at + ttl(ttl_class)
    last_retrieved_at timestamptz,
    -- provenance
    created_from     text NOT NULL CHECK (created_from IN
                       ('human','extractor','agent_run','consolidation','bootstrap','benchmark_seed')),
    created_by       text NOT NULL,               -- principal
    -- retrieval
    embedding        halfvec(1024),
    tsv              tsvector GENERATED ALWAYS AS
                       (to_tsvector('simple', title || ' ' || body)) STORED,
    acl              jsonb NOT NULL,              -- computed: ∩ of source ACLs (§15.2)
    metadata         jsonb NOT NULL DEFAULT '{}',
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX cards_scope_status ON memory_cards(tenant_id, namespace_id, status);
CREATE INDEX cards_tsv  ON memory_cards USING gin(tsv);
CREATE INDEX cards_vec  ON memory_cards USING hnsw (embedding halfvec_cosine_ops);
CREATE INDEX cards_verify_due ON memory_cards(verify_by) WHERE status = 'active';
```

Status semantics: `proposed` (awaiting promotion), `active` (in default recall), `stale` (verification clock expired or check failed — excluded from `boot/scoped`, served flagged in `deep`), `deprecated` (superseded, kept for history), `invalidated` (shown false), `quarantined` (security hold, never served), `dormant` (2×TTL with no retrieval and no verification — excluded everywhere, retained in DB).

#### 6.4.1 `structured` payload schemas by card_type

```jsonc
// procedure
{ "steps": ["edit invoice_status.proto", "run `make proto`", "run `make test`"],
  "preconditions": ["clean working tree"], "verify_command": "make test" }
// gotcha
{ "trigger": "editing files matching *_pb.go", "severity": "high",
  "consequence": "changes overwritten by codegen" }
// ownership
{ "owner": "team:payments", "escalation": "user:jdoe", "source_of_truth": "CODEOWNERS" }
// skill
{ "applies_when": "...", "recipe": "...", "observed_success_rate": 0.9 }
```

memd validates `structured` against these JSON Schemas at write time.

### 6.5 Card citations and links

```sql
CREATE TABLE memory_card_sources (
    card_id        uuid NOT NULL REFERENCES memory_cards(id),
    raw_id         uuid NOT NULL REFERENCES raw_evidence(id),
    chunk_id       uuid REFERENCES chunks(id),
    source_uri     text NOT NULL,
    source_version text,
    path           text, line_start int, line_end int,
    quote          text,                         -- exact supporting excerpt (for re-verification)
    support_type   text NOT NULL CHECK (support_type IN
                     ('supports','contradicts','supersedes','validates')),
    PRIMARY KEY (card_id, raw_id, source_uri, coalesce(line_start,-1), coalesce(line_end,-1))
);

CREATE TABLE memory_card_links (
    src_card_id uuid NOT NULL REFERENCES memory_cards(id),
    dst_card_id uuid NOT NULL REFERENCES memory_cards(id),
    link_type   text NOT NULL CHECK (link_type IN
                  ('related_to','supersedes','caused_by','prerequisite_for',
                   'warns_about','validates','contradicts')),
    weight      double precision NOT NULL DEFAULT 1.0,
    created_by  text NOT NULL,                   -- 'consolidation' | principal
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (src_card_id, dst_card_id, link_type)
);
```

Links are used for 1–2-hop retrieval expansion via recursive CTE only (never as truth, §3.1).

### 6.6 Bitemporal facts (machine reasoning)

```sql
CREATE TABLE facts (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     text NOT NULL,
    namespace_id  text NOT NULL REFERENCES namespaces(id),
    subject       text NOT NULL,     -- 'billing-api'
    predicate     text NOT NULL,     -- 'build_command'
    object        text NOT NULL,     -- 'make test'
    object_type   text NOT NULL DEFAULT 'string',
    fact_key      text GENERATED ALWAYS AS (subject || '|' || predicate) STORED,
    -- valid time (world) and transaction time (system): bitemporal
    valid_from    timestamptz NOT NULL,
    valid_to      timestamptz,                    -- null = currently valid
    asserted_at   timestamptz NOT NULL DEFAULT now(),
    retracted_at  timestamptz,                    -- null = system still believes it
    supersedes    uuid REFERENCES facts(id),
    card_id       uuid REFERENCES memory_cards(id),  -- optional owning card
    status        text NOT NULL DEFAULT 'proposed' CHECK (status IN
                    ('proposed','active','superseded','retracted')),
    acl           jsonb NOT NULL,
    created_by    text NOT NULL
);
CREATE INDEX facts_key ON facts(tenant_id, namespace_id, fact_key, valid_from);
CREATE TABLE fact_sources (LIKE memory_card_sources INCLUDING ALL);  -- same shape, fact_id column
ALTER TABLE fact_sources RENAME COLUMN card_id TO fact_id;
```

Current-state query: `valid_to IS NULL AND retracted_at IS NULL AND status='active'`. Point-in-time ("what did we believe on May 1?"): filter on `asserted_at ≤ t < coalesce(retracted_at, 'infinity')`. Conflict = two active facts sharing `fact_key` with overlapping validity and different `object` (§8.7).

### 6.7 Verifications

```sql
CREATE TABLE verifications (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         text NOT NULL,
    namespace_id      text NOT NULL,
    target_type       text NOT NULL CHECK (target_type IN ('card','fact')),
    target_id         uuid NOT NULL,
    verification_type text NOT NULL CHECK (verification_type IN
                        ('code_branch_check','test_passed','command_succeeded',
                         'source_still_exists','human_review','multi_source_support')),
    repo text, branch text, commit_sha text, head_sha text,
    result            text NOT NULL CHECK (result IN ('passed','failed','inconclusive')),
    detail            jsonb NOT NULL DEFAULT '{}',   -- per-citation outcomes (§9)
    verified_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX verif_target ON verifications(target_id, verified_at DESC);
CREATE INDEX verif_cache  ON verifications(target_id, repo, head_sha);  -- JIT cache key (§9.4)
```

### 6.8 Reviews (human/agent decisions on proposals)

```sql
CREATE TABLE card_reviews (
    card_id   uuid NOT NULL REFERENCES memory_cards(id),
    reviewer  text NOT NULL,            -- principal
    decision  text NOT NULL CHECK (decision IN ('approve','reject','invalidate','edit')),
    reason    text,
    edited_body text,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (card_id, reviewer, created_at)
);
```

### 6.9 Memory actions (instrumentation for behavior benchmarking)

```sql
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
```

### 6.10 Workspaces and jobs

```sql
CREATE TABLE workspaces (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    text NOT NULL,
    namespace_id text NOT NULL,
    principal    text NOT NULL,
    query        text NOT NULL,
    mode         text NOT NULL,
    manifest     jsonb NOT NULL,        -- §12.2, includes ACL decision log
    object_key   text NOT NULL,         -- tar.zst in object store
    token_total  int NOT NULL,
    expires_at   timestamptz NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);
-- Background jobs: river's schema (river_job) via its migrations. Job kinds:
-- ingest_chunk, embed, extract_cards, verify_sweep, decay_sweep, consolidate_ns,
-- recompute_acl, reembed_model_migration, workspace_gc.
```


---

## 7. Agent-facing memory tools

Seven tools, no more. These are the only memory verbs an agent needs; keeping the vocabulary tiny is itself a design feature (small tool sets are easier for agents to use correctly and easier to benchmark). Tool descriptions below are the exact text to ship in agent scaffolds — they steer mode selection (§5).

### 7.1 Tool schemas

```jsonc
// mem.search — quick lookup, no workspace. Use for factual questions.
{ "name": "mem.search",
  "input": { "query": "string", "namespace_hints": ["string"], "repo": "string?",
             "branch": "string?", "mode": "scoped|deep", "max_tokens": "int?" },
  "returns": "EvidencePack (§8.8)" }

// mem.workspace — DEFAULT for any task that will end in a code change or
// spans multiple steps. Mounts a curated evidence file tree you can read/grep.
{ "name": "mem.workspace",
  "input": { "query": "string (your goal, verbatim)", "repo": "string",
             "branch": "string", "namespace_hints": ["string"], "max_tokens": "int?" },
  "returns": { "workspace_id": "uuid", "path_or_uri": "string", "manifest_summary": "..." } }

// mem.open — fetch one item in full (card, fact, raw evidence, source span).
{ "name": "mem.open",
  "input": { "ref": "card:<id> | fact:<id> | raw:<id> | git://... (#L10-40)" },
  "returns": "full item + citations + verification history" }

// mem.log — record an observation from this run (cheap, always allowed).
// Logs are raw evidence of source_type=agent_run; they are NOT memory yet.
{ "name": "mem.log",
  "input": { "run_id": "uuid", "text": "string", "refs": ["string"], "outcome": "string?" } }

// mem.propose — propose a durable memory (card). Requires citations (I2).
{ "name": "mem.propose",
  "input": { "run_id": "uuid", "card_type": "gotcha|procedure|convention|...",
             "title": "string", "body": "string", "structured": "object?",
             "source_refs": [{ "source_uri": "string", "source_version": "string?",
                                "line_start": "int?", "line_end": "int?", "quote": "string?" }] },
  "returns": { "card_id": "uuid", "status": "proposed", "promotion_hint": "what would promote it" } }

// mem.verify — ask memd to re-verify a memory against branch/source/command NOW.
{ "name": "mem.verify",
  "input": { "target": "card:<id>|fact:<id>", "verification_type": "code_branch_check|...",
             "repo": "string?", "branch": "string?" },
  "returns": "VerificationResult (§9)" }

// mem.invalidate — flag a memory as probably wrong/stale, with evidence.
{ "name": "mem.invalidate",
  "input": { "target": "card:<id>|fact:<id>", "reason": "string",
             "evidence_refs": ["string"] },
  "returns": { "status": "recorded", "effect": "counter-evidence logged; card re-verification queued" } }
```

Every tool call is recorded in `memory_actions` (§6.9) automatically by memd — agents do not log their own memory actions.

### 7.2 Canonical agent flow (ship this in the L0 rules)

```text
1. Task arrives → mem.workspace(repo, branch, goal).
2. Read README.md, gotchas.md, procedures.md first; then targeted files.
3. Need more? mem.search from inside the task (workspace tells you what's missing).
4. Before acting on any item marked verified:false or stale:true → mem.verify it.
5. During work: mem.log observations worth remembering (failures especially).
6. After success: mem.propose durable lessons WITH source_refs (diff, CI run, doc).
7. Never mem.propose from a failed run as if it were established fact —
   log it; the consolidation job mines failure patterns with proper gating.
```

---

## 8. Read path

### 8.1 Query modes

| Mode | Use | Budget (default) | Contents |
|---|---|---|---|
| `boot` | session start | 700 | L0 (+L1 if repo known) |
| `scoped` | quick lookup | 4,000 | L1 excerpt + top cards/facts/gotchas |
| `deep` | hard question | 12,000 | full pipeline evidence pack |
| `workspace` | coding/long-horizon (default) | 64,000 across files | deep pipeline + L4 export |
| `audit` | explain a memory | n/a | provenance + verification chain for one item |
| `benchmark` | eval harness | per config | deterministic: pinned config, temp 0, logged prompts/model IDs |

### 8.2 Pipeline (normative)

```text
Query(q):
 1. Authenticate; resolve principal → permitted ACL subjects.
 2. Resolve namespace scope: hints + ancestors (§4.3).
 3. Analyze query (cheap, no LLM): extract symbols (CamelCase/snake_case tokens,
    path-like strings, error codes, TICKET-123 patterns), detect language,
    detect temporal qualifiers ("as of", "before the migration").
 4. Candidate generation — run in parallel, each returns top per_retriever_n=50
    *post ACL/namespace/quarantine filter* (filters are pushed into SQL):
      R1 lexical    : FTS websearch_to_tsquery over chunks.tsv + cards.tsv
      R2 identifier : pg_trgm similarity(ident_text, q_identifiers) > 0.3
      R3 vector     : HNSW cosine over chunks.embedding + cards.embedding
                      (iterative scan on; ef_search=80)
      R4 symbol     : exact/prefix match on chunks.symbols for extracted symbols
      R5 facts      : fact_key match on extracted (subject, predicate) guesses
                      + FTS over subject/object; temporal filter from step 3
      R6 links      : 1–2-hop card-link expansion seeded by R1–R5's top cards
                      (recursive CTE, depth ≤ 2, fan-out cap 25)
 5. Fusion — Reciprocal Rank Fusion across R1..R6:
      rrf(d) = Σ_r  w_r / (k + rank_r(d)),   k = 60,  w_r = 1.0 initially
    Keep fused_pool = 150. (RRF replaces v2's hand-tuned additive score soup:
    it is rank-based, robust to incomparable scores, and needs no calibration.)
 6. Rerank — cross-encoder over (query, candidate_text) for the top 100;
    candidate_text = title+body for cards, path+body for chunks. Model behind
    Reranker interface (default bge-reranker-v2-m3). Deep mode MAY add a final
    structured-LLM rerank over the top 20 (§8.5). Never LLM-first.
 7. Deterministic gates (order matters; gates move items, they don't re-score):
      G1 ACL          : already filtered; re-assert (defense in depth) — drop.
      G2 temporal     : superseded/retracted facts → `superseded` section.
      G3 verification : JIT branch verification (§9) for code-touching items
                        when repo+branch present; failed → `stale_flagged`,
                        inconclusive → served with verified:false.
      G4 conflicts    : conflicting fact/card groups → `conflicts` section
                        with deterministic winner annotation (§8.7).
      G5 diversity    : cap items per source_uri at 3 and per raw_id at 5
                        (anti-poisoning + coverage, §15.4).
 8. Pack assembly (§8.8) under the mode budget; greedy by rerank score with
    per-section floors: L1 profile excerpt always; every gotcha whose
    `structured.trigger` matches touched paths always; conflicts always.
 9. missing_evidence generation: query facets (from step 3) with zero
    post-gate results, plus items excluded solely by G3 failure, are written
    as explicit gaps ("found no current owner record for legacy_exporter").
10. Workspace mode: export pack per §12; return workspace ref.
11. Log: query_id, config_hash, retriever→fusion→rerank traces (OTel), and a
    memory_actions row.
```

Latency budget (p95, warm, no LLM rerank): `scoped` ≤ 700 ms; `deep` ≤ 2.5 s; `workspace` ≤ 6 s including export. Step 4 retrievers run concurrently; step 6 dominates — batch pairs to the reranker.

### 8.3 Why RRF + cross-encoder (decision record)

Hand-tuned additive bonus/penalty scoring (earlier design) mixes incomparable scales, drifts as corpora change, and is untestable. Rank fusion + learned reranking is the standard, evidenced hybrid-retrieval pattern; the v2 bonus/penalty *intents* survive as: hard gates (ACL, quarantine), sectioning (stale/conflict/superseded), per-section floors (gotchas), and two small deterministic multipliers applied post-rerank: `×0.8` if the item's only sources are chat/agent_run (source-authority), `×1.1` if runtime-validated within TTL. Nothing else touches scores.

### 8.4 Source authority (used by gates/floors and promotion, not for re-scoring)

```text
1 current code on target branch     5 incident reports
2 passing tests / CI / deploy       6 tickets
3 ADRs and maintained runbooks      7 chat (Slack)
4 PRs and review comments           8 unvalidated agent reflections
```

### 8.5 Optional structured LLM rerank (deep mode, top 20)

Returns per candidate: `{answers_query: yes|partial|no, needed_for:[procedure|gotcha|fact|context], risk: none|stale|conflicting, reason, score}`. Prompt + model ID + config hash logged (I5). Enable per namespace; adds ~1–2 s and ~$0.01–0.05/query — measure whether it pays on InstitutionalBench before enabling by default.

### 8.6 Answerability

The pack carries `answerability: high|partial|low` computed from: top rerank score, count of verified items, and unresolved-conflict presence. `low` instructs the agent (via L0 rules) to say "institutional memory doesn't establish this" rather than guess — abstention is a scored benchmark dimension (§17.4).

### 8.7 Deterministic conflict resolution

Two active claims conflict when facts share `fact_key` with overlapping validity and different objects, or cards are linked `contradicts`, or extraction flags a contradiction. Winner annotation order:

```text
branch-verified (passed, current) >
runtime-validated within TTL >
higher source authority (§8.4) >
later valid_from >
later asserted_at
```

The loser is never silently dropped: both appear in `conflicts` with the rule that decided. If the top two tie at authority level ≤ 3, mark `unresolved` and lower answerability.

### 8.8 EvidencePack shape (returned by /v1/query; also the workspace input)

```jsonc
{ "query_id": "uuid", "mode": "deep", "answerability": "high", "config_hash": "…",
  "profile_excerpt": "…",                       // L1 slice, cited
  "cards":       [ { "id":"…","card_type":"gotcha","title":"…","body":"…",
                     "status":"active","confidence":0.94,"verified":true,
                     "last_verified_at":"…","sources":[{"source_uri":"…","lines":"44-79"}] } ],
  "facts":       [ { "subject":"billing-api","predicate":"build_command","object":"make test",
                     "valid_from":"…","verified":true } ],
  "superseded":  [ … ],
  "conflicts":   [ { "claims":[…], "winner": "…", "rule":"branch-verified" } ],
  "stale_flagged":[ { "card": …, "verification": { "result":"failed", "detail":"file missing on branch Y" } } ],
  "raw_spans":   [ { "raw_id":"…","source_uri":"…","authority":4,"excerpt":"…" } ],
  "missing_evidence": [ "no current owner record for legacy_exporter" ],
  "token_total": 9412, "workspace_uri": null }
```

---

## 9. Verification subsystem

The feature that separates memba from a retrieval layer. Any memory citing code MUST be checkable against the branch the agent is about to change (I3). Design converges with GitHub Copilot's shipped just-in-time verification (§3.1).

### 9.1 Branch verification algorithm (per card/fact, per (repo, branch))

```text
VerifyAgainstBranch(target, repo, branch):
  head = resolve branch tip (Git connector; cached fetch, shallow, blobless)
  tree = cached checkout keyed by (repo, head_sha)          # §9.4
  results = []
  for ref in target.sources where ref.source_uri startswith "git://"+repo:
    A. file exists?           tree has ref.path            else FAIL(file_missing)
    B. symbol exists?         if ref names a symbol: look up in AST symbol
                              index at head (§9.3); if absent, search same
                              symbol name elsewhere in repo → INCONCLUSIVE(moved?)
                              else FAIL(symbol_missing)
    C. quote still holds?     if ref.quote set:
                                exact match within lines [start-20, end+20] → PASS
                                else normalized fuzzy match (token-level
                                similarity ≥ 0.85) anywhere in file → PASS(drifted)
                                else embedding sim(quote, file chunks) ≥ 0.80
                                     → INCONCLUSIVE(rewritten?)
                                else FAIL(content_changed)
    D. commit relevant?       if ref.source_version is a sha:
                                `git merge-base --is-ancestor sha head` → PASS
                                else INCONCLUSIVE(divergent_history)
  aggregate: any FAIL → failed;  all PASS → passed;  else inconclusive
  write verifications row {result, detail: per-ref outcomes, head_sha}
  on failed/passed: update card.last_verified_at / status per §10.7
```

Additional check types: `command_succeeded` (procedure's `verify_command` matches a script that exists in repo — and, where a sandbox runner is configured, actually exits 0); `source_still_exists` (non-git sources: doc revision still fetchable, Slack message not deleted); `test_passed` (CI connector observed green run touching the cited paths); `human_review`; `multi_source_support` (recount independent supporting sources ≥ 2).

### 9.2 Serving semantics

- `passed` → item served normally, `verified:true`.
- `failed` → item moved to `stale_flagged` with human-readable detail: *"This memory may be stale: supported by commit abc123, but `legacy_exporter/status_allowlist.yaml` no longer exists on branch feature/new-status."* Card status transitions to `stale` (§10.7). Never silently dropped — a wrong-but-flagged memory teaches the agent what changed.
- `inconclusive` → served with `verified:false` + reason; L0 rules tell agents to `mem.verify` or check the code before acting.

### 9.3 Code index

`internal/codeindex` maintains per-(repo, head) symbol tables: definitions, references, imports, package paths → `chunks.symbols`. Go via stdlib `go/ast` + `go/packages`; everything else via tree-sitter grammars (Go bindings), extracting function/method/type/const definitions and identifiers. Indexing is incremental: on push events, re-chunk only changed files (diff-driven), re-embed only changed chunks. Full-repo reindex is a fallback job.

### 9.4 Caching (verification must be cheap enough to run inline)

Cache key `(target_id, repo, head_sha)` in `verifications` — a JIT check first looks for an existing row at the current head. Working trees cached per `(repo, head_sha)` with LRU on disk (blobless clones; `git sparse-checkout` for cited paths only). Budget: p95 ≤ 150 ms per item warm, ≤ 2 s cold; deep-mode packs verify the top items concurrently (bounded worker pool, default 8).

---

## 10. Write path

The danger is not forgetting; it is polluting long-term memory with false or planted lessons (§3.2). Every write below is conservative by construction.

### 10.1 Ingest pipeline

```text
source event → connector normalizes → POST /v1/evidence (idempotent, §13.2)
  → store raw verbatim (I1)  [+ object store if > 256 KiB]
  → security scans (§15.3): secrets → quarantine/redact; injection patterns → quarantine
  → enqueue: chunk → embed → index
  → enqueue: extract_cards (LLM extractor over new/changed evidence)
       → candidate cards/facts written as status='proposed', created_from='extractor'
  → promotion evaluation (§10.6) runs on each proposal
```

### 10.2 Chunking (normative)

| Kind | Strategy | Size | Overlap |
|---|---|---|---|
| code | AST-aware: one chunk per function/method/type; oversized split at block boundaries; each chunk prefixed with `// file: path · package · imports summary` | ≤ 300 lines | none |
| prose/docs | heading-aware markdown/HTML splitter | 300–800 tokens | 10–15% |
| chat | one chunk per thread (or per 25 messages) | ≤ 800 tokens | none |
| PR | description; each diff hunk (with file header); each review thread | natural | none |
| CI/log | failure blocks + tail; head/tail sample of huge logs | ≤ 800 tokens | none |

### 10.3 Embeddings

One model for everything (ops simplicity): default `voyage-code-3` @ 1024-dim, stored as `halfvec`, behind the `Embedder` interface. `embedding_model` recorded per row; model migration = `reembed_model_migration` job (dual-write new column, atomic index swap). Quarantined or `embedding_forbidden` rows are never embedded.

### 10.4 Extraction

Extractor LLM runs over newly ingested evidence with per-source-type prompts producing **typed card/fact candidates with mandatory citations** (uri + line range + quote). Anything the extractor cannot cite is discarded, not stored (I2). Extraction is capped per document (≤ 8 candidates) and per day per namespace (config) to bound cost (§18.1). Prompts are versioned files in `configs/prompts/` — they are part of the benchmarked surface (§17.7).

### 10.5 Agent-run path

`mem.log` → raw evidence (`source_type=agent_run`, authority 8). `mem.propose` → card in `proposed`. Nothing an agent writes reaches `active` without §10.6. Failed-run reflections additionally carry `metadata.run_outcome=failed` and are excluded from the multi-source promotion rule entirely — they can only be promoted by human review or by later runtime validation.

### 10.6 Promotion rules (deterministic; the ONLY path to `active`)

A `proposed` card/fact is promoted iff at least one row matches:

| # | Rule | Assigned confidence |
|---|---|---|
| P1 | Human reviewer approves (`card_reviews.decision=approve`) | 0.95 |
| P2 | Runtime validation passed: `verify_command`/test executed green (sandbox or CI connector) within last 7 days | 0.90 |
| P3 | Branch verification passed against the repo default branch | 0.85 |
| P4 | ≥ 2 *independent* sources at authority ≤ 4 (different `source_uri` roots) support it | 0.75 |
| P5 | Namespace `policy.trusted_sources` explicitly whitelists the source type for auto-promotion | 0.70 |

Blockers (any → stays `proposed`, or `quarantined` for B5): B1 sole provenance is a failed run · B2 chat-only support · B3 contradicts a branch-verified active memory (files as counter-evidence instead) · B4 zero citations · B5 secret/PII/injection flags. **Ancestor-namespace (org-wide) promotion** additionally requires P1, or the same fact independently promoted in ≥ 2 child namespaces.

Anti-starvation: P2/P3 make the gate mostly automatic for code-backed knowledge — the common case ("this command works", "this file is generated") self-promotes within minutes via verification, no human in the loop. Track `proposal_queue_depth` and median time-to-promotion (§16); if the queue grows, tune P4/P5, don't bypass the gate.

### 10.7 Decay clock and invalidation (I6)

Every `active` memory carries `verify_by = last_verified_at + TTL(ttl_class)`:

```text
code:         28 days   (branch-checkable claims; matches the rolling-expiry
                         pattern of shipped industry systems, §3.1)
operational:  90 days   (ownership, environments, on-call, endpoints)
design:      180 days   (ADR-backed decisions, architectural constraints)
```

Sweeps (river cron): (1) `verify_sweep` re-runs the cheapest applicable check (source_still_exists, then branch check on default branch) for cards near `verify_by`; pass → clock resets; fail → `stale`. (2) `decay_sweep`: past `verify_by` with no passing check → `stale`; `stale` or unretrieved past `2×TTL` → `dormant`. (3) Invalidation triggers processed immediately, not on sweep: source deleted upstream, JIT branch check failed, superseding fact promoted, `verify_command` disappeared from repo, CODEOWNERS change (ownership cards), postmortem ingested that `contradicts` (extractor emits the link), human `invalidate`. Invalidation NEVER deletes raw evidence — it changes derived status and writes the reason (I1).

Retrieval touch (`last_retrieved_at`) does not extend `verify_by` — popularity is not truth — but does defer dormancy.

### 10.8 Deduplication / canonicalization (see also §11)

At propose-time: if an existing non-dormant card in scope has embedding cosine ≥ 0.92 AND same `subject`, the write becomes (a) a new `supports` citation on the existing card if bodies are equivalent, or (b) a `related_to`-linked new proposal flagged `near_duplicate_of` for the consolidation job to merge. Prevents the N-copies-of-the-same-gotcha failure mode.

---

## 11. Sleep-time consolidation

Offline improvement of memory between interactions (pattern validated by the sleep-time-compute line of work, §3.1). Nightly per-namespace `consolidate_ns` job; every output is a **proposal or link subject to §10.6** — consolidation never silently rewrites active memory.

```text
C1 dedup/merge   : cluster near-duplicate active cards (cosine ≥ 0.92, same
                   subject); emit one merged card proposal citing the union of
                   sources + `supersedes` links. Auto-promotable via P4.
C2 profile build : regenerate L1 (§5) from active profile-tagged cards + repo
                   HEAD facts; byte-stable serialization (sorted, templated)
                   so prompt caches only bust on real change.
C3 contradiction : sweep facts sharing fact_key with overlapping validity;
                   emit conflict records + re-verification jobs for both sides.
C4 link inference: propose `related_to` links from co-citation (shared raw
                   sources) and co-retrieval (same query_id in top-k) stats.
C5 gap mining    : cluster failed searches / low-answerability queries from
                   memory_actions; emit "missing knowledge" report per
                   namespace (human-facing; optionally card stubs).
C6 failure mining: cluster mem.log entries from failed runs; when the same
                   failure signature appears ≥ 3 times across runs, emit a
                   gotcha PROPOSAL citing the run logs (still gated —
                   promotion needs P1/P2 since sources are authority 8).
C7 decay/dormancy: as §10.7.
```

Budget: LLM usage for C1/C5/C6 capped per namespace per night (config). All consolidation runs write a `consolidation_runs` stats row; regressions in card count/quality are observable (§16).

---

## 12. Evidence workspace format (L4)

The core product surface for coding agents. A workspace is generated from an EvidencePack, written to the object store as `tar.zst`, and either (a) extracted to a local path by the agent runtime (preferred: agent uses native file tools) or (b) served file-by-file via `GET /v1/workspaces/{id}/files/*`.

### 12.1 Layout (normative)

```text
.memworkspace/
├── manifest.json            # §12.2 — machine-readable index + ACL decision log
├── README.md                # how to use this workspace + I7 data-not-instructions
│                            #   notice + "call mem.search for gaps" pointer
├── query.md                 # the goal, verbatim, + query analysis facets
├── profile.md               # L1 (cited)
├── memory_cards.md          # active cards, grouped by type; each with status,
│                            #   confidence, verified flag, sources
├── active_facts.jsonl       # current facts (one per line, with citations)
├── superseded_facts.jsonl   # what USED to be true (labeled)
├── conflicts.jsonl          # §8.7 groups with winner + rule
├── procedures.md            # step-by-step, with verify_command
├── gotchas.md               # ALWAYS present if any trigger matches the goal
├── verifications.jsonl      # JIT verification outcomes for included items
├── code_refs.jsonl          # symbol/path pointers into the actual repo
├── missing_evidence.md      # §8.2 step 9 — what memory could NOT establish
├── evidence/                # raw spans, one file per item, front-matter:
│   ├── docs/…               #   source_uri, authority(1-8), event_time,
│   ├── code/…               #   verified, quarantine=false, sha
│   ├── prs/… incidents/… slack/… ci/… agent_runs/…
└── scratch/
    └── agent_notes.md       # writable; agent runtime may mem.log it at task end
```

Ordering matters and is part of the benchmarked scaffold surface (§17.7): README → gotchas → procedures → cards is the shipping default (safety before recipes before facts).

### 12.2 manifest.json

```jsonc
{ "workspace_id": "…", "tenant_id": "acme",
  "namespace_id": "/acme/platform/payments/repos/billing-api",
  "query": "How do I add a new invoice status safely?",
  "repo": "billing-api", "branch": "feature/new-status",
  "mode": "workspace", "created_at": "…", "expires_at": "…",
  "config_hash": "…", "model_ids": { "embedder": "…", "reranker": "…" },
  "token_total": 41230,
  "files": [ { "path": "gotchas.md", "kind": "gotchas", "token_count": 900,
               "item_ids": ["card_123"] }, … ],
  "acl_decisions": [ { "item": "raw:…", "decision": "included",
                       "principal": "agent:payments-coder-17" },
                     { "item": "raw:…", "decision": "excluded_acl" } ],
  "redactions": [ { "file": "evidence/slack/thr_9.md", "marker": "⟦REDACTED:api_key⟧" } ] }
```

### 12.3 Generation algorithm and budgets

Greedy fill by rerank score within per-file caps (defaults, of the 64k total): profile 1.5k · cards 3k · facts 2k · procedures 2k · gotchas 1.5k · code_refs 1k · conflicts/verifications 1k · remainder to `evidence/` spans, highest-authority first, capped 3 files per source_uri (G5). Every evidence file gets YAML front-matter (source_uri, authority, verified, event_time) so a grepping agent always sees provenance adjacent to content. `missing_evidence.md` is always written, even if empty ("no gaps detected") — its presence trains abstention.

Workspaces expire (`workspace_ttl_hours: 72`, then `workspace_gc`); they are snapshots, not live views — the README states the created_at and tells the agent to re-verify anything older than the task at hand.

---

## 13. Public HTTP API

Small and stable. Note: this is plan v3 but the API ships as `/v1` — document version ≠ API version. JSON everywhere; errors are RFC 7807 `application/problem+json`; auth is bearer tokens carrying `{tenant_id, principal, acl_subjects[]}` (issued by the host platform; memd validates signature + tenant); optional mTLS. All mutating POSTs accept an `Idempotency-Key` header.

### 13.1 Endpoint summary

```text
POST /v1/evidence                    insert raw evidence (connectors, mem.log)
POST /v1/query                       all read modes (§8.1); mode=workspace mounts L4
GET  /v1/profile?namespace=&repo=&level=0|1     L0/L1 with ETag (cache-stable)
GET  /v1/workspaces/{id}             manifest
GET  /v1/workspaces/{id}/archive     tar.zst
GET  /v1/workspaces/{id}/files/{path}
POST /v1/verify                      run a verification now (mem.verify)
POST /v1/cards                       propose a card (mem.propose)
GET  /v1/cards/{id}                  card + citations + verification history (mem.open)
POST /v1/cards/{id}/review           {decision: approve|reject|invalidate|edit, reason}
POST /v1/cards/{id}/invalidate      (mem.invalidate; sugar over review)
GET  /v1/cards?namespace=&status=proposed      review queue (curation UI backend)
POST /v1/actions                     record memory action (agent runtimes w/o tool wrapper)
POST /v1/admin/namespaces            create/update namespace + policy
POST /v1/admin/bootstrap             §20 cold-start for a repo
GET  /v1/healthz · GET /v1/metrics   liveness · Prometheus
```

### 13.2 Representative contracts

`POST /v1/evidence` — idempotent on `(tenant, source_uri, source_version, content_hash)`; re-posting returns 200 + existing id.

```jsonc
{ "namespace_id": "/acme/platform/payments/repos/billing-api",
  "source_type": "git", "source_uri": "git://billing-api/src/invoice/status.go",
  "source_version": "abc123", "title": null, "body": "…",
  "acl": { "read": ["team:payments"] },
  "metadata": { "repo": "billing-api", "branch": "main", "language": "go" },
  "event_time": "2026-07-01T10:00:00Z" }
→ 201 { "raw_id": "…", "chunks_enqueued": true, "quarantined": false }
```

`POST /v1/query` (request; response is the EvidencePack of §8.8):

```jsonc
{ "namespace_hints": ["/acme/platform/payments/repos/billing-api"],
  "query": "How do I add a new invoice status safely?",
  "goal_type": "coding_change",            // coding_change | question | incident | onboarding
  "repo": "billing-api", "branch": "feature/new-status",
  "mode": "workspace", "max_tokens": 64000,
  "require_citations": true, "require_branch_verification": true,
  "as_of": null }                          // set for point-in-time audits (§6.6)
```

`POST /v1/verify` → `{ verification_id, result: passed|failed|inconclusive, detail: {per_ref: […]}, head_sha }`.

Error model: `401` bad token · `403` ACL (never distinguishable from 404 for objects the principal can't see — no existence oracle) · `409` idempotency conflict · `422` schema/citation-missing (B4) · `429` per-principal rate limits.

---

## 14. Go implementation structure

### 14.1 Repository layout

```text
memba/
├── cmd/
│   ├── memd/            # server (API + workers in one binary; --role=api|worker|all)
│   ├── memctl/          # admin CLI: bootstrap, review, reindex, namespace, inspect
│   └── mem-bench/       # benchmark runner (§17)
├── pkg/                 # public, importable
│   ├── memory/          # request/response types (EvidencePack, Card, …)
│   ├── client/          # Go client for /v1 (used by connectors, benchmarks, agents)
│   └── evalapi/         # BenchmarkMemory interface (§17.2)
├── internal/
│   ├── api/             # chi router, handlers, problem+json, authn/z middleware
│   ├── authz/           # ACL evaluation (§15.2), acl_subjects resolution
│   ├── ingest/          # normalization, idempotency, security scans
│   ├── chunk/           # per-kind chunkers (§10.2)
│   ├── codeindex/       # go/ast + tree-sitter symbol extraction, incremental
│   ├── embed/           # Embedder iface + voyage/openai/local adapters
│   ├── extract/         # card/fact extraction prompts + parsing + citation check
│   ├── cards/ facts/    # lifecycle, promotion (§10.6), decay (§10.7)
│   ├── verify/          # §9 (git ops via go-git + exec git for merge-base)
│   ├── retrieve/        # retrievers R1–R6, RRF fusion
│   ├── rerank/          # Reranker iface + bge/cohere/voyage adapters + LLM rerank
│   ├── gates/           # §8.2 G1–G5
│   ├── pack/            # evidence pack assembly + budgets
│   ├── workspace/       # §12 writer + GC
│   ├── consolidate/     # §11 jobs C1–C7
│   ├── actions/         # memory_actions recording
│   ├── jobs/            # river queue setup, cron schedules
│   ├── store/           # interfaces; store/postgres (pgx v5, sqlc-generated)
│   ├── objstore/        # S3 iface (aws-sdk-go-v2; MinIO-compatible)
│   ├── secscan/         # secrets (gitleaks rules + entropy) + injection scan (§15)
│   └── telemetry/       # OTel traces/metrics
├── connectors/          # standalone binaries/libs using pkg/client:
│   ├── git/ github/ slack/ jira/ docs/ ci/ incidents/ agent/
├── migrations/          # goose; river migrations vendored
├── configs/             # memd.yaml, prompts/, scaffolds/
└── testdata/            # golden packs, ACL fuzz corpora, injection corpus
```

### 14.2 Core interfaces (stability contract — escape hatches hang off these)

```go
type MemoryService interface {
    InsertEvidence(ctx, InsertRequest) (InsertResponse, error)
    Query(ctx, QueryRequest) (EvidencePack, error)
    Verify(ctx, VerifyRequest) (VerificationResult, error)
    Propose(ctx, ProposalRequest) (ProposalResponse, error)
    Review(ctx, ReviewRequest) (ReviewResponse, error)
    RecordAction(ctx, MemoryAction) error
}
type Retriever interface {   // one per R1..R6; fused by retrieve.Fuse
    Retrieve(ctx, AnalyzedQuery, ScopeFilter, n int) ([]Candidate, error)
}
type Embedder  interface { Embed(ctx, []string) ([][]float16, error); ModelID() string }
type Reranker  interface { Score(ctx, query string, docs []string) ([]float64, error) }
type Verifier  interface { Verify(ctx, VerifyRequest) (VerificationResult, error) }
type WorkspaceWriter interface { Write(ctx, EvidencePack) (WorkspaceRef, error) }
type Promoter  interface { Evaluate(ctx, CardID) (PromotionDecision, error) }
type VectorStore interface { // pgvector today; pgvectorscale/external later (§19)
    Upsert(ctx, []VecRow) error
    Search(ctx, vec []float16, filter ScopeFilter, n int) ([]Hit, error)
}
```

### 14.3 Dependencies (pinned rationale)

`pgx/v5` + `sqlc` (typed queries) · `pressly/goose` (migrations) · `riverqueue/river` (jobs) · `go-chi/chi` (HTTP) · `go-git` + shelling to `git` for merge-base/sparse-checkout · `smacker/go-tree-sitter` (+ vendored grammars) · `aws-sdk-go-v2/s3` · `otel-go` · `gitleaks` rule set (vendored TOML, own matcher). No ORM; no gRPC in v3 (add later behind api/ if an agent runtime needs it).

### 14.4 Connectors

Each connector: watches its source (webhook or poll), normalizes to `POST /v1/evidence`, and is stateless beyond a cursor. Phase order: local-file → git → github (PRs/reviews/CODEOWNERS) → ci → docs → slack → incidents/jira → agent (the runtime-side shim that exposes `mem.*` tools and forwards mem.log/propose).

### 14.5 Concurrency & performance notes

Retrievers fan out with `errgroup` + per-retriever timeouts (partial results acceptable, logged). Reranker batched (≤ 100 pairs/call). Verification worker pool (8). Embedding writes batched (64). All external model calls carry circuit breakers; retrieval degrades gracefully to R1+R2 if the embedder is down (flagged in pack).

### 14.6 Testing strategy (CI-gated)

- Unit + `testcontainers-go` Postgres integration for every store query.
- **Golden pack tests**: fixed corpus + fixed config hash → byte-stable EvidencePack snapshots; diffs reviewed like code.
- **ACL property tests**: generated principals/ACLs; assert no returned item (any section, any workspace file) has an effective ACL excluding the principal; runs 10k random queries in CI.
- **Tenant-isolation property test**: two tenants, identical content; assert zero cross-tenant hits at every retriever.
- **Injection corpus test**: known poisoning payloads (MINJA/AgentPoison-style, §15.4) must quarantine and never reach L0/L1 or workspaces.
- Benchmark smoke: 20-case LongMemEval subset must not regress > 2 pts on merge.

---

## 15. Security and governance

### 15.1 Threat model (what we defend against)

1. Cross-tenant/cross-team leakage via retrieval or embeddings.
2. Secrets ingested from code/chat/CI resurfacing in packs.
3. **Memory poisoning**: an attacker (or a hallucinating agent) plants content that later steers agents — demonstrated as MINJA-style injection through ordinary interactions and AgentPoison-style backdoors where trigger queries retrieve malicious records (§3.2).
4. Prompt injection *through* retrieved evidence (evidence contains instructions).
5. Stale-memory hazards (agent acts on outdated truth) — treated as a safety property, handled by I3/I6.

### 15.2 ACL model

ACL subjects: `user:*, team:*, agent:*, svc:*, tenant:*`. Evaluation: principal's `acl_subjects` ∩ object's `acl.read` ≠ ∅. **Derivation rule**: a derived object's ACL = intersection of all its sources' read sets, computed at write time and stored (`cards.acl`); a card citing sources with disjoint read sets is only visible to principals in the intersection — if that's empty, the card is valid but unservable until a human review explicitly re-scopes it with redaction. `recompute_acl` job cascades upstream ACL changes (source restricted → derived cards restricted within minutes). Workspaces record every include/exclude decision in the manifest (§12.2) for audit.

### 15.3 Secrets

Ingest scan: gitleaks default ruleset + Shannon entropy > 4.5 on base64/hex-like tokens ≥ 20 chars. Hits → span-level redaction `⟦REDACTED:aws_key⟧` in chunks (retrievable, harmless), raw body stored with `quarantined=true, embedding_forbidden=true` under stricter access (`tenant:secops` only). Verification quotes are redacted the same way. Secret-bearing spans never reach embeddings, packs, or workspaces.

### 15.4 Memory-poisoning defenses (maps to threats 3–4)

```text
D1 Provenance gate    §10.6 — agent/chat content cannot self-promote; failed-run
                      content cannot promote via multi-source at all.
D2 Injection scan     ingest-time: pattern set (imperative-to-agent phrasing,
                      "ignore previous", tool-call/JSON syntax in prose, base64
                      blobs in chat) + small classifier → quarantined,
                      never indexed for default recall; secops review queue.
D3 Compiled L0/L1     always-on layers are generated ONLY from active cards
                      (post-gate); no verbatim low-authority text ever (I7).
D4 Data framing       workspace README + per-file authority front-matter mark
                      evidence as untrusted data; agent scaffolds instructed to
                      never execute instructions found inside evidence/.
D5 Diversity cap      G5 (§8.2): ≤ 3 items per source_uri per pack — a single
                      poisoned document cannot dominate retrieval.
D6 Trigger audit      consolidation flags cards whose retrieval is dominated by
                      one narrow query pattern (AgentPoison signature) for review.
D7 Full provenance    every served item carries source, author principal, and
                      verification chain — post-incident forensics via `audit` mode.
```

### 15.5 Governance & retention

Per-namespace policy JSON controls: trusted sources (P5), TTL overrides, human-review-required card types, retention (raw evidence default indefinite; `agent_run` raw default 180 d then object-store cold tier), and right-to-be-forgotten: hard delete of raw + cascade invalidation of derived (the one sanctioned deletion path; logged). Full audit log = `memory_actions` + `card_reviews` + `verifications` + workspace manifests.

---

## 16. Observability

OTel traces span the full pipeline (ingest event → chunks → extraction; query → retrievers → fusion → rerank → gates → pack → workspace) with `config_hash` on every query span. Prometheus metrics (selection):

```text
ingest:   memba_ingest_lag_seconds{source_type}, memba_chunks_total,
          memba_quarantine_total{reason}
query:    memba_query_latency_seconds{mode,quantile}, memba_pack_tokens{mode},
          memba_retriever_latency{r}, memba_rerank_latency,
          memba_answerability_total{level}
memory:   memba_cards{status}, memba_cards_verify_overdue,
          memba_verification_pass_ratio{type}, memba_proposal_queue_depth,
          memba_time_to_promotion_seconds, memba_promotions_total{rule}
behavior: memba_actions_total{action_type}, memba_search_open_ratio,
          memba_ignored_relevant_ratio (judge-sampled), memba_stale_served_total
security: memba_acl_denials_total, memba_tenant_isolation_violations_total (MUST be 0),
          memba_injection_quarantine_total
```

Ship one **memory health dashboard** per tenant: card freshness histogram, verification pass rate trend, proposal queue + median time-to-promotion, stale-served count, gap-mining topics (C5), quarantine review backlog. This dashboard is the human curation surface's front page; the review queue (`GET /v1/cards?status=proposed`) is its second page. A minimal web UI for review ships in Phase 5 — human curation throughput is a first-class product concern, not an afterthought.

---

## 17. Benchmarking

Benchmark-first is invariant I5: `mem-bench` drives the same `/v1` API as production, from Phase 0 onward.

### 17.1 Harness

```go
type BenchmarkMemory interface {
    Reset(ctx, namespace string) error                 // fresh namespace per case-set
    Insert(ctx, item BenchmarkItem) error              // → POST /v1/evidence
    Query(ctx, q BenchmarkQuery) (EvidencePack, error) // → POST /v1/query
    RecordAction(ctx, a MemoryAction) error
}
```

A separate **Reader** (the answering agent) consumes packs/workspaces and produces answers; readers are pluggable (fixed model, pinned prompt, temp 0) so memory quality is measured independently of reader strength. For workspace-mode benchmarks the reader is a minimal file-tool agent loop (read/grep/list + mem.search), mirroring the SOTA context-gathering setup (§3.1).

### 17.2 Suite

| Benchmark | Role | Why |
|---|---|---|
| **LongMemEval-V2** | primary | hardest current target; agentic context-gathering framing matches L4; published SOTA ≈ 72.5% (AgentRunbook-C) |
| **LongMemEval (v1)** | primary | standard axes: extraction, multi-session, temporal, knowledge updates, abstention |
| **MemoryAgentBench** | primary | retrieval + test-time learning + long-range understanding + selective forgetting (maps to decay/invalidation) |
| **InstitutionalBench** | primary (internal) | §17.3 — the only suite that tests what memba is actually for |
| **BEAM** | secondary | very-long-conversation stress |
| **LoCoMo** | sanity only | documented annotation-quality problems; never tune to it |
| SWE-ContextBench (2026) | adapter in Phase 6 | coding-agent context benchmark surfaced in the research pass; adopt once independently reproduced |

(Dropped from earlier plans: the supermemory MemoryBench harness — vendor-maintained, coverage overlaps MemoryAgentBench; "watch, don't integrate.")

### 17.3 InstitutionalBench (build this; nothing public tests institutional coding memory)

Generated from 2–3 real OSS repos + synthetic org history (scripted PRs, docs with planted staleness, incidents, failed agent runs), gold labels human-reviewed. Task families: onboarding Q&A · **stale-doc traps** (doc says X, code says Y — credit requires flagging staleness, not parroting either) · migration ordering · ownership lookup · prior-failure avoidance (seeded failed run; later task must surface the gotcha) · forbidden-file avoidance (generated code) · command recall · **abstention set** (questions the corpus cannot answer; credit for `missing_evidence`). Private test split, versioned, hash-pinned; generation code public inside the repo, gold answers not.

### 17.4 Metrics (always reported separately — never one collapsed number)

Retrieval (R@5/10/20, MRR, NDCG) · Answer (EM/F1, pinned LLM judge, 5% human sample) · Evidence (citation coverage %, source-authority mix, tokens/pack) · Temporal (current-fact accuracy, superseded-detection, stale-suppression) · Procedural (command/precondition/gotcha recall) · Coding (tests pass, forbidden-file avoidance, generated-code rule adherence) · Verification (JIT pass rate, stale-detection precision/recall, latency) · Governance (ACL leak = 0 required, secret leak = 0 required, quarantine P/R) · Behavior (search→open precision, ignored-relevant rate, proposal acceptance, invalidation acceptance) · Efficiency (p50/p95 by mode, model calls/query, $/query, storage, index build time).

### 17.5 Beat-SOTA plan (concrete)

Targets, stated against paper-reported numbers at research time (July 2026):

```text
T1 LongMemEval-V2 overall ≥ 75%   (SOTA 72.5%, AgentRunbook-C)
T2 LongMemEval(v1) avg: ≥ best published reproduced number at eval time,
   with the abstention subscore ≥ 90% (most systems' weakest axis)
T3 MemoryAgentBench: top-quartile on all four axes, best-in-class on
   selective forgetting (decay/invalidation is memba's home turf)
T4 InstitutionalBench: ≥ 90% forbidden-file avoidance, ≥ 85% stale-doc
   trap detection, ≥ 80% prior-failure avoidance
T5 Report tokens & $ next to every accuracy number; win the
   accuracy-per-1k-evidence-tokens frontier, not just raw accuracy.
```

The mechanism (the differentiated hypothesis, testable as ablations):

```text
H1 curated workspace > raw-trajectory workspace: cards+facts+labels reduce
   the reader's search burden vs. AgentRunbook-C-style raw files.
H2 in-workspace re-query (mem.search from inside the task) > single mount.
H3 verification & missing_evidence.md lift temporal/abstention axes where
   top-k systems bleed points.
H4 RRF+CE cascade ≥ additive scoring at equal latency (regression guard).
Ablation grid: {raw files, curated, curated+re-query} × {verify on/off} ×
   {missing_evidence on/off} — run on LME-V2 + InstitutionalBench.
```

### 17.6 Contamination and integrity controls

Private splits for InstitutionalBench; public-benchmark items tagged `benchmark_seed` and excluded from consolidation and from any future model training (Phase 7); `Reset()` guarantees namespace isolation; every run archives `run/{config.yaml, config_hash, model_ids.json, prompts/, inserted_items.jsonl, queries.jsonl, memory_actions.jsonl, evidence_packs.jsonl, workspaces/, answers.jsonl, scores.json, traces.jsonl}` so regressions are inspectable diff-by-diff. Self-reported vendor numbers are never used as targets without reproduction.

### 17.7 Scaffold optimization (offline, gated)

The agent's memory *behavior* is a skill; optimize the scaffold, not the database. `mem-bench scaffoldopt --bench institutionalbench --candidate configs/scaffolds/v3a.yaml --baseline configs/scaffolds/v3base.yaml` runs A/B over: tool descriptions, L0 rules text, workspace file order/layout, extraction prompts, promotion thresholds (P4 count, TTLs), RRF weights `w_r`, rerank_top. Optimizer: reflective prompt evolution in the GEPA/DSPy style (propose → evaluate → keep-if-better with significance test), producing a human-readable diff report ("gotchas.md before procedures.md: +7.4% gotcha recall, +2.1% task success, +350 tokens, no governance regressions"). Never auto-optimized: schema, ACL policy, raw-evidence handling, production promotion rules without human sign-off.

---

## 18. Cost model and the long-context question

### 18.1 Design-point cost (1M docs ≈ 10M chunks ≈ 4B tokens; defaults from §4.4)

```text
One-time embed      4B tok × ~$0.02–0.18/M        ≈ $80–720 (model-dependent)
Vector storage      10M × 1024-dim halfvec ≈ 20 GB + HNSW ≈ 30–40 GB total
Postgres footprint  ~150–250 GB all-in (text, indexes, vectors) → 64 GB RAM box
Extraction (ingest) ≤ 8 candidates/doc, capped/day; ~$0.002–0.01/doc → budget knob
Per query           scoped: 1 embed call ≈ $0.0001
                    deep:   + CE rerank (self-host ≈ GPU amortized ~$0.001;
                            hosted ≈ $0.002/query) + optional LLM rerank $0.01–0.05
                    workspace: + object-store write, negligible
Sleep-time          capped LLM budget/namespace/night (default $2) — the whole
                    point of consolidation is spending cheap offline tokens to
                    save expensive online ones
```

Track `$/query` and `$/promoted-card` as first-class metrics (§17.4-Efficiency).

### 18.2 Why long context doesn't obsolete this system (decision record)

1M–10M-token contexts change delivery, not need: (a) **context rot** — models degrade non-uniformly with length and distractor density (NoLiMa; Chroma 2025 report), and institutional corpora are almost all distractors for any given task; (b) **economics** — re-reading org history per task is linear cost per call vs. memba's amortized index; (c) **no governance** — a raw dump has no ACLs, no verification, no staleness flags; (d) **compaction synergy** — when agent runtimes compact context, mem.log is where the compacted-away observations persist. What long context *does* change: L0/L1 are engineered as **cache-stable prefix blocks** (deterministic serialization, change only on promotion) so prompt caching makes always-on memory nearly free, and workspace budgets (64k) are generous because modern readers handle them — but curated-and-verified beats big-and-raw on every benchmark axis we target (H1/H3, §17.5).

---

## 19. Scale limits and escape hatches (pre-committed, so nobody panic-rewrites)

| Pressure | Threshold | Escape hatch (in order) |
|---|---|---|
| Vector recall/latency | > 30–50M vectors per cluster or deep p95 > 2× budget with tuned ef_search + partial indexes | (1) LIST-partition by tenant/namespace; (2) pgvectorscale (StreamingDiskANN — still Postgres); (3) external vector store behind `VectorStore` iface |
| Filtered-HNSW recall | measured R@20 < 0.9 vs. exact scan sample | iterative scans → partial per-namespace indexes → (2) above |
| Lexical scale | FTS p95 > 500 ms at > 100M chunks | dedicated search engine behind a `Retriever` — adapter, not rewrite |
| Graph traversal | link expansion > 2 hops needed, or CTE p95 > 300 ms | materialized closure tables first; graph DB only if product genuinely needs deep traversal (it shouldn't — links route search, §6.5) |
| Ingest volume | > 5k evidence/s sustained | connectors → queue in front (NATS/Kafka) feeding the same /v1/evidence semantics |
| Job throughput | river saturation | worker-role replicas (`memd --role=worker`) before any new infra |

The one-service/one-Postgres/one-object-store constraint holds until a threshold above is *measured*, not feared.

---

## 20. Cold start (a fresh deployment must be useful on day one)

`memctl bootstrap --repo <url> --namespace <ns>` (also `POST /v1/admin/bootstrap`):

```text
1. Ingest: README*, docs/, ADRs, CODEOWNERS, Makefile/justfile/package.json
   scripts, CI configs (.github/workflows, etc.), last 500 merged PRs
   (title+description+review comments), git log --numstat for ownership stats.
2. Extract seed cards: build/test/deploy commands, generated-code rules
   (from .gitattributes, //go:generate, codegen configs), ownership,
   top conventions from repeated review comments ("don't edit generated…").
3. Verify immediately: run P3 branch checks on every seed card; where a
   sandbox runner is configured, execute build/test commands → P2 runtime
   validation. Verified seeds promote instantly; the rest stay proposed
   in the review queue.
4. Generate L1 profile (C2) and emit a bootstrap report: cards promoted,
   proposals awaiting review, detected gaps.
Target: useful, verified L1 within 1 hour of install; zero human input
required for the code-backed 80%.
```

Week-one: connect github + ci connectors (PR/CI stream keeps verification live), schedule nightly consolidation, seed InstitutionalBench namespace for regression tracking.

---

## 21. Phased roadmap (each phase ships behind the same /v1 API; sizes S/M/L)

```text
P0 Contracts & skeleton (M)
   /v1 API types, migrations (goose+river), authz middleware, raw evidence
   store + idempotency, local-file connector, FTS retrieval, mem-bench
   skeleton driving /v1, golden-pack + ACL property test rigs.
   EXIT: Insert/Query work on a local corpus; a benchmark case runs E2E.

P1 Hybrid retrieval & workspace (L)
   object store, chunkers, embeddings+HNSW, trigram, RRF fusion, CE rerank,
   pack assembly + budgets, workspace writer + GC, LongMemEval(v1) adapter.
   EXIT: LME(v1) runs; every deep query emits an inspectable workspace;
   golden packs stable.

P2 Code awareness & verification (L)
   codeindex (go/ast + tree-sitter), git/github connectors (incremental),
   §9 branch verification + JIT cache, G3 gate, `audit` mode.
   EXIT: stale citations flagged E2E on a real repo; verification p95 in budget.

P3 Cards, proposals, promotion (M)
   card lifecycle, mem.propose/open/verify/invalidate, promotion engine
   (P1–P5/B1–B5), review API + minimal queue UI, dedup-at-propose, decay
   sweeps + TTL clocks.
   EXIT: unvalidated proposals never appear in default recall; time-to-
   promotion metric live; decay demonstrably retires stale cards.

P4 Temporal facts & conflicts (M)
   facts + bitemporal queries, supersession, G2/G4 gates, conflict rules,
   `as_of` audit queries.
   EXIT: LME temporal/knowledge-update axes improve vs. P1 baseline;
   packs separate active/superseded/conflicting.

P5 Consolidation, security hardening, connectors (L)
   sleep-time jobs C1–C7, secscan (secrets+injection) + quarantine flows,
   D1–D7 poisoning defenses + injection-corpus CI test, ci/docs/slack
   connectors, health dashboard + review UI, bootstrap command.
   EXIT: injection corpus 100% quarantined; nightly consolidation running;
   cold-start ≤ 1 h on a reference repo.

P6 Beat-SOTA campaign (L)
   LongMemEval-V2 + MemoryAgentBench + BEAM adapters, InstitutionalBench v1,
   ablation grid (H1–H4), scaffoldopt loop, efficiency reporting.
   EXIT: T1–T5 hit or a written analysis of the gap with next actions.

P7 Optional memory-specialist model (M, only if P6 evidence demands)
   train a small memory-ops model on memory_actions data; A/B vs. prompt-only
   scaffold. EXIT: wins quality without governance regressions, or is shelved.
```

### Acceptance criteria for "v3 complete" (end of P6)

```text
A1 Benchmarks: T1–T4 achieved on pinned readers; full run artifacts archived.
A2 Governance: 0 ACL leaks and 0 tenant-isolation violations across the 10k-query
   fuzz; 0 secret leaks on the secret corpus; injection corpus 100% quarantined.
A3 Latency: scoped p95 ≤ 700 ms, deep ≤ 2.5 s (no LLM rerank), workspace ≤ 6 s.
A4 Lifecycle: median time-to-promotion < 10 min for code-backed proposals;
   stale-served rate < 1% of deep queries; decay retires ≥ 95% of planted-stale
   fixtures within one sweep cycle.
A5 Ops: single binary + Postgres + object store; bootstrap ≤ 1 h; dashboard live.
```

---

## 22. Risks and open questions (tracked, not hidden)

```text
R1 Extraction quality ceiling: cards are only as good as the extractor;
   mitigation = citation-mandatory extraction + promotion gates + C5 gap
   mining; measure card precision via review-decision rates.
R2 Verification blind spots: semantics can change while cited lines survive
   (quote passes, meaning changed). Mitigation: P2 runtime validation for
   procedures; treat quote-PASS(drifted) as verified:false for high-severity
   gotchas. Open: cheap semantic-diff checks.
R3 Review-queue starvation in teams that never review: P2/P3 auto-promotion
   covers code-backed knowledge; C5 reports make the backlog visible; policy
   can widen P5 per namespace. Watch time-to-promotion.
R4 pgvector at the top of its range: thresholds + hatches in §19; measure
   monthly.
R5 Benchmark overfitting: private splits, ablations, and the efficiency
   frontier (T5) as a counterweight to leaderboard chasing.
R6 SWE-ContextBench maturity: adopt only after independent reproduction.
```

---

## 23. Changelog: v2 → v3 (for the record; the document above is standalone)

| Area | v2 | v3 | Why |
|---|---|---|---|
| KEEP | raw-evidence-first, one-service/one-Postgres/one-object-store, cards+facts+links, branch verification, proposal gating, memory actions, workspace, benchmark-first, phased roadmap | same | evidence supported all of it |
| Default interface | workspace "promoted" but optional | **L4 workspace is the default** for coding/long-horizon tasks | SOTA context-gathering result (§3.1) |
| Scoring | additive bonus/penalty soup | **RRF (k=60) + cross-encoder cascade** + deterministic gates; intents moved into gates/floors | standard evidenced hybrid pattern; testable (§8.3) |
| Lifecycle | statuses only | **verification clocks (verify_by) + TTL classes (28/90/180d) + dormancy** (I6) | shipped industry practice; anti-"hallucinations of the past" |
| Consolidation | promotion only | **nightly sleep-time jobs C1–C7** (dedup, profile, conflicts, gap/failure mining) | validated offline-compute line of work |
| Security | ACLs + secrets | + **memory-poisoning threat model & defenses D1–D7** (MINJA/AgentPoison), injection CI corpus, I7 | 2025–26 attack literature |
| Schema | separate procedure/gotcha/skill catalogs; separate proposals | **collapsed into typed cards**; proposals = status='proposed' | one lifecycle, one review surface, simpler |
| Benchmarks | incl. MemoryBench; LoCoMo peer | **drop MemoryBench**; LoCoMo demoted to sanity; add MemoryAgentBench, BEAM, LME-V2 as primary, SWE-ContextBench watch; explicit SOTA targets + ablations | benchmark-quality findings (§3.2) |
| Citations | included "MemPalace" | **removed (unverifiable)**; ideas re-grounded in verified sources; AutoMem kept as verified repo w/ self-reported results | research-pass integrity (§3.3) |
| New sections | — | cold start (§20), cost model (§18), observability/curation UX (§16), escape-hatch thresholds (§19), tenant-isolation & injection CI tests (§14.6) | gaps surfaced by research |

---

## 24. References

Verified during the July 2026 research pass unless noted. "SR" = self-reported results.

```text
Benchmarks
- LongMemEval — arXiv:2410.10813
- LongMemEval-V2 (AgentRunbook-C, SOTA ≈72.5%) — arXiv:2605.12493
- LoCoMo — arXiv:2402.17753 (known annotation-quality caveats; sanity use only)
- MemoryAgentBench — arXiv:2507.05257
- BEAM — very-long-conversation benchmark (2025)
- SWE-ContextBench — 2026 coding-context benchmark; adopt after reproduction

Systems & methods
- GitHub Copilot memory — github.blog: "Building an agentic memory system
  for GitHub Copilot" (citations, permission scoping, JIT branch verification,
  rolling expiry)
- MemGPT — arXiv:2310.08560; Letta docs; Sleep-time Compute — arXiv:2504.13171
- Zep/Graphiti (temporal KG, bitemporal edges) — arXiv:2501.13956 (SR)
- Mem0 — arXiv:2504.19413 (SR; public reproduction disputes noted)
- A-MEM (Zettelkasten agentic memory) — arXiv:2502.12110
- HippoRAG — arXiv:2405.14831; HippoRAG 2 — arXiv:2502.14802
- MIRIX (multi-type memory) — arXiv:2507.07957 (deferred: complexity)
- AutoMem — github.com/autoLearnMem/AutoMem (verified repo; SR gains in game
  environments; instrumentation idea adopted, training loop deferred)
- Reflexion — arXiv:2303.11366; Voyager skill library — arXiv:2305.16291
- GraphRAG — arXiv:2404.16130
- Product memory docs: Claude Code memory/CLAUDE.md (docs.claude.com),
  Cursor Memories (docs.cursor.com), Devin Knowledge (docs.devin.ai),
  Windsurf Memories (product docs)

Optimization
- GEPA (reflective prompt evolution) — arXiv:2507.19457
- DSPy — arXiv:2310.03714

Security
- AgentPoison (memory/RAG backdoor) — arXiv:2407.12784
- MINJA (memory injection via interaction) — arXiv:2503.03704

Long context
- NoLiMa (long-context degradation beyond literal matching) — arXiv:2502.05167
- Chroma "context rot" technical report (2025) — research.trychroma.com

Integrity note: "MemPalace"/"AAAK", cited in pre-v3 drafts, could not be
verified to exist and is excluded (§3.3).
```

— end of specification —
