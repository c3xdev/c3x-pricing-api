#!/usr/bin/env bash
# Back up the pricing database from the docker compose `db` service.
#
# Writes a pg_dump custom-format archive (compressed, restorable with
# deploy/restore.sh or pg_restore) to $BACKUP_DIR as
# c3x_pricing-<UTC timestamp>.dump, verifies it is readable, then deletes
# archives older than $RETENTION_DAYS. A failed or unreadable dump is
# removed and the script exits non-zero, and old archives are only pruned
# after a good one exists, so a broken run never eats the last good backup.
#
# Environment:
#   COMPOSE_DIR     directory holding docker-compose.yml and .env
#                   (default: the repo root this script lives in)
#   BACKUP_DIR      where archives go (default: /var/backups/c3x-pricing)
#   RETENTION_DAYS  delete archives older than this many days (default: 7)
#   DB_SERVICE      compose service name of Postgres (default: db)
#
# Usage: deploy/backup.sh      (see deploy/README.md for the cron line)
set -euo pipefail

COMPOSE_DIR="${COMPOSE_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
BACKUP_DIR="${BACKUP_DIR:-/var/backups/c3x-pricing}"
RETENTION_DAYS="${RETENTION_DAYS:-7}"
DB_SERVICE="${DB_SERVICE:-db}"

case "$RETENTION_DAYS" in
  ''|*[!0-9]*) echo "backup: RETENTION_DAYS must be a non-negative integer" >&2; exit 2 ;;
esac

log() { echo "[backup] $(date -u +%Y-%m-%dT%H:%M:%SZ) $*"; }

umask 077
mkdir -p "$BACKUP_DIR"
cd "$COMPOSE_DIR"

stamp="$(date -u +%Y%m%dT%H%M%SZ)"
final="$BACKUP_DIR/c3x_pricing-$stamp.dump"
tmp="$final.partial"
trap 'rm -f "$tmp"' EXIT

log "dumping service '$DB_SERVICE' to $final"
# -Fc: custom format (compressed, selective/parallel restore via pg_restore).
# -Z 6: explicit compression level. User and database come from the
# container's own POSTGRES_USER / POSTGRES_DB, over the local socket.
# scrape_seen is per-run scratch state (UNLOGGED); its rows are not worth
# keeping, only its definition.
docker compose exec -T "$DB_SERVICE" sh -c \
  'exec pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Fc -Z 6 --exclude-table-data=scrape_seen' \
  > "$tmp"

if [ ! -s "$tmp" ]; then
  log "ERROR: dump is empty" >&2
  exit 1
fi

# Read the whole archive back (to /dev/null): catches truncation and
# corruption anywhere, not just in the table of contents.
if ! docker compose exec -T "$DB_SERVICE" pg_restore -f /dev/null < "$tmp"; then
  log "ERROR: dump failed verification (pg_restore could not read it end to end)" >&2
  exit 1
fi

mv "$tmp" "$final"
trap - EXIT
log "ok: $(du -h "$final" | cut -f1) written"

# Retention: only after a verified backup exists.
pruned="$(find "$BACKUP_DIR" -maxdepth 1 -type f -name 'c3x_pricing-*.dump' -mtime "+$RETENTION_DAYS" -print -delete | wc -l | tr -d ' ')"
log "pruned $pruned archive(s) older than $RETENTION_DAYS day(s)"
