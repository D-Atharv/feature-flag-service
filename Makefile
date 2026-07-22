.PHONY: up down logs build docker-build test lint migrate seed demo-restart

# Override at invocation if you ever need a specific target platform, e.g.
# `make docker-build PLATFORM=linux/amd64`. Not baked in by default: Render
# builds the Dockerfile itself on its own amd64 infra, so there's no local
# manual-build-and-push step that needs this.
PLATFORM ?=

up:
	docker compose up --build -d

down:
	docker compose down

logs:
	docker compose logs -f

build:
	go build -o bin/server  ./cmd/api
	go build -o bin/migrate ./cmd/migrate

docker-build:
	docker build $(if $(PLATFORM),--platform=$(PLATFORM)) -t feature-flag-service:local .

test:
	go test -race ./...

lint:
	golangci-lint run ./...
	govulncheck ./...
	./scripts/check-no-ratelimit-deps.sh

# --build: docker compose run reuses whatever image is already tagged, which
# silently goes stale the moment the Dockerfile or source changes.
migrate:
	docker compose run --build --rm --entrypoint /app/migrate api up

seed:
	docker compose run --build --rm --entrypoint /app/migrate api seed

# Proves the assignment's third requirement: rate limit state survives a
# restart. Pass DEMO_KEY to reuse an existing key; otherwise the script seeds a
# fresh one and restarts the API so it picks the new keys up.
demo-restart:
	./scripts/demo-restart.sh
