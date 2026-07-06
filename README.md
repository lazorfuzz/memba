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

### LongMemEval adapter (spec §17.2)

Download the LongMemEval v1 dataset (`longmemeval_s.json` / `_m` / oracle
split) and run it through the public API. Scoring is memory-side **evidence
recall** (gold answer surfaced in the pack) plus the abstention axis
(`*_abs` instances credit `answerability: low` / `missing_evidence`),
reported per question type — a pinned LLM reader for answer-level EM/F1
plugs in on top of the archived packs (§17.1):

```bash
go run ./cmd/mem-bench longmemeval --token "$BENCH_TOK" \
  --file longmemeval_s.json --limit 100 --mode deep -v
# smoke fixture: testdata/bench/lme_sample.json
```

### LLM extractor (spec §10.4)

`extract_cards` jobs run a citation-gated extractor over newly ingested
evidence: candidates whose quotes are not found **verbatim** in the evidence
body are discarded (I2), survivors enter the normal proposal → promotion
gate, capped at 8 candidates/document and a per-namespace daily budget.
Enable by exporting `ANTHROPIC_API_KEY` (config `models.extractor`,
provider `anthropic`; `claude-sonnet-latest` resolves to `claude-sonnet-5`).
Without credentials the worker records the jobs as no-ops. Prompts are
versioned files in `configs/prompts/` (part of the §17.7 scaffold surface).

### Sleep-time consolidation (spec §11)

The worker schedules `consolidate_ns` per namespace on the `--sweep-every`
cadence (set it to ~24h in production). Jobs C1–C7: near-duplicate merge
proposals with `supersedes` links (auto-promotable via P4), byte-stable L1
profile generation served by `GET /v1/profile?level=1`, fact-contradiction
sweeps, co-citation `related_to` link inference, gap mining from
low-answerability queries (report under `reports/…/missing_knowledge.md` in
the object store), failure mining (≥3 same-signature failed runs → gated
gotcha proposal), and decay/dormancy. Every run writes a
`consolidation_runs` stats row.

### Branch verification (spec §9)

Place (or let the git connector maintain) clones under `verify.repo_root`
(default `./data/repos/<repo>`). Cards citing `git://<repo>/<path>` are then
checkable: `POST /v1/verify {"target":"card:<id>","verification_type":
"code_branch_check","repo":"<repo>","branch":"main"}`. Passing checks reset
the card's `verify_by` clock and auto-promote proposals via rule P3; failing
checks move cards to `stale`, and deep queries serve them **flagged, never
silently dropped**.

### Connectors (spec §14.4)

All connectors normalize into `POST /v1/evidence` and are stateless beyond a
cursor; re-runs are cheap thanks to ingest idempotency.

```bash
# git — maintains the clone under verify.repo_root (keeps branch
# verification + the code index live) and ingests diff-driven increments
go run ./connectors/git --repo-url https://github.com/acme/x.git --repo x \
  --namespace /acme/repos/x --token $TOK --branch main --interval 60s

# github — merged PRs (github_pr) + failed workflow runs (ci); GITHUB_TOKEN optional for public repos
go run ./connectors/github --owner acme --repo x --namespace /acme/repos/x --token $TOK --interval 5m

# slack — channel history batches (authority 7; injection/secret scans server-side); needs SLACK_TOKEN
go run ./connectors/slack --channels C0PAY --namespace /acme/team/payments --token $TOK --interval 5m

# localfile — directory trees (docs)
go run ./connectors/localfile --dir ./docs --namespace /acme/repos/x --token $TOK
```

### Curation surface (spec §16)

`GET /ui` serves the memory-health dashboard + review queue (paste an
operator token). Data endpoints: `GET /v1/admin/health` (card freshness,
verify-overdue, proposal queue depth, median time-to-promotion,
low-answerability count) and `GET /v1/cards?status=proposed`.

### Audit mode (spec §8.1, D7)

`POST /v1/query` with `{"mode":"audit","query":"card:<id>"}` returns the full
provenance chain: card/fact, citations with cited-evidence summaries,
verification history, and review decisions.

### Cold start (spec §20)

