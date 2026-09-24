# Deployment examples

This directory contains opinionated, copy-pasteable deployment recipes. They are
**not** loaded by the binary and have no runtime coupling to the rest of the
codebase. Pick the one that matches your platform and adapt it.

| Subdirectory | Use case |
|---|---|
| [`compose/`](compose/)   | Local dev + small self-hosted deployments. Adds a `scraper` cron sidecar to `docker-compose.yml`. |
| [`k8s/`](k8s/)            | Kubernetes via plain manifests: `Deployment` for the API, `CronJob` per vendor for scrapes, `Service` + probes wired to `/readyz` / `/healthz`. |
| [`github-actions/`](github-actions/) | Run the scraper on a schedule against a hosted Postgres (e.g. Supabase, Neon, RDS). Zero infrastructure, free-tier friendly. |

## Design notes

- The `scrape` subcommand is **one-shot**: it exits 0 on success, non-zero on
  failure. All schedulers here rely on that contract.
- A per-vendor Postgres advisory lock (`pg_try_advisory_lock('scrape:<vendor>')`)
  makes overlapping runs safe. Two schedulers firing at once will cause one to
  skip with a warning, not corrupt data.
- Freshness is recorded in the `scrape_runs` table. To alert on stale data:
  ```sql
  SELECT vendor, MAX(finished_at) AS last_success
  FROM scrape_runs
  WHERE status = 'success'
  GROUP BY vendor;
  ```
- Recommended cadences (tradeoff: freshness vs. API politeness):
  - AWS: **weekly**, bulk price list churns slowly.
  - Azure: **daily**, retail prices API updates frequently.
  - GCP: **daily**, catalog has ~daily churn on new SKUs.

## Backups (docker compose)

[`backup.sh`](backup.sh) runs `pg_dump` inside the compose `db` service
(`docker compose exec`), writing a compressed custom-format archive to
`$BACKUP_DIR/c3x_pricing-<UTC timestamp>.dump`. It reads the archive back
end to end before keeping it, and only then deletes archives older than
`$RETENTION_DAYS` (default 7), so a failed run never prunes the last good
backup. Defaults: `COMPOSE_DIR` = the repo checkout the script lives in,
`BACKUP_DIR=/var/backups/c3x-pricing`.

Nightly at 02:30 UTC, before the 03:00 scraper window (root crontab, VPS
clock in UTC; `/opt/c3x-pricing-api` is the compose checkout):

```cron
30 2 * * * COMPOSE_DIR=/opt/c3x-pricing-api BACKUP_DIR=/var/backups/c3x-pricing RETENTION_DAYS=7 /opt/c3x-pricing-api/deploy/backup.sh >> /var/log/c3x-pricing-backup.log 2>&1
```

Size the backup disk for `RETENTION_DAYS` archives; check one with
`ls -lh /var/backups/c3x-pricing` after the first run. Copy archives off
the host (object storage, another machine) as well: a backup on the same
disk as the database does not survive losing that disk.

### Restore

[`restore.sh`](restore.sh) replaces the database contents with an archive.
It verifies the archive, stops `api` (and `scraper`, if running), restores
with `pg_restore --clean --if-exists --single-transaction` (all or nothing:
a failure leaves the current data untouched), then starts them again.

```bash
cd /opt/c3x-pricing-api
ls -t /var/backups/c3x-pricing/            # pick an archive
deploy/restore.sh /var/backups/c3x-pricing/c3x_pricing-20260101T023000Z.dump
# prompts for "restore"; pass --yes as the 2nd argument to skip the prompt
curl -fsS http://127.0.0.1:4000/readyz     # api back and connected
curl -fsS http://127.0.0.1:4000/status     # vendor product counts as expected
```

To rehearse without touching production, restore into a scratch Postgres
(`docker run -d --name pgtest -e POSTGRES_PASSWORD=x postgres:16-alpine`, then
`docker exec -i pgtest pg_restore -U postgres -d postgres --no-owner < archive.dump`).

## Monitoring

`.github/workflows/status-monitor.yml` polls `https://pricing.c3x.dev/status`
every 3 hours and fails when any vendor is `failed`, `stale`, `empty` or
`never`. On failure it opens one issue titled `Pricing API: vendor data
unhealthy` (label `status-monitor`), or comments on it if it is already
open, so a long outage does not spam; close the issue once fixed.
