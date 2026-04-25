# Kubernetes deployment

Minimal, opinionated manifests for running `c3x-pricing-api` on Kubernetes.

## Contents

| File | Purpose |
|---|---|
| [`namespace.yaml`](namespace.yaml)   | `c3x-pricing` namespace. |
| [`secret.example.yaml`](secret.example.yaml) | Template for `DATABASE_URL`, `API_KEY`, `GCP_API_KEY`. **Do not commit a filled copy.** |
| [`deployment.yaml`](deployment.yaml) | API server `Deployment` + `Service` with `/healthz` liveness and `/readyz` readiness probes. |
| [`cronjob-aws.yaml`](cronjob-aws.yaml)     | Weekly AWS scrape (Sunday 03:00 UTC). |
| [`cronjob-azure.yaml`](cronjob-azure.yaml) | Daily Azure scrape (03:15 UTC). |
| [`cronjob-gcp.yaml`](cronjob-gcp.yaml)     | Daily GCP scrape (03:30 UTC). |

## Quick start

```bash
kubectl apply -f namespace.yaml
kubectl apply -f secret.example.yaml  # edit first!
kubectl apply -f deployment.yaml
kubectl apply -f cronjob-aws.yaml -f cronjob-azure.yaml -f cronjob-gcp.yaml
```

## Design notes

- **No leader election needed.** Scrapes use a Postgres advisory lock. Two
  concurrent runs of the same vendor will just have one skip.
- **Separate CronJobs per vendor** so a slow AWS run does not delay the Azure
  schedule, and so `kubectl get cronjob` shows you exactly which vendor is stale.
- `concurrencyPolicy: Forbid` means a still-running scrape will not be
  interrupted by the next tick. The advisory lock is a belt-and-braces safety.
- Probes hit `/healthz` (liveness, no DB) and `/readyz` (readiness, DB ping) so
  a transient DB blip does not restart your pod.
- You are expected to bring your own Postgres (managed is recommended). Adjust
  the `Secret` accordingly.
