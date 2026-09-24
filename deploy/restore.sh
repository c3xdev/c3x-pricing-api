#!/usr/bin/env bash
# Restore the pricing database from an archive written by deploy/backup.sh.
#
# DESTRUCTIVE: replaces the contents of the database with the archive.
# Stops the api (and the scraper sidecar, if present) so nothing writes
# during the restore, restores in a single transaction (all or nothing),
# then starts them again.
#
# Environment: COMPOSE_DIR, DB_SERVICE as in backup.sh.
#
# Usage: deploy/restore.sh <archive.dump> [--yes]
set -euo pipefail

COMPOSE_DIR="${COMPOSE_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
DB_SERVICE="${DB_SERVICE:-db}"

archive="${1:-}"
confirm="${2:-}"
if [ -z "$archive" ] || [ ! -f "$archive" ]; then
  echo "usage: $0 <archive.dump> [--yes]" >&2
  exit 2
fi
archive="$(cd "$(dirname "$archive")" && pwd)/$(basename "$archive")"

log() { echo "[restore] $(date -u +%Y-%m-%dT%H:%M:%SZ) $*"; }

cd "$COMPOSE_DIR"

if [ "$confirm" != "--yes" ]; then
  printf 'This REPLACES the database in %s (service %s) with\n  %s\nType "restore" to continue: ' \
    "$COMPOSE_DIR" "$DB_SERVICE" "$archive"
  read -r answer
  [ "$answer" = "restore" ] || { echo "aborted"; exit 1; }
fi

log "verifying archive"
docker compose exec -T "$DB_SERVICE" pg_restore -f /dev/null < "$archive"

# Stop writers. The scraper only exists with the scraper overlay; stopping
# a service that is not defined or not running is not an error here.
stopped=()
for svc in api scraper; do
  if docker compose ps --status running --services 2>/dev/null | grep -qx "$svc"; then
    log "stopping $svc"
    docker compose stop "$svc"
    stopped+=("$svc")
  fi
done
restart_stopped() {
  for svc in "${stopped[@]+"${stopped[@]}"}"; do
    log "starting $svc"
    docker compose start "$svc" || log "WARNING: could not start $svc" >&2
  done
}
trap restart_stopped EXIT

log "restoring $archive"
# --clean --if-exists drops each object before recreating it;
# --single-transaction makes the restore all-or-nothing;
# --no-owner so a dump taken under another role name still restores.
docker compose exec -T "$DB_SERVICE" sh -c \
  'exec pg_restore -U "$POSTGRES_USER" -d "$POSTGRES_DB" --clean --if-exists --no-owner --single-transaction --exit-on-error' \
  < "$archive"

log "restore complete"
