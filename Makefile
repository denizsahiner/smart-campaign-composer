.PHONY: test test-go test-db-less test-web up down llm-try

# Postgres from docker-compose.yml, used by the integration tests. Override
# TEST_DATABASE_URL to point at your own server and skip the container.
TEST_DB_URL ?= postgres://composer:composer@localhost:5433/composer?sslmode=disable

test: test-go test-web

# Runs every Go test, including the Postgres integration tests. If TEST_DATABASE_URL is
# unset, the compose postgres is started first (and stopped again if we started it).
# Each package creates and drops its own throwaway database, so no data is touched.
test-go:
	@if [ -n "$$TEST_DATABASE_URL" ]; then \
		go test ./... ; \
	else \
		was_running=$$(docker compose ps --status running --services postgres 2>/dev/null); \
		docker compose up -d --wait postgres || { echo "make test needs Docker for the Postgres integration tests (or set TEST_DATABASE_URL)"; exit 1; }; \
		TEST_DATABASE_URL="$(TEST_DB_URL)" go test ./... ; rc=$$?; \
		[ -n "$$was_running" ] || docker compose stop postgres >/dev/null; \
		exit $$rc; \
	fi

# Go tests without a database: integration tests are skipped.
test-db-less:
	go test ./...

# Frontend tests (Vitest, jsdom): no browser or running API needed. Installs the
# locked dependencies on first run.
test-web:
	cd web && { [ -d node_modules ] || npm ci --no-audit --no-fund; } && npm test

up:
	docker compose up --build

down:
	docker compose down

# Try both LLM steps for one brief against the API in .env, e.g.
#   make llm-try BRIEF="İstanbul'da son 30 günde sepetini terk edenlere %30 indirim"
# No local Go needed; runs in a Go container. Without a key in .env, uses the mock.
llm-try:
	docker run --rm -v "$(CURDIR)":/src -w /src --env-file .env golang:1.26-alpine go run ./cmd/app llm-try "$(BRIEF)"
