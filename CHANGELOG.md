# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/c3xdev/c3x-pricing-api/compare/v1.0.2...HEAD
[1.0.2]: https://github.com/c3xdev/c3x-pricing-api/compare/v0.1.0...v1.0.2
[0.1.0]: https://github.com/c3xdev/c3x-pricing-api/releases/tag/v0.1.0
