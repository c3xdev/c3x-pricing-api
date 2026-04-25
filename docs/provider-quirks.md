# Provider quirks

Tribal knowledge about each upstream pricing API that's not in their docs. Grow
this file whenever you hit a surprise. The moat of any OSS pricing aggregator
is the accumulated quirks list. The code is the easy part.

> Conventions: lines prefixed with **⚠** mean "known footgun, preserved in code
> behavior for a reason". Lines prefixed with **🔧** mean "what we do about it".

---

## AWS

**Source:** AWS Price List Bulk API (public, no auth).
Endpoint: `https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/index.json`
then per-service `<service>/current/<region>/index.json`.

- ⚠ **Size.** The top-level index plus per-service files can sum to **>2 GB**.
  🔧 We cap each body with `io.LimitReader` at 2 GB; increase `C3X_AWS_MAX_BODY_MB`
  only if AWS expands (unlikely).
- ⚠ **China pricing is in CNY**, not USD. AWS does not publish a conversion rate.
  🔧 We apply `CNY_USD_RATE` (default 6.2069) to CN regions at scrape time.
  Override per deploy; revisit yearly.
- ⚠ **Rate limiting is undocumented.** The CloudFront-fronted bulk API
  occasionally returns 403 under concurrent scrapes.
  🔧 `SCRAPE_CONCURRENCY` (default 4) bounds parallelism via
  `errgroup.SetLimit`. Bump to 8 only if you see no throttling.
- ⚠ **Savings Plans** live under `savingsPlan/v1/…`, not `offers/v1.0/…`. We do
  not currently scrape them. Flagged for a future PR.
- ⚠ **`priceDimensions` can have a trailing suffix** (`.JRTCKXETXF.6YS6EN2CT7`)
  that makes naive keys non-stable across scrapes. 🔧 We build our own
  `PriceHash` from semantic fields (purchase option, unit, start usage amount,
  term length).

## Azure

**Source:** Azure Retail Prices API (public, no auth).
Endpoint: `https://prices.azure.com/api/retail/prices?$filter=serviceName eq '…'`

- ⚠ **OData injection.** Service names with apostrophes (none exist today, but
  nothing stops Microsoft from adding one) would break the filter.
  🔧 We `strings.ReplaceAll("'", "''")` before interpolation.
- ⚠ **Pagination is slow and unbounded.** Some services paginate through
  hundreds of pages at ~100 items each.
  🔧 `fetchPage` uses `context.NewRequestWithContext` so SIGTERM actually stops
  a scrape in progress, and retries use `contextSleep` instead of `time.Sleep`.
- ⚠ **`isPrimaryMeterRegion=false` is the common case for some services**,
  including DNS, VPN Gateway, and Load Balancer. Filtering it out drops most of their
  rows. 🔧 We keep a manual `usesVirtualRegions()` allow-list for services
  whose pricing is keyed by Zone / Global rather than ARM region.
- ⚠ **Virtual regions ("Zone 1", "Global", "US Gov Zone 1", "DE …") produce
  duplicate prices** when multiple ARM regions map to the same zone.
  🔧 We de-duplicate by `(purchaseOption, unit, startUsageAmount, USD, termLength)`
  for real regions, and by `(purchaseOption, startUsageAmount, termLength)` for
  virtual regions (last-price-wins).
- ⚠ **China cloud** requires `armRegionName`, not `location`. 🔧 Our region
  mapping in `azure.go` handles both.
- ⚠ **`tierMinimumUnits` on non-tiered prices is 0, not null.** 🔧 We treat 0
  as "no tier boundary" and emit `startUsageAmount="0"`.

## GCP

**Source:** Cloud Billing Catalog API.
Endpoint: `https://cloudbilling.googleapis.com/v1/services/<id>/skus`

- ⚠ **Requires an API key.** Free tier is generous; create one in the Google
  Cloud Console. Without `GCP_API_KEY`, `scrape --vendor gcp` fails fast (we
  check at command boundary, not deep inside the HTTP call).
- ⚠ **`Units` is a decimal string**, not a JSON number, to preserve precision.
  🔧 Parse as `int64` and combine with `Nanos` (int64): `units + nanos/1e9`.
- ⚠ **Sustained Use Discount (SUD) and Committed Use Discount (CUD) prices
  appear as separate SKUs alongside on-demand.** The CLI wants on-demand
  pricing. 🔧 `synthesizeMachineTypes` skips any description containing `"SUD"`
  or commitment terms.
- ⚠ **Compute Engine prices CPU and RAM separately.** There is no SKU for
  "n1-standard-2" at the API level. 🔧 We synthesize predefined machine-type
  products by combining CPU + RAM SKUs per family (`n1`, `n2`, `e2`, …) via
  `gcpFamilyCPUPatterns`.
- ⚠ **Pagination token can expire mid-scrape** if the catalog is re-ordered
  upstream. Today this manifests as a repeated page, not a hard error.
  🔧 We rely on upsert-by-hash to absorb the duplication; add dedup here if
  you observe real issues.
- ⚠ **Service IDs are static-but-unpublished** (e.g., Compute Engine is
  `6F81-5844-456A`). Google has never rotated one in five years, but there is
  no guarantee. 🔧 Hard-coded in `gcp.go::gcpServices`; easy to extend.
- ⚠ **Regions are a free-form string list per SKU.** Some SKUs list `global`,
  some list a dozen regions each. 🔧 We emit one product per region per SKU.

---

## Shared gotchas

- **Region naming is not harmonized.** `us-east-1` (AWS) vs `eastus` (Azure)
  vs `us-east1` (GCP) is a downstream-consumer problem; we store whatever the
  vendor returns.
- **Pricing is USD by default**, but *not* normalized across currencies.
  Anything emitted from a scraper is expected to be USD already (we convert
  CNY → USD for AWS China; see above).
- **Upsert-by-hash is authoritative.** If a scrape returns fewer products than
  the previous run (vendor dropped a SKU), `DeleteStaleProducts` removes
  anything older than `scrapeStart`. This is the only way to clean up retired
  SKUs without a parallel "known-SKU-set" table.
