# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Per-vendor scrape concurrency overrides via `SCRAPE_CONCURRENCY_{AWS,AZURE,GCP}`.
- `scrape_runs` retention: old rows pruned after 30 days by default
  (`SCRAPE_RUNS_RETENTION_DAYS`; `0` disables).
- Testcontainers-backed DB integration tests (`-tags=integration`).
- SIGTERM mid-scrape regression test (advisory lock released, run marked `failed`).
- Release workflow (`.github/workflows/release.yml`) producing multi-arch
  container images, SBOM attestation (Syft), and cosign keyless signatures.

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

[Unreleased]: https://github.com/c3xdev/c3x-pricing-api/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/c3xdev/c3x-pricing-api/releases/tag/v0.1.0
