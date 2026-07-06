# memba

**An institutional memory kernel for long-running AI coding agents.**

memba ingests your organization's evidence — code, pull requests, docs, CI runs, incidents, chat, prior agent runs — distills it into *verified, citable memory*, and serves it back to agents as compact context blocks or mounted file workspaces they can `ls`, `grep`, and read.

It is not RAG with a REST wrapper. It is a small, strict, verifiable evidence engine:

```text
institutional memory =
    raw verbatim evidence            (immutable, always)
  + validated, cited memory cards    (nothing uncited survives)
  + bitemporal facts                 ("what did we believe on May 1?")
  + branch-verified code claims      (checked against YOUR branch, just-in-time)
  + navigable evidence workspaces    (agents read files, not prompts)
  + recorded memory actions          (memory behavior is itself measurable)
```

One Go binary. One Postgres. One object store. That's the whole footprint.

---

## Why

Long-running coding agents fail in organizations for a predictable reason: the knowledge they need is scattered, stale, permissioned, and contradictory. A retrieval layer that treats this as "vector search over a database" produces confident answers built on outdated docs, chat rumors, and other agents' failed experiments.

memba is built on three findings from the 2025–2026 memory literature:

1. **Agentic file exploration beats one-shot retrieval** on hard, long-horizon memory tasks — so the mounted **workspace is the default interface**, pre-curated with verified cards instead of raw logs.
2. **Unverified memory is worse than no memory.** Open-gate memory stores accumulate "hallucinations of the past" and are a demonstrated attack surface (MINJA, AgentPoison) — so *every* write is gated, every code claim is branch-verified, and every active memory sits on a decay clock.
3. **Long context doesn't replace memory.** Million-token windows degrade on distractor-heavy input and carry no ACLs, no verification, no staleness flags — so memba treats long context and prompt caching as delivery mechanisms, not competitors.

## What makes it different

| | Typical RAG memory | memba |
|---|---|---|
| Writes | append-only, open gate | **propose → validate → promote** — deterministic rules (P1–P5), hard blockers (B1–B5); agents *cannot* write directly to active memory |
| Code claims | served as-is | **branch-verified just-in-time**: file exists · quote still holds (exact → fuzzy → embedding) · symbol still defined (go/ast index) · commit is an ancestor |
| Staleness | forever-fresh fiction | **verification clocks**: 28/90/180-day TTLs; expired memory degrades to `stale` (served *flagged*, never silently dropped), then `dormant` |
| Citations | optional | **mandatory** (invariant I2): a memory without verbatim citations cannot leave `proposed` — extractor output that can't quote its source is discarded |
| Contradictions | last-write-wins | **bitemporal facts** + deterministic conflict resolution (branch-verified > runtime-validated > source authority > recency), losers shown with the rule that decided |
| Unanswerable questions | confident nonsense | **answerability scoring + `missing_evidence.md`** — the system says "institutional memory doesn't establish this" |
| Poisoning | not modeled | injection scanning at ingest, quarantine, per-source diversity caps, compiled-only always-on layers, provenance-gated promotion (defenses D1–D7) |
| Tenancy | a `WHERE` clause you hope holds | `tenant_id` on every table, every query; property-tested (10k-query fuzz, zero tolerance) |

## The memory stack

```text
L0  agent boot context      ≤ 700 tok   compiled rules of engagement; cache-stable
L1  repo operating profile  ≤ 2k tok    a perfect CLAUDE.md — except every line cites
                                        a card on a verification clock
L2  scoped memory cards     on demand   "don't edit *_pb.go; edit the proto, run make proto"
L3  deep hybrid retrieval   on demand   6 retrievers → RRF → rerank → deterministic gates
L4  evidence workspace      DEFAULT     a .memworkspace/ file tree: gotchas.md,
    for coding tasks                    procedures.md, facts.jsonl, labeled raw spans,
                                        missing_evidence.md, writable scratch/
```

