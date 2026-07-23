.PHONY: up down logs build docker-build test lint migrate seed demo-restart demo-shutdown

# Build metadata stamped into the binary via -ldflags and surfaced at GET /.
# BUILD_TIME is the commit timestamp, not `now`: it is reproducible for a given
# commit, so an unchanged tree still hits the Docker build cache.
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GIT_SHA    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell git log -1 --format=%cI 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -s -w -X main.version=$(VERSION) -X main.gitSHA=$(GIT_SHA) -X main.buildTime=$(BUILD_TIME)

# Exported so every recipe — and every script a recipe calls — rebuilds with
# the same metadata. Without this a demo script's `docker compose --build`
# silently re-stamps the image as dev/unknown, and GET / starts lying.
export VERSION
export GIT_SHA
export BUILD_TIME

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
	go build -ldflags "$(LDFLAGS)" -o bin/server  ./cmd/api
	go build -ldflags "$(LDFLAGS)" -o bin/migrate ./cmd/migrate

docker-build:
	docker build $(if $(PLATFORM),--platform=$(PLATFORM)) \
		--build-arg VERSION="$(VERSION)" \
		--build-arg GIT_SHA="$(GIT_SHA)" \
		--build-arg BUILD_TIME="$(BUILD_TIME)" \
		-t feature-flag-service:local .

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

# Proves SIGTERM drains in-flight requests rather than cutting them.
demo-shutdown:
	./scripts/demo-shutdown.sh
