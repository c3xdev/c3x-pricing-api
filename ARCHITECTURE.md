# Architecture

One Go binary, three subcommands, one Postgres database, one GraphQL endpoint.

## High level

```
                ┌─────────────────────────┐
                │  Vendor pricing APIs    │
                │  AWS • Azure • GCP      │
                └────────────┬────────────┘
                             │  (bulk JSON / OData / REST)
                             ▼
              ┌──────────────────────────────┐
              │   c3x-pricing-api scrape     │   one-shot CLI
              │   errgroup per vendor        │   Postgres advisory lock
              └────────────┬─────────────────┘
                           │  UPSERT + DeleteStaleProducts
                           ▼
                  ┌──────────────────┐
                  │   PostgreSQL     │   products (JSONB + GIN)
                  │                  │   scrape_runs, schema_version
                  └────────┬─────────┘
                           │
                           ▼
              ┌──────────────────────────────┐
              │   c3x-pricing-api serve      │   GraphQL @ :4000
              │   rate limit → auth → CORS   │
              │   AST-validated queries       │
              └──────────────────────────────┘
                           │
                           ▼
                    GraphQL clients
                    (C3X CLI, UIs, scripts)
```

## Layout

```
cmd/server/main.go         # Cobra CLI: serve / scrape / seed
internal/
  config/   config.go      # env-driven Config, Validate() (incl. prod sslmode guard)
  db/       db.go          # pgxpool, versioned migrations, advisory lock, scrape_runs
            queries.go     # QueryProducts, UpsertProducts, DeleteStaleProducts, regex cache
            seed.go        # streaming JSON seeder
            migrations*.sql
  graphql/  schema.go      # single `products(filter, limit, offset)` query
  scraper/  aws.go         # bulk JSON, fan-out via errgroup
            azure.go       # retail prices OData, per-service errgroup
            gcp.go         # Cloud Billing Catalog, per-service errgroup
            scraper.go     # common interface + ProductHash/PriceHash helpers
            hash.go / aws_regions.go
  server/   server.go      # HTTP server, middleware chain, handlers
            server_test.go
deploy/                    # compose / k8s / github-actions recipes (not wired into code)
```

## Request path (`serve`)

Middleware chain applied top-down in [internal/server/server.go](internal/server/server.go):

1. **requestID**: honors `X-Request-ID` if it matches `^[A-Za-z0-9_-]{1,64}$`, else generates via `crypto/rand`.
2. **recoverMiddleware**: catches panics, returns typed 500 JSON, uses a `responseTracker` so it doesn't try to write headers twice.
3. **securityHeaders**: X-Content-Type-Options, X-Frame-Options, CSP, Referrer-Policy.
4. **CORS**: allow-list from `CORS_ALLOWED_ORIGINS`, always emits `Vary: Origin`.
5. **auth**: SHA-256 + `subtle.ConstantTimeCompare` on `API_KEY`. Skipped when `API_KEY` is empty (dev mode).
6. **rateLimit**: per-IP token bucket (`golang.org/x/time/rate`) in an LRU bounded at 10k. `X-Forwarded-For` is honored only when the peer is in `TRUSTED_PROXIES` (CIDR list).
7. **handleGraphQL**: parses single or batched POST, enforces body/size/depth/introspection caps, executes each query with a shared timeout, fills remaining batch slots with a typed timeout error on ctx cancellation.

GraphQL validation uses the graphql-go AST:

- **Depth** is computed by walking `SelectionSet` through inline + named fragments with a visited-set cycle guard.
- **Introspection block** matches `__schema` / `__type` / `__Type` at the field level, not by substring.

## Resolver path

The single root field is `products(filter, limit, offset)`. The resolver lives
in [internal/graphql/schema.go](internal/graphql/schema.go) and calls into
[internal/db/queries.go](internal/db/queries.go):

1. `p.Context` (the per-request `context.WithTimeout`) flows directly to `QueryProducts`. It also carries the per-request budget: every `products` field must set `vendorName` and `service`, and one HTTP request may run at most `MAX_PRODUCT_QUERIES_PER_REQUEST` products queries returning at most `MAX_PRODUCTS_PER_REQUEST` products, across aliases and batch items.
2. `QueryProducts` builds a dynamic `WHERE` over the JSONB attributes with `$N`-bound args only. No string interpolation, no SQL injection surface. Attribute equality is JSONB containment (`attributes @> '{"k":"v"}'`) so the GIN index on `attributes` serves it; regex filters use `attributes->>'k' ~ ...`.
3. Regex filters compile into a 1024-entry LRU cache, bounded at 200 chars.
4. Every pooled connection has `SET statement_timeout = '300000'` set via `pgxpool.AfterConnect`, so even a rogue regex cannot pin Postgres.

## Scrape path

Each vendor implements `Scraper`:

```go
type Scraper interface {
    Name() string
    Scrape(ctx context.Context) ([]db.Product, error)
}
```

`runOneScrape` in [cmd/server/main.go](cmd/server/main.go):

1. Acquires `pg_try_advisory_lock(hashtext('scrape:<vendor>'))` on a dedicated pool connection. If another run holds it, this process skips with a warning.
2. Inserts a `scrape_runs` row with `status='running'` and the DB's `now()` timestamp.
3. Calls `Scraper.Scrape(ctx)`. Each scraper fans out across services with `errgroup.WithContext` + `SetLimit(cfg.ScrapeConcurrency)`. Per-service errors are logged but do not abort siblings; ctx cancellation (SIGTERM) does abort.
4. `UpsertProducts` writes in 1000-row batches (8-column batch size stays well under Postgres's 65,535 parameter cap). The `ON CONFLICT ... DO UPDATE` only fires when `prices`, `attributes` or `sku` actually differ, so an unchanged product is not rewritten and `updated_at` means "content last changed". Each batch is also recorded in the narrow, UNLOGGED `scrape_seen (run_id, product_hash)` table.
5. `DeleteStaleProducts` removes the vendor's rows with `updated_at < scrapeStart` (the DB's own `now()`, not wall-clock) that are not in this run's `scrape_seen` set. It is skipped when any service/region failed, when the product count dropped by half, or when the seen-set is under half the ingested count (a Postgres crash truncates UNLOGGED tables). The run's `scrape_seen` rows are dropped when it ends.
6. Updates `scrape_runs` with `status`, `products`, `deleted`, optional `error`.

## Freshness

The `scrape_runs` table is the source of truth for "is our data stale?":

```sql
SELECT vendor, MAX(finished_at) AS last_success
FROM scrape_runs
WHERE status = 'success'
GROUP BY vendor;
```

Expose it via `/readyz` or a future `scrapeStatus` GraphQL field. This is the
single most useful signal for operators of a pricing service.

## What is intentionally not here

- **No embedded scheduler.** `scrape` is one-shot. Scheduling is delegated to
  cron / K8s CronJob / GitHub Actions ([deploy/](deploy/)). This keeps the
  binary simple, leader election someone else's problem, and lets users pick
  cadence per vendor.
- **No OpenTelemetry / Prometheus / gzip** in v1.0. Tracked as deferred
  observability items; see [AUDIT.md](AUDIT.md).
- **No distributed mode.** If a single Postgres can't hold the data, you have
  earned the right to a v2 conversation.