```text
     Git · PRs · Docs · Slack · CI · Incidents · Agent runs
                          │  connectors
                          ▼
┌───────────────────────── memd (one Go binary) ─────────────────────────┐
│ ingest → raw evidence (immutable) → chunk/embed/index → extractor      │
│                                          │        (LLM, citation-gated)│
│                                          ▼                             │
│                            PROPOSED cards & facts                      │
│                                          │                             │
│              promotion rules (P1–P5 / blockers B1–B5)                  │
│                                          ▼                             │
│ query → authz → R1–R6 retrievers → RRF → rerank → gates ──► evidence   │
│         (FTS·trigram·vector·symbol·facts·links)  G1–G5      pack / L4  │
│                                                                        │
│ background: JIT verification · decay clocks · sleep-time consolidation │
│             (dedup/merge · profile build · conflict sweep · link       │
│              inference · gap mining · failure mining)                  │
└────────────────────────────────────────────────────────────────────────┘
                          │  mem.search / mem.workspace / mem.open /
                          ▼  mem.log / mem.propose / mem.verify / mem.invalidate
                 long-running coding agents
```

Seven agent tools, no more. Everything runs behind one `/v1` API — and the benchmark harness drives **the same API as production** (invariant I5; no benchmark-only paths).

## Quickstart

```bash
# 1. Postgres with pgvector
docker run -d --name memba-pg -p 5432:5432 \
  -e POSTGRES_USER=memba -e POSTGRES_PASSWORD=memba -e POSTGRES_DB=memba \
  pgvector/pgvector:pg16

export MEMD_PG_DSN='postgres://memba:memba@localhost:5432/memba?sslmode=disable'
export MEMD_TOKEN_SECRET='dev-secret-change-me'

# 2. Migrate + run (dev defaults need zero external model services)
go run ./cmd/memctl migrate
go run ./cmd/memd --config configs/memd.yaml --role all
```

Or `docker compose up --build` for the full Postgres + MinIO + memd stack.

**Sixty-second tour** (second terminal):

```bash
TOK=$(go run ./cmd/memctl token --tenant acme --principal agent:coder-1 --subjects team:payments)

# ingest evidence
curl -s -X POST localhost:8080/v1/evidence -H "Authorization: Bearer $TOK" \
  -H 'Content-Type: application/json' -d '{
    "namespace_id":"/acme/payments/repos/billing-api",
    "source_type":"doc","source_uri":"doc://billing-api/README.md",
    "body":"Run `make test` before pushing. Never edit generated invoice_status.pb.go."}'

# ask
curl -s -X POST localhost:8080/v1/query -H "Authorization: Bearer $TOK" \
  -H 'Content-Type: application/json' -d '{
    "namespace_hints":["/acme/payments/repos/billing-api"],
    "query":"what do I run before pushing?","mode":"scoped"}' | jq .

# propose a memory — two independent sources auto-promote it (rule P4);
# a chat-only citation would be blocked (B2); zero citations is a 422 (B4)
curl -s -X POST localhost:8080/v1/cards -H "Authorization: Bearer $TOK" \
  -H 'Content-Type: application/json' -d '{
    "namespace_id":"/acme/payments/repos/billing-api","card_type":"gotcha",
    "title":"Never edit invoice_status.pb.go",
    "body":"It is generated; edit the proto and run make proto.",
    "subject":"billing-api",
    "structured":{"trigger":"editing *_pb.go","severity":"high","consequence":"overwritten"},
    "source_refs":[{"source_uri":"doc://billing-api/README.md"},
                   {"source_uri":"git://billing-api/src/invoice/invoice_status.pb.go"}]}' | jq .

# mount a workspace for a coding task (the default interface for agents)
curl -s -X POST localhost:8080/v1/query -H "Authorization: Bearer $TOK" \
  -H 'Content-Type: application/json' -d '{
    "namespace_hints":["/acme/payments/repos/billing-api"],
    "query":"add a new invoice status safely","goal_type":"coding_change",
    "mode":"workspace"}' | jq '{workspace_uri, cards: [.cards[].title]}'
```

Open **`http://localhost:8080/ui`** for the memory-health dashboard and review queue.

### Watch verification catch a stale memory

Point a card at real code (clones live under `verify.repo_root`, maintained by the git connector):

```bash
curl -s -X POST localhost:8080/v1/verify -H "Authorization: Bearer $TOK" \
  -H 'Content-Type: application/json' \
  -d '{"target":"card:<id>","verification_type":"code_branch_check",
       "repo":"billing-api","branch":"main"}' | jq .
```

