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
docker compose exec scraper c3x-pricing-api scrape --vendor aws
```

### Memory

An AWS scrape parses EC2 price files of 0.5-1.5 GB each and then writes
about 2.4 million products, so it needs far more memory than the API. The
sidecar's defaults (`SCRAPER_MEM_LIMIT=8g`, `SCRAPE_CONCURRENCY=4`) assume a
host with at least 8 GB for it. On a smaller host, set them in `.env` below
the machine's RAM minus what Postgres uses, for example on a 4 GB VPS:

```bash
SCRAPER_MEM_LIMIT=3g
SCRAPER_GOMEMLIMIT=2500MiB
SCRAPE_CONCURRENCY=1
```

Do not run a scrape with `docker compose run api scrape ...`: that
container inherits the `api` service's 1 GB limit, and an AWS scrape is
OOM-killed (exit 137) when it starts writing products.

## Why a sidecar and not `docker compose run`?

- Single compose invocation brings the whole system up.
- Persistent container makes scrape timing visible in `docker compose logs scraper`.
- No host-level cron required.
