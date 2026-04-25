# docker-compose deployment

This directory contains a standalone compose file that adds a **scraper cron
sidecar** on top of the base `docker-compose.yml` in the repo root.

## Usage

From the repo root:

```bash
docker compose -f docker-compose.yml -f deploy/compose/docker-compose.scraper.yml up -d
```

This starts:

- `db`: Postgres 16 (from the base file)
- `api`: the GraphQL server on :4000 (from the base file)
- `scraper`: runs `c3x-pricing-api scrape --vendor all` **once a day at 03:00 UTC**
  via a tiny cron loop. The container sleeps otherwise.

To trigger a scrape manually:

```bash
docker compose exec scraper /app/c3x-pricing-api scrape --vendor aws
```

## Why a sidecar and not `docker compose run`?

- Single compose invocation brings the whole system up.
- Persistent container makes scrape timing visible in `docker compose logs scraper`.
- No host-level cron required.
