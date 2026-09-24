# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Per-request query cost limits on `/graphql`. A `products` query must set
  both `vendorName` and `service`; one HTTP request may run at most
  `MAX_PRODUCT_QUERIES_PER_REQUEST` (50) products queries returning at most
  `MAX_PRODUCTS_PER_REQUEST` (1000) products, summed over aliases and batch
  items (a field's `limit` is clamped to what is left). Previously a single
  request under the per-IP rate limit could fan out into hundreds of
  full-JSONB scans. The c3x CLI sends one query per request with both
  filters and `limit:50`, so it is unaffected.
- Global in-flight limit for `/graphql` (`MAX_INFLIGHT_REQUESTS`, default
  DB pool size minus 2). A request that finds no slot within
  `INFLIGHT_WAIT_MS` (250) gets `503` with `Retry-After: 1` instead of
  queueing on the DB pool until its timeout.
- `.github/workflows/status-monitor.yml`: polls `/status` every 3 hours,
  fails when a vendor is `failed`, `stale`, `empty` or `never`, and opens
  (or comments on, only when the unhealthy set changes) a single
  `status-monitor` issue.
- `deploy/backup.sh` / `deploy/restore.sh`: compressed, verified,
  timestamped `pg_dump` archives with N-day retention via
  `docker compose exec`, and an all-or-nothing restore. Cron line and
  restore procedure in `deploy/README.md`.
- Memory limits for the `db` (2g), `api` (1g) and `scraper` (8g) compose
  services, overridable via `DB_MEM_LIMIT` / `API_MEM_LIMIT` /
  `SCRAPER_MEM_LIMIT`, with a matching `GOMEMLIMIT` for the Go processes.

### Changed
- `/metrics` is no longer served on the public port. It is only on its own
  listener, `METRICS_ADDR` (default `127.0.0.1:9090`, `off` disables).
  The legacy `METRICS_PORT=<p>` still works and means `:<p>`.
- GraphQL introspection defaults to disabled when `ENV=production`, and
  `docker-compose.yml` sets `DISABLE_INTROSPECTION=true`. `__typename`
  (used by `c3x doctor`) is still allowed.
- `MAX_BATCH_SIZE` default lowered from 100 (500 in `docker-compose.yml`)
  to 50. Deployments that set `MAX_BATCH_SIZE` in `.env` keep their value.
- Attribute equality filters use JSONB containment
  (`attributes @> '{"k":"v"}'`) instead of `attributes->>'k' = 'v'`, so the
  existing GIN index on `attributes` serves them instead of a filter over
  every row of the vendor/service. Results are identical: every stored
  attribute value is a JSON string. Regex filters are unchanged.
- Scrape upserts skip products whose `prices`, `attributes` and `sku` are
  unchanged, so a daily scrape no longer rewrites ~2.9M rows (heap, TOAST
  and GIN churn). `updated_at` now means "content last changed". Stale
  cleanup no longer relies on every row being touched: each run records
  the products it saw in a new UNLOGGED `scrape_seen` table and deletes
  only the vendor's products it did not see. Cleanup is skipped if that
  set is lost or incomplete.

### Fixed
- Azure: rows Azure flags `isPrimaryMeterRegion=false` are ingested when
  they are the only row for their price. Azure lists a meter under every
  region it is sold in but makes one region (often "Global") its primary;
  the other regions carry a copy of the same meter ID at the same price.
  Dropping those copies left most regions with no row for meters such as
  AKS "Standard Uptime SLA", Azure Firewall "Standard Data Processed",
  "General Block Blob v2" Hot/Cool LRS and GRS, Windows App Service Basic
  and Standard plans and API Management units. A non-primary row now
  fills a price slot (product, sku, meter, region, purchase option, unit,
  tier, term) only when no primary row exists for it, so a slot never
  gets a second, different price, and every price stored before is
  unchanged. On a sample of every scraped service in eastus and
  westeurope (40,186 rows, 6,428 non-primary) products grow from 22,245
  to 25,079; under "Global", from 2,706 to 2,943. The services that were
  already exempt from the filter are unchanged.
- Catalog: `aws_cloudwatch_log_group`, `aws_dms_replication_instance`,
  `aws_kinesis_stream`, `aws_kms_key`, `aws_mq_broker`,
  `aws_opensearch_domain` and `azurerm_firewall` each had a line whose
  lookup matched no product (a filter on a value or attribute the data
  does not carry, or a product family that does not exist), so it was
  $0 in every region. They now match the right product, and the fixtures
  carry the vendor-published totals. `azurerm_firewall` is now priced
  live from the Global meters instead of an inline deployment rate, and
  `azurerm_service_plan` filters on the OS-specific product name so a
  Linux plan is not priced at the Windows rate once Windows rows are
  ingested.
- AWS products in eu-south-2, eu-central-2, mx-central-1, ap-east-2 and
  ap-southeast-6 were stored under their display name ("Europe (Spain)")
  instead of their region code, because AWS renamed or added the
  locations and the name map didn't know them, so no lookup for those
  regions could match. The scraper now takes the offer file's own
  `regionCode` for products in a region proper (Local Zones, Wavelength
  and Outposts keep their location name, so they are not folded into
  their parent region), with the name map as the fallback. The rows
  stored under the old names are removed by the stale-row cleanup on the
  next full scrape.
- Catalog (`catalog/`) pricing corrections, served to every client from
  `/catalog`. Each was checked against the vendor's published price and
  the live pricing API, and its fixture now holds the vendor's number:
  - `aws_msk_cluster` priced every cluster as kafka.m5.large and its
    broker storage at $0. It now prices `broker_node_group_info`
    `instance_type` and the per-broker EBS volume: 3 x kafka.m5.4xlarge
    with 1,000 GB each goes from $459.90 to $3,979.20/mo.
  - `azurerm_mssql_database` keyed on a `vcores` attribute the resource
    does not have, so every database was 2-vCore General Purpose. It now
    parses `sku_name` (GP/BC/HS, hardware family, vCores; serverless
    `GP_S_`/`HS_S_`; DTU `Basic`, `S*`, `P*`) and adds the SQL license
    unless `license_type = "BasePrice"`: `BC_Gen5_8` goes from $225.92 to
    $3,975.90/mo.
  - `azurerm_kubernetes_cluster` priced the Standard tier at the
    Premium/LTS rate ($0.60/hr, $438/mo); Standard is $0.10/hr ($73/mo).
    The Premium tier, previously unpriced, is now $438/mo.
  - `aws_rds_cluster_instance` with `instance_class = "db.serverless"`
    (Aurora Serverless v2) was $0. It now prices ACU-hours at the
    cluster's `serverlessv2_scaling_configuration.min_capacity` (0.5 ACU
    if not visible), or `monthly_acu_hours` from a usage file.
  - Cosmos DB throughput was never priced: the databases and containers
    that carry it were listed as free. `azurerm_cosmosdb_sql_database`,
    `_sql_container`, `_mongo_database`, `_mongo_collection`,
    `_cassandra_keyspace`, `_cassandra_table`, `_gremlin_database`,
    `_gremlin_graph` and `azurerm_cosmosdb_table` now price `throughput`
    and `autoscale_settings.max_throughput` (at max unless usage is given)
    per account region, at the multi-region-write rate when the account
    has it. `azurerm_cosmosdb_account` prices serverless request units and
    per-region storage from usage, and always reports its usage lines.
  - `google_cloud_run_v2_service` was skipped unless usage was supplied.
    It now always reports its vCPU, memory and request lines and prices
    minimum instances from `template.scaling.min_instance_count` and the
    container's `resources.limits`.
  - `azurerm_storage_account` priced every account as Hot LRS. It now
    follows `account_replication_type` (LRS, ZRS, GRS, RAGRS, GZRS,
    RAGZRS), `access_tier`, `account_tier = "Premium"` and
    `is_hns_enabled`: 100 GB Hot GRS goes from $2.10 to $4.58/mo.
  - `azurerm_linux_virtual_machine` / `azurerm_windows_virtual_machine`
    OS disks were only priced when `os_disk.disk_size_gb` was set. They are
    now priced by `storage_account_type` and size (30 GiB Linux / 127 GiB
    Windows image size when unset), across the full P/E/S tier ladder and
    ZRS. `azurerm_managed_disk`, which was always priced as E10, uses the
    same ladder.

  Every expression uses only functions and syntax that released CLIs
  already evaluate. The Aurora Serverless v2 scaling range and the Cosmos
  DB region count / multi-region-write flag are declared on the parent
  resource and copied onto the child by the CLI parser from the next CLI
  release; older clients price the same lines at the documented defaults
  (0.5 ACU; one region at the single-write rate).

