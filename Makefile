GO ?= go

.PHONY: build test vet migrate run bench-smoke compose-up compose-down

build:
	$(GO) build ./...
	$(GO) build -o bin/memd ./cmd/memd
	$(GO) build -o bin/memctl ./cmd/memctl
	$(GO) build -o bin/mem-bench ./cmd/mem-bench
	$(GO) build -o bin/localfile ./connectors/localfile

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

# Requires MEMD_PG_DSN
migrate:
	$(GO) run ./cmd/memctl migrate

# Requires MEMD_PG_DSN and MEMD_TOKEN_SECRET
run:
	$(GO) run ./cmd/memd --config configs/memd.yaml --role all

# End-to-end benchmark smoke against a running memd (I5: same /v1 API).
# Usage: make bench-smoke MEMBA_TOKEN=<token>
bench-smoke:
	$(GO) run ./cmd/mem-bench run --base http://localhost:8080 \
		--token $(MEMBA_TOKEN) --cases testdata/bench/smoke.jsonl -v

compose-up:
	docker compose up -d --build

compose-down:
	docker compose down -v