`passed` → the card's verification clock resets and proposals auto-promote (P3). Then someone refactors the cited code, and the same check returns:

```json
{"result":"failed","per_ref":[{"check":"quote_holds","result":"failed","detail":"content_changed"}]}
```

The card degrades to `stale`, and deep queries serve it **flagged** — `"This memory may be stale: … quote_holds failed for git://billing-api/src/invoice/status.go [checked against branch main]"` — because a wrong-but-flagged memory teaches the agent what changed; a silently dropped one teaches nothing.

## Connectors

All connectors normalize into `POST /v1/evidence`, are stateless beyond a cursor, and re-run for free thanks to content-hash idempotency:

```bash
# git — maintains blobless clones under verify.repo_root (keeps branch
# verification and the code index live); diff-driven incremental ingest
go run ./connectors/git --repo-url https://github.com/acme/x.git --repo x \
  --namespace /acme/repos/x --token $TOK --branch main --interval 60s

# github — merged PRs with review verdicts + failed CI runs (feeds failure mining)
go run ./connectors/github --owner acme --repo x --namespace /acme/repos/x --token $TOK --interval 5m

# slack — channel history (authority 7: chat can never self-promote)
go run ./connectors/slack --channels C0PAY --namespace /acme/team/payments --token $TOK --interval 5m

# localfile — any directory of docs
go run ./connectors/localfile --dir ./docs --namespace /acme/repos/x --token $TOK
```

**Cold start (§20):** `POST /v1/admin/bootstrap {"repo":"x","namespace_id":"/acme/repos/x"}` ingests docs/build/CI files from the clone, seeds procedure cards from Makefile targets, and branch-verifies them immediately — code-backed seeds promote with zero human input.

## While you sleep

Nightly per-namespace consolidation (every output re-enters the promotion gate — consolidation never silently rewrites active memory):

