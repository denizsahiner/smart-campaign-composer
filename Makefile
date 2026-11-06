.PHONY: test up down

test:
	go test ./...

up:
	docker compose up --build

down:
	docker compose down
