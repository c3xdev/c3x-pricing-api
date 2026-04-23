# C3X Pricing API

Open-source cloud pricing API for [C3X](https://c3x.dev). Scrapes pricing data directly from AWS, Azure, and Google Cloud public APIs and serves it via a GraphQL endpoint.

**At a glance:** one Go binary, three subcommands (`serve`, `scrape`, `seed`),
PostgreSQL-backed, versioned schema, production-grade middleware (rate limiting,
auth, CORS, panic recovery, AST-based GraphQL validation). See
[ARCHITECTURE.md](ARCHITECTURE.md) for the ten-minute tour, and
[docs/provider-quirks.md](docs/provider-quirks.md) for the hard-won tribal
knowledge about each upstream pricing API.

## Quick Start

```bash
# Clone and configure
git clone https://github.com/c3xdev/c3x-pricing-api.git
cd c3x-pricing-api
cp .env.example .env
# Edit .env, set POSTGRES_PASSWORD

# Start with Docker Compose
docker compose up -d

# Scrape pricing data
docker compose exec api ./c3x-pricing-api scrape --vendor aws
docker compose exec api ./c3x-pricing-api scrape --vendor azure
docker compose exec api ./c3x-pricing-api scrape --vendor gcp

# ...or scrape all three:
docker compose exec api ./c3x-pricing-api scrape --vendor all
```

The API will be available at `http://localhost:4000/graphql`.

## Usage with C3X CLI

```bash
export C3X_SELF_HOSTED=true
export C3X_PRICING_API_ENDPOINT=http://localhost:4000
c3x estimate --path /path/to/terraform
```

## API Endpoints

| Endpoint | Method | Description |
|---|---|---|
| `/graphql` | POST | GraphQL pricing queries (supports batched requests) |
| `/status`  | GET | Scrape status and product counts per vendor (JSON) |
| `/healthz` | GET | Liveness probe, no DB dependency |
| `/readyz` | GET | Readiness probe, pings the DB |
| `/health`  | GET | Backwards-compatible alias for `/readyz` |

## Deploying

See [deploy/](deploy/) for opinionated recipes:

- [`deploy/compose/`](deploy/compose/): local dev with a cron sidecar for scheduled scrapes.
- [`deploy/k8s/`](deploy/k8s/): plain-manifest Kubernetes deployment with per-vendor CronJobs.
- [`deploy/github-actions/`](deploy/github-actions/): run the scraper on GitHub's free schedule against a hosted Postgres.

Scrapes are one-shot CLI invocations, guarded by a Postgres advisory lock so
overlapping runs are safe. Freshness is tracked per vendor in the `scrape_runs`
table.

## Configuration

| Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | *required* | PostgreSQL connection string |
| `PORT` | `4000` | HTTP server port |
| `API_KEY` | *(empty)* | API key for authentication. Empty = no auth. |
| `GCP_API_KEY` | *(empty)* | Google Cloud API key for scraping GCP prices |
| `SCRAPE_CONCURRENCY` | `4` | Global concurrent scrape workers |
| `SCRAPE_CONCURRENCY_AWS` | `0` | AWS-specific override (0 = inherit global) |
| `SCRAPE_CONCURRENCY_AZURE` | `8` | Azure-specific override |
| `SCRAPE_CONCURRENCY_GCP` | `0` | GCP-specific override (0 = inherit global) |
| `MAX_REQUEST_BODY_MB` | `4` | Maximum request body size in MB |
| `MAX_BATCH_SIZE` | `100` | Maximum number of queries per batch request |
| `QUERY_TIMEOUT_SECS` | `30` | Query execution timeout in seconds |
| `MAX_QUERY_DEPTH` | `10` | Maximum GraphQL query nesting depth |
| `RATE_LIMIT_PER_SEC` | `100` | Maximum requests per second per IP |
| `CORS_ALLOWED_ORIGINS` | *(empty)* | Comma-separated allowed CORS origins. `*` = all |
| `TRUSTED_PROXIES` | *(empty)* | Comma-separated CIDRs/IPs for X-Forwarded-For trust |
| `DISABLE_INTROSPECTION` | `false` | Block GraphQL introspection queries |
| `DB_MAX_CONNS` | `0` | Max database pool connections (0 = pgx default) |
| `DB_MIN_CONNS` | `0` | Min database pool connections (0 = pgx default) |
| `METRICS_PORT` | *(empty)* | Separate port for /metrics. Empty = main port |
| `CNY_USD_RATE` | `6.2069` | CNY/USD exchange rate for AWS China pricing |

## Scraping

```bash
# Scrape all vendors
./c3x-pricing-api scrape --vendor aws
./c3x-pricing-api scrape --vendor azure
./c3x-pricing-api scrape --vendor gcp
./c3x-pricing-api scrape --vendor all

# Seed from JSON files (for testing)
./c3x-pricing-api seed --file data/products.json
```

AWS and Azure pricing data is publicly available without authentication. GCP requires a free API key. Get one from [Google Cloud Console](https://console.cloud.google.com/apis/credentials).

## Security

- Rate limiting per IP address
- Request body size limits
- Query execution timeouts
- Timing-safe API key comparison
- Graceful shutdown on SIGTERM
- Database health checks
- SHA-256 hashing (no MD5)
- No hardcoded credentials

## License

[Apache License 2.0](https://choosealicense.com/licenses/apache-2.0/)
