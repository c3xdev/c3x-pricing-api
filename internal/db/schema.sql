-- C3X Pricing API: Database Schema
-- This file defines the complete schema. Applied as a single migration on first boot.

-- Products: the core pricing data table.
CREATE TABLE IF NOT EXISTS products (
    product_id      BIGSERIAL PRIMARY KEY,
    product_hash    TEXT NOT NULL UNIQUE,
    sku             TEXT NOT NULL,
    vendor_name     TEXT NOT NULL,
    region          TEXT,
    service         TEXT NOT NULL,
    product_family  TEXT,
    attributes      JSONB NOT NULL DEFAULT '{}',
    prices          JSONB NOT NULL DEFAULT '[]',
    updated_at      TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_products_vendor_service_region
    ON products(vendor_name, service, region);
CREATE INDEX IF NOT EXISTS idx_products_vendor_service
    ON products(vendor_name, service);
CREATE INDEX IF NOT EXISTS idx_products_attributes
    ON products USING GIN(attributes);
CREATE INDEX IF NOT EXISTS idx_products_sku
    ON products(sku);

-- Scrape runs: tracks each scraper invocation per vendor.
CREATE TABLE IF NOT EXISTS scrape_runs (
    id           BIGSERIAL PRIMARY KEY,
    vendor       TEXT        NOT NULL,
    started_at   TIMESTAMPTZ NOT NULL,
    finished_at  TIMESTAMPTZ,
    status       TEXT        NOT NULL, -- 'running' | 'success' | 'failed'
    products     INT         NOT NULL DEFAULT 0,
    deleted      INT         NOT NULL DEFAULT 0,
    error        TEXT
);

CREATE INDEX IF NOT EXISTS idx_scrape_runs_vendor_finished
    ON scrape_runs (vendor, finished_at DESC);
CREATE INDEX IF NOT EXISTS idx_scrape_runs_finished_at
    ON scrape_runs (finished_at) WHERE finished_at IS NOT NULL;

-- Price snapshots: append-only audit trail of price changes per scrape run.
CREATE TABLE IF NOT EXISTS price_snapshots (
    id              BIGSERIAL PRIMARY KEY,
    product_hash    TEXT NOT NULL,
    prices          JSONB NOT NULL,
    scrape_run_id   BIGINT REFERENCES scrape_runs(id) ON DELETE CASCADE,
    captured_at     TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_price_snapshots_product
    ON price_snapshots(product_hash, captured_at DESC);
CREATE INDEX IF NOT EXISTS idx_price_snapshots_run
    ON price_snapshots(scrape_run_id);
