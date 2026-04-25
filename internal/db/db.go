package db

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

type DB struct {
	Pool *pgxpool.Pool
}

// PoolOptions holds optional pool sizing parameters.
type PoolOptions struct {
	MaxConns int
	MinConns int
}

func New(ctx context.Context, databaseURL string, opts ...PoolOptions) (*DB, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse database URL: %w", err)
	}
	// O9: attach an OpenTelemetry tracer to every pooled connection so DB
	// calls appear as spans under the enclosing HTTP span. When no exporter
	// is configured the global tracer is a no-op, so this is free.
	config.ConnConfig.Tracer = otelpgx.NewTracer()

	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET statement_timeout = '300000'")
		return err
	}

	if len(opts) > 0 {
		o := opts[0]
		if o.MaxConns > 0 {
			config.MaxConns = int32(o.MaxConns) //nolint:gosec // value range validated by config
		}
		if o.MinConns > 0 {
			config.MinConns = int32(o.MinConns) //nolint:gosec // value range validated by config
		}
	}

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	return &DB{Pool: pool}, nil
}

// RunMigrations applies the database schema. All tables use IF NOT EXISTS so
// this is safe to call on every startup. It's a no-op if the schema is already
// in place. An advisory lock prevents concurrent runs from multiple replicas.
func (d *DB) RunMigrations(ctx context.Context) error {
	if _, err := d.Pool.Exec(ctx, "SELECT pg_advisory_lock(42)"); err != nil {
		return fmt.Errorf("failed to acquire schema lock: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = d.Pool.Exec(unlockCtx, "SELECT pg_advisory_unlock(42)")
	}()

	if _, err := d.Pool.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("failed to apply schema: %w", err)
	}

	slog.Info("schema applied")
	return nil
}



func (d *DB) PingCtx(ctx context.Context) error {
	return d.Pool.Ping(ctx)
}

func (d *DB) Close() {
	d.Pool.Close()
}

// AcquireScrapeLock takes a session-level advisory lock scoped to the given
// vendor so overlapping scrape runs do not trample each other. The lock is
// released when release() is called or the session ends. Returns (true, release)
// if the lock was acquired, (false, no-op) if another session holds it.
func (d *DB) AcquireScrapeLock(ctx context.Context, vendor string) (bool, func(), error) {
	conn, err := d.Pool.Acquire(ctx)
	if err != nil {
		return false, func() {}, fmt.Errorf("acquire connection for scrape lock: %w", err)
	}
	var locked bool
	if err := conn.QueryRow(ctx,
		"SELECT pg_try_advisory_lock(hashtext($1))", "scrape:"+vendor,
	).Scan(&locked); err != nil {
		conn.Release()
		return false, func() {}, fmt.Errorf("pg_try_advisory_lock: %w", err)
	}
	if !locked {
		conn.Release()
		return false, func() {}, nil
	}
	release := func() {
		// Best-effort unlock with a bounded timeout to avoid blocking shutdown.
		// Session-level advisory locks are also released when the connection is
		// returned to the pool, so this is defense-in-depth.
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx,
			"SELECT pg_advisory_unlock(hashtext($1))", "scrape:"+vendor)
		conn.Release()
	}
	return true, release, nil
}

// StartScrapeRun inserts a row into scrape_runs with status='running' and
// returns its id. Callers should use the returned id with FinishScrapeRun
// when the scrape completes or fails.
func (d *DB) StartScrapeRun(ctx context.Context, vendor string, startedAt time.Time) (int64, error) {
	var id int64
	err := d.Pool.QueryRow(ctx,
		`INSERT INTO scrape_runs (vendor, started_at, status) VALUES ($1, $2, 'running') RETURNING id`,
		vendor, startedAt,
	).Scan(&id)
	return id, err
}

// FinishScrapeRun updates a scrape_runs row with the final status, counts, and
// optional error message. status is "success" or "failed".
func (d *DB) FinishScrapeRun(ctx context.Context, id int64, status string, products int, deleted int64, scrapeErr error) error {
	var errMsg *string
	if scrapeErr != nil {
		s := scrapeErr.Error()
		if len(s) > 2000 {
			s = s[:2000]
		}
		errMsg = &s
	}
	_, err := d.Pool.Exec(ctx,
		`UPDATE scrape_runs SET finished_at = now(), status = $2, products = $3, deleted = $4, error = $5 WHERE id = $1`,
		id, status, products, deleted, errMsg,
	)
	return err
}

// PruneScrapeRuns deletes scrape_runs rows older than the given retention
// window. It is safe to call repeatedly; callers typically invoke it at the end
// of a successful scrape. Returns the number of rows deleted.
//
// We intentionally keep this as an app-level call (rather than a SQL trigger or
// cron) so the operation shows up in logs/metrics and can be disabled by
// passing retain=0 (which short-circuits without hitting the DB).
func (d *DB) PruneScrapeRuns(ctx context.Context, retain time.Duration) (int64, error) {
	if retain <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-retain)
	tag, err := d.Pool.Exec(ctx,
		`DELETE FROM scrape_runs WHERE finished_at IS NOT NULL AND finished_at < $1`,
		cutoff,
	)
	if err != nil {
		return 0, fmt.Errorf("prune scrape_runs: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ReapStaleScrapeRuns marks scrape runs that have been in 'running' status
// longer than timeout as 'failed'. This handles process crashes mid-scrape.
func (d *DB) ReapStaleScrapeRuns(ctx context.Context, timeout time.Duration) (int64, error) {
	cutoff := time.Now().Add(-timeout)
	tag, err := d.Pool.Exec(ctx,
		`UPDATE scrape_runs SET status = 'failed', finished_at = NOW(), error = 'reaped: exceeded timeout'
		 WHERE status = 'running' AND started_at < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("reap stale scrape_runs: %w", err)
	}
	return tag.RowsAffected(), nil
}

// CountProducts returns the total number of products in the database.
func (d *DB) CountProducts(ctx context.Context) (int64, error) {
	var count int64
	err := d.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM products`).Scan(&count)
	return count, err
}
