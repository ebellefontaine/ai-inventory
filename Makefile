.PHONY: build test lint check run db-up db-down

build:
	go build -o bin/aiinv ./cmd/aiinv

test:
	go test ./... -race -count=1

lint:
	go vet ./...
	@gofmt -l -d .
	@test -z "$$(gofmt -l .)" || { echo "gofmt: files above need formatting"; exit 1; }

check: lint test

run: build
	./bin/aiinv

db-up:
	docker compose up -d postgres

db-down:
	docker compose down -v