- The compose scraper overlay called `/app/c3x-pricing-api`, but the image
  installs the binary at `/usr/local/bin`, and computed its next run with
  GNU/BSD `date` flags that the Alpine image's BusyBox `date` rejects, so
  the sidecar never scraped. It now calls `c3x-pricing-api` from `PATH`,
  schedules with shell arithmetic, and exits non-zero on a failed scrape
  instead of swallowing it with `|| echo`.
- A failed EC2 region (fetch, parse or upsert) now counts as a failed
  service, so stale cleanup is skipped instead of deleting that region's
  EC2 products.

## [1.1.4] - 2026-09-22

### Fixed
- Aurora clusters were priced at the I/O-Optimized rate whether or not
  they used it. The two instance SKUs per class share `instanceType`,
  `databaseEngine` and `deploymentOption`, which was all the mapping
  filtered on, so both matched and the max-non-zero picker took the
  dearer one: a `db.r6g.large` priced at $246.74/mo instead of $189.80,
  a ~30% overcharge on Aurora Standard. Instances now discriminate on
  the `storage` attribute and cluster storage follows `storage_type`.
  (c3xdev/c3x#68)
- Aurora I/O was priced at zero everywhere except us-east-1: the
  `usagetype` pin `Aurora:StorageIOUsage` never matches the
  region-prefixed `EU-Aurora:StorageIOUsage` upstream. Pin removed.
- Aurora I/O is no longer billed on `aurora-iopt1` clusters, where it is
  included in the storage rate.
- `/status` no longer reports a vendor `ready` when it has no data. It
  derived readiness from the newest successful run and never downgraded
  it, so a vendor whose credential is revoked kept answering `ready`
  with `products: 0` indefinitely, the same silent failure the scraper
  was fixed to stop in 1.1.3. Two states are new: `failed` (the most
  recent run failed, served data is frozen) and `empty` (the last
  successful run ingested nothing). The status vocabulary is now
  documented in the README. Serving is unaffected. (#56)

### Changed
- Go 1.27.1 across CI, release and the Docker image. Go 1.25 went end of
  life when 1.27 shipped, so the pinned toolchain no longer received
  security fixes. This also unblocked `golang.org/x/time` 0.16.0, which
  requires go 1.26. (#54)
- Dependency bumps: OpenTelemetry 1.46 / contrib 0.70, pgx 5.11,
  otelpgx 0.12, prometheus/client_golang 1.24.1. (#53)

## [1.1.3] - 2026-09-21

### Fixed
- A scrape that ingested zero products is recorded as `failed` instead
  of `success`, does not advance the last-success freshness gauge, and
  makes `scrape` exit non-zero so cron and CI surface it. Per-service
  errors are swallowed by design, so a wholly invalid credential
  previously surfaced only as "every service failed, zero products" and
  was still written to `scrape_runs` as a green row; an invalid GCP key
  went unnoticed for over a month that way. The existing zero-product
  guard sat after the failed-services case in the cleanup switch and so
  never ran in exactly that scenario. Partial data is still a success,
  and an empty run still skips stale-product cleanup so the previous
  catalog is preserved. An empty vendor no longer cancels its siblings'
  in-flight scrapes. (#52)

## [1.1.2] - 2026-09-16

### Fixed
- Bound `DB_MAX_CONNS` / `DB_MIN_CONNS` with `<= math.MaxInt32` before
  narrowing to pgxpool's `int32`, so an absurd configured pool size
  cannot overflow into a negative value. (#46)

### Security
- Least-privilege `permissions: contents: read` on the CI workflow. (#46)
- Enabled CodeQL default-setup scanning on the repository.

## [1.1.1] - 2026-09-16

### Fixed
- The `db` service has `restart: unless-stopped`, matching `api`. A
  Docker daemon restart previously left Postgres stopped while the API
  crash-looped on `lookup db: no such host`, taking the endpoint down
  until it was started by hand. (#44)

### Changed
- Dependency bumps: testcontainers 0.44.0, OpenTelemetry 1.45 / contrib
  0.70, prometheus/client_golang 1.24.1. (#45)

## [1.1.0] - 2026-09-16

### Added
- `TRUSTED_PROXIES` accepts the literal `cloudflare`, expanding to
  Cloudflare's published edge ranges. (#40)
- `/` answers 200 with `X-Robots-Tag: noindex` instead of 404, so the
  public endpoint stops reporting a broken root to search engines. (#39)

### Security
- Rate limiting and metrics key on the real client IP: `clientIP` prefers
  `CF-Connecting-IP`, then the first `X-Forwarded-For` hop, and only when
  the immediate peer is a trusted proxy. Behind Cloudflare every request
  previously counted against the shared edge IP, so per-client limits
  were not being enforced. (#40, #41)
- Patched 9 known vulnerabilities: Go 1.25.13 (6 stdlib CVEs), grpc
  v1.83.1, x/text v0.39.0. The release workflow builds with the same
  pinned Go as CI and the Dockerfile. (#43)
- Bind the API to `127.0.0.1` rather than `0.0.0.0`. (#32)

### Fixed
- Access logs record the real client IP instead of the local proxy
  hop. (#42)
- Cloud SQL `db-g1-small` tier is priced. (#31)
- Corrected DNS query, ACI and Digital Twins over-pricing; completed the
  priced long tail at 1,340 recognized kinds.

### Changed
- `price_snapshots` recording is gated behind `ENABLE_PRICE_SNAPSHOTS`
  and off by default: the table had no reader and grew unbounded.

## [1.0.4] - 2026-06-09

### Added
- AWS scraper covers 9 additional services: DAX, MemoryDB, EMR,
  App Runner, AppSync, Amplify, Cognito, X-Ray, Athena. (Storage
  Gateway excluded — AWS publishes no bulk-pricing offer file.)
- Per-service AWS attribute normalisation: SageMaker `instanceType`
  role-suffix strip, EKS `mode` discriminator (Auto/Classic),
  Athena `productFamily` backfill.
- GCP scraper resolves 11 additional services by display name at
  scrape time (Memorystore, BigQuery, Filestore, Bigtable, Secret
  Manager, KMS, Artifact Registry, Logging, Spanner, Dataflow,
  Dataproc). Unresolved names log a warning; the scrape continues.
- Prometheus metrics: `c3x_products_total{vendor}` gauge (fed from
  `/status`) and `c3x_graphql_queries_total{result}` counter with
  ok/empty/error outcomes — `empty` flags CLI filter shapes the
  database doesn't carry.

### Fixed
- Azure: Cosmos DB (incl. autoscale/serverless variants), Container
  Registry, Logic Apps, Key Vault, and Event Hubs Standard-tier rows
  were dropped by the `isPrimaryMeterRegion` filter and invisible to
  the CLI; they now ingest.

## [1.0.3] - 2026-06-02

### Security
- Bumped Go toolchain to **go1.25.11** in CI, release workflow, and Dockerfile.
  Picks up stdlib fixes for [GO-2026-5039](https://pkg.go.dev/vuln/GO-2026-5039)
  (`net/textproto`) and [GO-2026-5037](https://pkg.go.dev/vuln/GO-2026-5037)
  (`crypto/x509`). The `v1.0.2` images shipped with `go1.25.10` and remain
  affected — upgrade to `v1.0.3`. (#22)

### Changed
- Release workflow uses `actions/attest` instead of the deprecated
  `actions/attest-sbom`; inputs are identical. (#21)

## [1.0.2] - 2026-06-02

### Added
- Per-vendor scrape concurrency overrides via `SCRAPE_CONCURRENCY_{AWS,AZURE,GCP}`.
- `scrape_runs` retention: old rows pruned after 30 days by default
  (`SCRAPE_RUNS_RETENTION_DAYS`; `0` disables).
- Testcontainers-backed DB integration tests (`-tags=integration`).
- SIGTERM mid-scrape regression test (advisory lock released, run marked `failed`).
- Release workflow (`.github/workflows/release.yml`) producing multi-arch
  container images, SBOM attestation (Syft), and cosign keyless signatures.
- `:latest` Docker tag is now published by the release workflow (#13).

### Changed
- Dockerfile now uses a numeric UID (`USER 1000`) so K8s admission with
  `runAsNonRoot: true` can verify the image's non-root identity (#13).
- K8s manifests reference `:IMAGE_TAG` placeholder instead of `:latest`; the
  deploy README documents a one-liner `sed` substitution for pinning to a
  concrete release version, which preserves `kubectl rollout undo` semantics (#20).
- Cronjob memory `requests`/`limits` tuned per measured vendor product counts
  (AWS / Azure: request 2Gi, limit 14Gi; GCP: request 1Gi, limit 4Gi) so the
  scheduler does not permanently reserve burst capacity for once-a-day jobs (#19).

### Fixed
- AWS scrape no longer OOM-kills on clusters whose scheduler enforces request-based
  memory; limit raised to a level that accommodates ~410k products in batch (#13).

## [0.1.0] - 2024-11-01

### Added
- Initial release of the C3X Pricing API.
- Single-binary Go service with `serve`, `scrape`, and `seed` subcommands.
- AWS, Azure, and GCP pricing scrapers (errgroup-parallelized).
- GraphQL API with AST-validated depth limit, introspection toggle, and
  redacted access logs.
- Postgres JSONB storage with versioned migrations and `scrape_runs` tracking.
- Prometheus `/metrics` endpoint and OpenTelemetry HTTP + pgx tracing
  (no-op when `OTEL_EXPORTER_OTLP_ENDPOINT` is unset).
- Gzip response compression with a 1 KB threshold.
- Deploy recipes for Docker Compose, Kubernetes, and GitHub Actions.

### Security
- Advisory-lock-guarded scrapes prevent concurrent writes per vendor.
- `sslmode=disable/allow/prefer` rejected in production via strict URL parsing.
- Security headers, CORS allow-list, request-body cap, and per-IP rate limiting.

[Unreleased]: https://github.com/c3xdev/c3x-pricing-api/compare/v1.0.3...HEAD
[1.0.3]: https://github.com/c3xdev/c3x-pricing-api/compare/v1.0.2...v1.0.3
[1.0.2]: https://github.com/c3xdev/c3x-pricing-api/compare/v0.1.0...v1.0.2
[0.1.0]: https://github.com/c3xdev/c3x-pricing-api/releases/tag/v0.1.0
