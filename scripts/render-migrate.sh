#!/usr/bin/env bash
# Run the migrate binary against a deployed Render Postgres, from outside
# Render. Needed because the free web service plan rejects preDeployCommand,
# and Shell access requires a paid plan (and wouldn't work anyway — the
# runtime image is distroless, no /bin/sh).
#
# Same code path as docker-compose's `migrate`/`seed` targets: an entrypoint
# override running /app/migrate directly, no shell involved on either side.
#
# Usage:
#   DATABASE_URL='postgres://user:pass@host:port/db' ./scripts/render-migrate.sh up
#   DATABASE_URL='...' ./scripts/render-migrate.sh seed
#
# Get DATABASE_URL from: Render dashboard -> ffs-postgres -> Connect ->
# External Database URL.
set -euo pipefail

CMD="${1:?usage: render-migrate.sh <up|seed|status>}"

if [ -z "${DATABASE_URL:-}" ]; then
  echo "DATABASE_URL is not set — copy it from Render dashboard -> ffs-postgres -> Connect -> External Database URL" >&2
  exit 1
fi

echo "Building the image locally (same Dockerfile Render builds)..."
docker build -t feature-flag-service:migrate .

echo "Running migrate ${CMD} against the deployed database..."
docker run --rm \
  --entrypoint /app/migrate \
  -e "ENV=prod" \
  -e "PORT=8080" \
  -e "DATABASE_URL=${DATABASE_URL}?sslmode=require" \
  -e "REDIS_ADDR=unused:0" \
  -e "LOG_LEVEL=info" \
  feature-flag-service:migrate "${CMD}"

if [ "$CMD" = "seed" ]; then
  echo
  echo "Seeded. The running Render service still holds its OLD key map in"
  echo "memory (keys load once at boot) — go to the Render dashboard and"
  echo "trigger Manual Deploy > Restart Service, or the new keys will 401."
fi
