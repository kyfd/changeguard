.PHONY: run test build docker-up docker-down

run:
	go run ./cmd/dbguard

test:
	go test ./...

build:
	go build -trimpath -o bin/dbguard ./cmd/dbguard

docker-up:
	docker compose up -d --build

docker-down:
	docker compose down
