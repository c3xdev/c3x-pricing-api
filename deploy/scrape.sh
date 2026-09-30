#!/usr/bin/env bash
# Scrape vendors on a docker compose deployment, each in its own
# short-lived container, for cron.
#
#   deploy/scrape.sh                # aws, azure, gcp
#   deploy/scrape.sh aws azure      # a subset
#
# Why not `docker compose exec api c3x-pricing-api scrape ...`: that runs
# the scrape inside the api container and under its memory limit (1 GB by
# default). An AWS scrape needs more, so it is OOM-killed, and because it
# shares the container it can take the API down with it. Here each vendor
# gets its own container with SCRAPE_MEM_LIMIT (default 3g, plus swap), so
# a failure affects only that scrape, and one vendor failing does not stop
# the others. Exits non-zero if any vendor failed.
#
# Cron (VPS clock in UTC), e.g. in the deploy user's crontab:
#   0 3 * * * /opt/c3x-pricing-api/deploy/scrape.sh >> /var/log/c3x-pricing-scrape.log 2>&1
set -uo pipefail

COMPOSE_DIR="${COMPOSE_DIR:-$(cd "$(dirname "$0")/.." && pwd)}"
MEM="${SCRAPE_MEM_LIMIT:-3g}"
SWAP="${SCRAPE_MEM_SWAP:-6g}"
GOMEM="${SCRAPE_GOMEMLIMIT:-2500MiB}"
CONC="${SCRAPE_CONCURRENCY:-1}"
VENDORS=("$@")
[ ${#VENDORS[@]} -eq 0 ] && VENDORS=(aws azure gcp)

cd "$COMPOSE_DIR" || { echo "[scrape] $COMPOSE_DIR not found" >&2; exit 1; }
set -a; . ./.env; set +a

project=$(docker compose ps --format '{{.Project}}' 2>/dev/null | head -1)
project=${project:-$(basename "$COMPOSE_DIR")}
image="${project}-api"
network="${project}_default"
url="postgres://${POSTGRES_USER:-c3x}:${POSTGRES_PASSWORD}@db:5432/${POSTGRES_DB:-c3x_pricing}?sslmode=disable"

failed=()
for v in "${VENDORS[@]}"; do
  name="c3x-scrape-$v"
  docker rm -f "$name" >/dev/null 2>&1 || true
  echo "[scrape] $(date -u +%FT%TZ) $v: start"
  if docker run --rm --name "$name" --network "$network" \
      --memory "$MEM" --memory-swap "$SWAP" \
      -e DATABASE_URL="$url" -e GCP_API_KEY="${GCP_API_KEY:-}" \
      -e GOMEMLIMIT="$GOMEM" -e SCRAPE_CONCURRENCY="$CONC" \
      "$image" scrape --vendor "$v"; then
    echo "[scrape] $(date -u +%FT%TZ) $v: ok"
  else
    echo "[scrape] $(date -u +%FT%TZ) $v: FAILED (exit $?)" >&2
    failed+=("$v")
  fi
done

if [ ${#failed[@]} -gt 0 ]; then
  echo "[scrape] failed: ${failed[*]}" >&2
  exit 1
fi
