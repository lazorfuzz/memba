# memba — Institutional Memory Kernel

memba is an institutional memory system for long-running AI coding agents. It
ingests an organization's evidence (code, PRs, docs, chat, CI runs, incident
reports, prior agent runs), distills it into **verified, citable memory**, and
serves it back to agents as compact evidence packs or mounted file workspaces.

This repository implements the v3 specification (`memba_v3_spec.md`), built in
its phase order (§21). Current coverage: **Phase 0 complete, plus the
deterministic core of Phases 1–4** — see *Status* below.

## Design invariants (spec §2)

```text
I1. Raw evidence is immutable and verbatim.
I2. Every durable derived memory cites raw evidence.
I3. Code memories are branch-verified before serving, or flagged stale.
I4. Agents PROPOSE memory; only memd PROMOTES it (deterministic rules).
I5. Benchmarks exercise the same public /v1 API as production.
I6. Every active memory has a verification clock (verify_by).
I7. Retrieved evidence is data, not instructions.
```

## Stack (spec §4.1)

```text
One Go service        memd          API + ingest + retrieval + verification + jobs
One database          PostgreSQL 16 + pgvector (halfvec HNSW) + pg_trgm
One object store      fs backend (dev default) / S3-compatible behind an interface
One workspace format  .memworkspace/  (tar.zst export)
One public API        /v1
```

## Quick start

```bash
# 1. Postgres with pgvector
docker run -d --name memba-pg -p 5432:5432 \
  -e POSTGRES_USER=memba -e POSTGRES_PASSWORD=memba -e POSTGRES_DB=memba \
  pgvector/pgvector:pg16

export MEMD_PG_DSN='postgres://memba:memba@localhost:5432/memba?sslmode=disable'
export MEMD_TOKEN_SECRET='dev-secret-change-me'

# 2. Migrate + run
go run ./cmd/memctl migrate
go run ./cmd/memd --config configs/memd.yaml --role all

# 3. Mint a token, create a namespace, ingest, query
TOK=$(go run ./cmd/memctl token --tenant acme --principal agent:coder-1 --subjects team:payments)
go run ./cmd/memctl namespace --token "$TOK" --id /acme/payments/repos/billing-api --kind repo
curl -s -X POST localhost:8080/v1/evidence -H "Authorization: Bearer $TOK" \
  -H 'Content-Type: application/json' -d '{
    "namespace_id":"/acme/payments/repos/billing-api",
    "source_type":"doc","source_uri":"doc://billing-api/README.md",
    "body":"Run `make test` before pushing."}'
curl -s -X POST localhost:8080/v1/query -H "Authorization: Bearer $TOK" \
  -H 'Content-Type: application/json' -d '{
    "namespace_hints":["/acme/payments/repos/billing-api"],
    "query":"what do I run before pushing?","mode":"scoped"}'
```

Or `docker compose up --build` (Postgres + MinIO + memd).

### Benchmark smoke (I5: drives the public API)

```bash
BENCH_TOK=$(go run ./cmd/memctl token --tenant bench --principal svc:mem-bench)
go run ./cmd/mem-bench run --token "$BENCH_TOK" --cases testdata/bench/smoke.jsonl -v
```

### Branch verification (spec §9)

Place (or let the git connector maintain) clones under `verify.repo_root`
(default `./data/repos/<repo>`). Cards citing `git://<repo>/<path>` are then
checkable: `POST /v1/verify {"target":"card:<id>","verification_type":
"code_branch_check","repo":"<repo>","branch":"main"}`. Passing checks reset
the card's `verify_by` clock and auto-promote proposals via rule P3; failing
checks move cards to `stale`, and deep queries serve them **flagged, never
silently dropped**.

## Layout

Matches spec §14.1: `cmd/{memd,memctl,mem-bench}`, `pkg/{memory,client,evalapi}`,
`internal/{api,authz,ingest,chunk,embed,cards,facts→cards,verify,retrieve,rerank,
gates,pack,workspace,actions,jobs,store,objstore,secscan,config,tokens}`,
`connectors/localfile`, `migrations/`, `configs/`, `testdata/`.

## Testing

```bash
go test ./...                                    # unit (promotion, ACL property, secscan, RRF, gates, chunkers)
MEMBA_TEST_PG_DSN=postgres://… go test ./internal/store/postgres   # integration (idempotency, tenant isolation, ACL-in-SQL, job queue)
```

## Status vs. the v3 roadmap (spec §21)

| Phase | State |
|---|---|
| P0 contracts & skeleton | ✅ /v1 API, migrations, authz, raw evidence + idempotency, FTS retrieval, local-file connector, mem-bench skeleton, ACL property + tenant-isolation tests |
| P1 hybrid retrieval & workspace | ✅ chunkers, embeddings + HNSW (halfvec, iterative scans), trigram, RRF fusion, reranker cascade, pack budgets, workspace writer + GC. Public-benchmark adapters (LongMemEval) pending |
| P2 code awareness & verification | ✅ §9.1 branch checks (file/quote/commit) + JIT cache + G3 gate over local clones. Pending: tree-sitter code index, git/github connectors, `audit` mode |
| P3 cards/proposals/promotion | ✅ full lifecycle: P1–P5 / B1–B5, dedup-at-propose, decay + dormancy sweeps, review API |
| P4 temporal facts & conflicts | ✅ bitemporal facts, `as_of` filtering, supersession, G2/G4 + §8.7 winner rules |
| P5 consolidation & connectors | partial: secscan (secrets + injection) + quarantine shipped; sleep-time jobs C1–C7, remaining connectors, dashboard pending |
| P6 beat-SOTA campaign | not started (needs P5) |

### Deliberate deviations from the spec (all behind interfaces)

- **Jobs**: a Postgres `FOR UPDATE SKIP LOCKED` queue with the spec's job
  kinds instead of `riverqueue/river`; same coordination semantics, swap is an
  adapter behind `internal/jobs`.
- **Models**: dev/CI defaults are local and deterministic — a feature-hashing
  embedder and a lexical-overlap reranker — so the entire pipeline runs with
  zero external services. Production models (voyage-code-3, cross-encoder,
  extractor LLM) plug in behind `Embedder`/`Reranker` (§14.2); the Voyage
  adapter is included (`VOYAGE_API_KEY`).
- **Object store**: fs backend by default; S3/MinIO is an adapter behind
  `objstore.Store`.
- **Chunking**: block-boundary code chunking (line-capped, file-header
  prefixed); AST/tree-sitter chunking lands with `internal/codeindex` (P2).
- **Ingest**: chunk+embed run inline (read-your-writes for benchmarks);
  `extract_cards` is enqueued for an offline extractor worker.
