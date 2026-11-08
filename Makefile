.PHONY: test up down llm-try

test:
	go test ./...

up:
	docker compose up --build

down:
	docker compose down

# Try both LLM steps for one brief against the API in .env, e.g.
#   make llm-try BRIEF="İstanbul'da son 30 günde sepetini terk edenlere %30 indirim"
# No local Go needed; runs in a Go container. Without a key in .env, uses the mock.
llm-try:
	docker run --rm -v "$(CURDIR)":/src -w /src --env-file .env golang:1.26-alpine go run ./cmd/app llm-try "$(BRIEF)"
