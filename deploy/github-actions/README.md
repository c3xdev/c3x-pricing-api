# GitHub Actions scraper

Run `c3x-pricing-api scrape` on a schedule against a hosted Postgres (Neon,
Supabase, RDS, CrunchyBridge, etc.) using only free GitHub Actions minutes.

## Setup

1. Provision a managed Postgres and get its `DATABASE_URL`. Use `sslmode=require`.
2. Add these repository secrets under **Settings → Secrets and variables → Actions**:
   - `C3X_DATABASE_URL`: the Postgres connection string.
   - `C3X_GCP_API_KEY`: optional, only needed for GCP scrapes.
3. Copy [`scrape.yml`](scrape.yml) into your fork at `.github/workflows/scrape.yml`.
4. Commit and push. The first run will trigger on the next UTC hour boundary,
   or you can dispatch it manually from the Actions tab.

The workflow runs AWS weekly, Azure/GCP daily, staggered across the hour to
keep the upstream APIs happy. Each vendor runs in its own job, so one vendor's
outage does not stop the others.

## Why this is useful

- **Zero infra**: no servers, no cron daemons. The cheapest hosted pricing
  dataset possible.
- **Free for public repos**; for private repos, each run is ~5 minutes of
  billable Actions time per vendor.
- **Public snapshot pattern**: if you add a `pg_dump` + upload step after each
  run, you can publish a nightly `products.jsonl.zst` to Releases and let
  downstream users skip the scraper entirely.
