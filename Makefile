.PHONY: up down logs build docker-build test lint migrate seed

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
	go build -o bin/server ./cmd/api

docker-build:
	docker build $(if $(PLATFORM),--platform=$(PLATFORM)) -t feature-flag-service:local .

test:
	go test -race ./...

lint:
	golangci-lint run ./...
	govulncheck ./...
	./scripts/check-no-ratelimit-deps.sh

# cmd/migrate lands in Phase 1 (goose + embed.FS migrations).
migrate:
	@echo "make migrate: cmd/migrate isn't implemented yet — lands in Phase 1" >&2
	@exit 1

# Depends on internal/store/postgres (Phase 1) for the API-key repository.
seed:
	@echo "make seed: internal/store/postgres isn't implemented yet — lands in Phase 1" >&2
	@exit 1
