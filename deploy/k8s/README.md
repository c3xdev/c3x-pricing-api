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

The manifests reference `ghcr.io/c3xdev/c3x-pricing-api:IMAGE_TAG` as a
placeholder. Substitute it for a concrete release version (recommended) or for
`latest` (not recommended for production — no rollback safety, surprise
upgrades on pod restart).

```bash
# 1. Pick a version. Browse https://github.com/c3xdev/c3x-pricing-api/releases
#    or use the latest tag programmatically:
VERSION=$(curl -fsSL https://api.github.com/repos/c3xdev/c3x-pricing-api/releases/latest | grep -oE '"tag_name": *"[^"]+' | cut -d'"' -f4)
echo "Deploying $VERSION"

# 2. Apply with substitution
kubectl apply -f namespace.yaml
kubectl apply -f secret.example.yaml  # edit first!
for f in deployment.yaml cronjob-aws.yaml cronjob-azure.yaml cronjob-gcp.yaml; do
  sed "s|:IMAGE_TAG|:$VERSION|g" "$f" | kubectl apply -f -
done
```

To upgrade, repeat with the new `$VERSION`; `kubectl rollout undo` then
correctly rolls back to the previously-deployed image.

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
