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