- **dedup/merge** — near-duplicate cards become one merged proposal citing the union of sources, with `supersedes` links
- **profile build** — regenerates the L1 operating profile, byte-stable so prompt caches only bust on real change
- **conflict sweep** — contradicting facts get flagged and queued for re-verification
- **link inference** — `related_to` links from co-citation (links route search; they are never truth)
- **gap mining** — clustered low-answerability queries become a per-namespace *missing knowledge* report
- **failure mining** — the same failure signature in ≥ 3 agent runs becomes a gotcha proposal (still gated: agent evidence can't self-promote)
- **decay** — expired clocks re-verify or go stale; untouched stale memory goes dormant

## Benchmarks

`mem-bench` drives the public API only. Metrics are always reported per axis, never one collapsed number. Current in-repo sample fixtures (local deterministic models, single machine):

| Suite | Result |
|---|---|
| InstitutionalBench v1 (8 task families incl. stale-doc traps, forbidden files, prior failures) | 24/24, all T4 targets met, abstention 6/6 |
| LongMemEval v1 adapter (evidence recall + abstention) | sample fixture 3/3 |
| Smoke suite | 5/5 retrieval, 1/1 abstention, ~350 evidence tok/pack, ~6 ms/query |

```bash
TOK=$(go run ./cmd/memctl token --tenant bench --principal svc:mem-bench)
go run ./cmd/mem-bench institutional --token $TOK --services 3 --seed 7 -v
go run ./cmd/mem-bench longmemeval  --token $TOK --file longmemeval_s.json --limit 100
go run ./cmd/mem-bench run          --token $TOK --cases testdata/bench/smoke.jsonl -v
```

The InstitutionalBench generator is deterministic and public (per spec); private hash-pinned splits layer on top for real campaigns. Sample numbers above measure **evidence recall** — whether memory surfaced the right, current, cited material — which is the memory system's job; answer-level EM/F1 with a pinned reader model is the beat-SOTA campaign's job (§17.5 targets: LME-V2 ≥ 75%, abstention ≥ 90%, InstitutionalBench T4).

## API

```text
POST /v1/evidence                     ingest (idempotent; secret + injection scans)
POST /v1/query                        boot | scoped | deep | workspace | audit | benchmark
GET  /v1/profile?level=0|1            L0/L1, ETag'd and cache-stable
GET  /v1/workspaces/{id}[/archive]    manifest / tar.zst
POST /v1/cards                        propose (422 without citations)
POST /v1/cards/{id}/review            approve | reject | invalidate
POST /v1/verify                       run a verification now
POST /v1/facts · /v1/actions          facts; memory-action instrumentation
POST /v1/admin/bootstrap              cold start a repo
GET  /v1/admin/health · /ui           dashboard JSON · curation UI
```

RFC 7807 errors; HMAC bearer tokens; per-principal rate limiting; 403 is never distinguishable from 404 (no existence oracle). Every derived object's ACL is the **intersection** of its sources' ACLs — memory can never widen access to its evidence.

## Repository layout

```text
cmd/            memd (server+workers) · memctl (admin CLI) · mem-bench (harness)
pkg/            memory (types) · client (Go client) · evalapi (benchmark iface)
internal/       api · authz · ingest · chunk · codeindex · embed · extract ·
                cards · verify · retrieve · rerank · gates · pack · workspace ·
                consolidate · jobs · store/postgres · objstore · secscan
connectors/     git · github · slack · localfile
migrations/     goose SQL (full schema: evidence, chunks, cards, facts,
                verifications, actions, workspaces, jobs, consolidation_runs)
configs/        memd.yaml · versioned extraction prompts
```

## Testing

```bash
go test ./...                                        # unit: promotion-rule matrix, 10k-iteration
                                                     # ACL property test, injection corpus, RRF,
                                                     # gates, chunkers, go/ast symbols, rate limiter
MEMBA_TEST_PG_DSN=postgres://… go test ./...         # + integration: tenant isolation, ACL-in-SQL,
                                                     # ingest idempotency, extractor E2E (fake LLM),
                                                     # full consolidation run w/ idempotence check
```

Security invariants are tested as invariants: zero cross-tenant hits, zero ACL leaks, injection corpus 100% quarantined, secrets span-redacted (`⟦REDACTED:aws_key⟧`) before anything reaches an embedding or a pack.

## Configuration

Everything lives in `configs/memd.yaml` — budgets, TTLs, promotion thresholds, RRF constants, scan rules. Every query logs `config_hash = sha256(effective config)` so results are reproducible. Dev defaults run with **zero external services** (deterministic hash embedder + lexical reranker); production plugs in `voyage-code-3`, a cross-encoder, and an Anthropic extractor model behind interfaces — set `ANTHROPIC_API_KEY` and the citation-gated extractor turns on by itself.

## Status

Built to the [v3 specification](memba_v3_spec.md), in its phase order. **P0–P5 complete**; P6 (beat-SOTA campaign) started.

| Phase | |
|---|---|
| P0 contracts & skeleton | ✅ |
| P1 hybrid retrieval & workspace | ✅ RRF + rerank cascade, budgeted packs, L4 writer, LongMemEval adapter |
| P2 code awareness & verification | ✅ branch checks (file/quote/symbol/commit), go/ast code index, JIT cache, audit mode |
| P3 cards, proposals, promotion | ✅ P1–P5/B1–B5, dedup-at-propose, decay + dormancy, review API |
| P4 temporal facts & conflicts | ✅ bitemporal facts, `as_of` queries, deterministic conflict rules |
| P5 consolidation, security, connectors | ✅ C1–C7 jobs, LLM extractor, poisoning defenses, connectors, dashboard, bootstrap |
| P6 beat-SOTA campaign | ▶ InstitutionalBench v1 + LME adapter shipped; LME-V2/MemoryAgentBench/BEAM adapters, pinned-reader scoring, ablations pending |
| P7 memory-specialist model | gated on P6 evidence, per spec |

**Known deviations** (all behind interfaces, documented in-code): jobs use a Postgres `FOR UPDATE SKIP LOCKED` queue with river-compatible semantics; non-Go languages use regex symbol extraction until tree-sitter grammars are vendored; dev model defaults are local and deterministic.

---

*The spec's design bet, in one line: agents don't need a bigger context window — they need an org-shaped memory that can prove what it says, admit what it doesn't know, and expire what stopped being true.*