`POST /v1/admin/bootstrap {"repo":"x","namespace_id":"/acme/repos/x"}` runs
over the git connector's clone: ingests docs/build/CI files, seeds procedure
cards from Makefile targets, branch-verifies them immediately (P3 promotes
code-backed seeds with zero human input), reports gaps, and triggers C2
profile generation. `memctl bootstrap` is the client-side equivalent for a
local checkout.

### InstitutionalBench v1 (spec §17.3)

```bash
go run ./cmd/mem-bench institutional --token $TOK --services 3 --seed 7 -v
```

Deterministic generator (public, per spec — private hash-pinned splits layer
on top) covering the §17.3 task families: onboarding · stale-doc traps (doc
says X, CI says Y — credit requires the current truth) · migration ordering ·
ownership · prior-failure avoidance · forbidden-file (generated code) ·
command recall · abstention. Reported per family against the T4 targets
(§17.5); the current in-repo sample passes all targets at 100%.

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
| P1 hybrid retrieval & workspace | ✅ chunkers, embeddings + HNSW (halfvec, iterative scans), trigram, RRF fusion, reranker cascade, pack budgets, workspace writer + GC, LongMemEval(v1) adapter |
| P2 code awareness & verification | ✅ §9.1 branch checks (file/quote/symbol/commit) + JIT cache + G3 gate; code index (`go/ast` for Go per §9.3, regex fallback elsewhere pending tree-sitter); git/github connectors; `audit` mode |
| P3 cards/proposals/promotion | ✅ full lifecycle: P1–P5 / B1–B5, dedup-at-propose, decay + dormancy sweeps, review API |
| P4 temporal facts & conflicts | ✅ bitemporal facts, `as_of` filtering, supersession, G2/G4 + §8.7 winner rules |
| P5 consolidation & connectors | ✅ sleep-time jobs C1–C7 + stats; LLM extractor with citation gate + caps; secscan + quarantine; git/github(PR+CI)/slack/localfile connectors; health dashboard + review UI; server-side bootstrap |
| P6 beat-SOTA campaign | started: LongMemEval(v1) adapter + InstitutionalBench v1 (generator + runner, T4-checked). Pending: LME-V2/MemoryAgentBench/BEAM adapters, pinned-reader answer scoring, ablation grid (H1–H4), scaffoldopt |
| P7 memory-specialist model | not started (gated on P6 evidence, per spec) |

### Deliberate deviations from the spec (all behind interfaces)

- **Jobs**: a Postgres `FOR UPDATE SKIP LOCKED` queue with the spec's job
  kinds instead of `riverqueue/river`; same coordination semantics, swap is an
  adapter behind `internal/jobs`.
- **Models**: dev/CI defaults are local and deterministic — a feature-hashing
  embedder and a lexical-overlap reranker — so the entire pipeline runs with
  zero external services. Production models (voyage-code-3, cross-encoder,
  extractor LLM) plug in behind `Embedder`/`Reranker` (§14.2); the Voyage
  adapter is included (`VOYAGE_API_KEY`).
- **Object store**: fs backend by default; `backend: s3` enables the
  aws-sdk-go-v2 adapter (MinIO path-style; creds via
  MEMD_S3_ACCESS_KEY/MEMD_S3_SECRET_KEY or the AWS chain).
- **Code index**: Go via stdlib `go/ast` (the spec's prescribed path);
  other languages use the regex extractor until tree-sitter grammars are
  vendored — an accuracy upgrade behind the same `codeindex` interface.
- **Chunking**: block-boundary code chunking (line-capped, file-header
  prefixed); AST-aware boundaries ride the same tree-sitter upgrade.
- **Symbol citations**: §9.1 step B triggers on `git://repo/path#Symbol`
  URI fragments (SourceRef has no dedicated symbol field).
- **Ingest**: chunk+embed run inline (read-your-writes for benchmarks);
  `extract_cards` runs in the background worker via the Anthropic SDK.
- **LongMemEval scoring**: the adapter reports evidence recall + abstention
  (memory-side metrics); answer-level EM/F1 with a pinned reader model is
  the P6 campaign's job.
