# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
