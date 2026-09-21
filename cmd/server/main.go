package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/c3xdev/c3x-pricing-api/internal/config"
	"github.com/c3xdev/c3x-pricing-api/internal/db"
	"github.com/c3xdev/c3x-pricing-api/internal/scraper"
	"github.com/c3xdev/c3x-pricing-api/internal/server"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "c3x-pricing-api",
		Short: "C3X Cloud Pricing API",
		Long:  "A self-hosted cloud pricing API that scrapes AWS, Azure, and GCP pricing data and serves it via GraphQL.",
	}

	rootCmd.AddCommand(serveCmd())
	rootCmd.AddCommand(scrapeCmd())
	rootCmd.AddCommand(seedCmd())

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// buildVersion is injected at link time via:
//
//	go build -ldflags "-X main.buildVersion=v1.0.0"
//
// Leave as "dev" otherwise.
var buildVersion = "dev"

func serveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Start the pricing API server",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := config.Load()
			if err := cfg.Validate(); err != nil {
				return err
			}
			ctx := context.Background()

			// O9: initialize OpenTelemetry. No-op when OTEL_EXPORTER_OTLP_ENDPOINT
			// is unset, so local dev pays nothing.
			shutdownTelemetry, err := server.InitTelemetry(ctx, "c3x-pricing-api", buildVersion)
			if err != nil {
				return fmt.Errorf("failed to initialize telemetry: %w", err)
			}
			defer func() {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = shutdownTelemetry(shutdownCtx)
			}()

			database, err := db.New(ctx, cfg.DatabaseURL, db.PoolOptions{
				MaxConns: cfg.DBMaxConns,
				MinConns: cfg.DBMinConns,
			})
			if err != nil {
				return fmt.Errorf("failed to connect to database: %w", err)
			}
			defer database.Close()

			if err := database.RunMigrations(ctx); err != nil {
				return fmt.Errorf("failed to run migrations: %w", err)
			}

			srv, err := server.New(cfg, database)
			if err != nil {
				return fmt.Errorf("failed to create server: %w", err)
			}

			return srv.Start()
		},
	}
}

func scrapeCmd() *cobra.Command {
	var vendor string

	cmd := &cobra.Command{
		Use:   "scrape",
		Short: "Scrape cloud pricing data",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := config.Load()
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			database, err := db.New(ctx, cfg.DatabaseURL)
			if err != nil {
				return fmt.Errorf("failed to connect to database: %w", err)
			}
			defer database.Close()

			if err := database.RunMigrations(ctx); err != nil {
				return fmt.Errorf("failed to run migrations: %w", err)
			}

			// O31: Validate GCP API key early before starting any scraping
			if (vendor == "gcp" || vendor == "all") && cfg.GCPAPIKey == "" {
				return fmt.Errorf("GCP_API_KEY is required for GCP scraping, set it in your .env file")
			}

			var scrapers []scraper.Scraper

			switch vendor {
			case "aws":
				scrapers = append(scrapers, scraper.NewAWSScraper(cfg))
			case "azure":
				scrapers = append(scrapers, scraper.NewAzureScraper(cfg))
			case "gcp":
				scrapers = append(scrapers, scraper.NewGCPScraper(cfg))
			case "all":
				scrapers = append(scrapers,
					scraper.NewAWSScraper(cfg),
					scraper.NewAzureScraper(cfg),
					scraper.NewGCPScraper(cfg),
				)
			default:
				return fmt.Errorf("unknown vendor: %s (use aws, azure, gcp, or all)", vendor)
			}

			// Run vendor scrapes concurrently. Each vendor acquires its own
			// advisory lock, so parallel runs of different vendors are safe.
			var (
				mu    sync.Mutex
				empty []string
			)
			g, gctx := errgroup.WithContext(ctx)
			for _, s := range scrapers {
				s := s
				g.Go(func() error {
					ok, err := runOneScrape(gctx, database, s, cfg.EnablePriceSnapshots)
					if err != nil {
						return err
					}
					if !ok {
						mu.Lock()
						empty = append(empty, strings.ToLower(s.Name()))
						mu.Unlock()
					}
					return nil
				})
			}
			if err := g.Wait(); err != nil {
				return err
			}

			// Fail the command when a vendor ingested nothing, so cron and CI
			// surface it instead of reporting a green run over stale data.
			// The run itself is already recorded as failed; collecting the
			// vendors here (rather than returning early) means one broken
			// vendor does not cancel the others mid-scrape.
			if len(empty) > 0 {
				sort.Strings(empty)
				return fmt.Errorf("scrape ingested 0 products for: %s (recorded as failed; check credentials and logs)",
					strings.Join(empty, ", "))
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&vendor, "vendor", "all", "Vendor to scrape: aws, azure, gcp, or all")
	return cmd
}

// emptyScrapeError returns the error to record for a run that ingested no
// products, or nil when the run produced data.
//
// No vendor legitimately has zero priced products, so an empty run always
// means something is broken: a revoked or invalid API key, a changed
// upstream API, or a network failure. Per-service errors are swallowed on
// purpose (one flaky service must not abort a whole vendor), so an empty
// run is frequently the only signal that every service failed, and
// marking it 'success' hides the outage behind a green row in
// scrape_runs for as long as nobody eyeballs the product counts.
func emptyScrapeError(products int, failedServices int64) error {
	if products > 0 {
		return nil
	}
	if failedServices > 0 {
		return fmt.Errorf("scrape ingested 0 products and %d service(s) failed: "+
			"check the vendor credentials and upstream API", failedServices)
	}
	return fmt.Errorf("scrape ingested 0 products: check the vendor credentials and upstream API")
}

// runOneScrape executes a single vendor's scrape run under a pg advisory lock,
// records its progress in scrape_runs, and updates stale row counts.
//
// The returned bool reports whether the run produced data. A run that
// ingested nothing is recorded as 'failed' and returns (false, nil): the
// nil error is deliberate, since vendors share an errgroup context and a
// returned error would cancel the other vendors' in-flight scrapes. The
// caller aggregates the false results and fails the command afterwards.
// A non-nil error is reserved for unrecoverable problems (lock, database).
// Per-service errors within a scraper stay logged-and-swallowed, so a run
// with partial data is still 'success', matching the existing semantics.
func runOneScrape(ctx context.Context, database *db.DB, s scraper.Scraper, recordSnapshots bool) (bool, error) {
	vendorName := strings.ToLower(s.Name())

	locked, unlock, err := database.AcquireScrapeLock(ctx, vendorName)
	if err != nil {
		return false, fmt.Errorf("acquire scrape lock for %s: %w", s.Name(), err)
	}
	if !locked {
		slog.Warn("another scrape is already running for this vendor; skipping", "vendor", s.Name())
		return true, nil
	}
	defer unlock()

	var scrapeStart time.Time
	if err := database.Pool.QueryRow(ctx, "SELECT now()").Scan(&scrapeStart); err != nil {
		return false, fmt.Errorf("failed to get DB time: %w", err)
	}

	runID, err := database.StartScrapeRun(ctx, vendorName, scrapeStart)
	if err != nil {
		return false, fmt.Errorf("start scrape run record: %w", err)
	}

	slog.Info("scraping pricing data", "vendor", s.Name(), "run_id", runID)

	var totalProducts int64
	handler := func(ctx context.Context, products []db.Product) error {
		if err := database.UpsertProducts(ctx, products); err != nil {
			return err
		}
		// A2: Record price snapshots for audit trail. Disabled by default —
		// the table has no reader and one row per product per run grew
		// unbounded (see ENABLE_PRICE_SNAPSHOTS). Best-effort when on:
		// snapshot failures don't abort the scrape, only log a warning.
		if recordSnapshots {
			if err := database.RecordPriceSnapshots(ctx, products, runID); err != nil {
				slog.Warn("failed to record price snapshots", "vendor", s.Name(), "error", err)
			}
		}
		current := atomic.AddInt64(&totalProducts, int64(len(products)))
		slog.Info("upserted batch", "vendor", s.Name(), "batch_size", len(products), "total_so_far", current)
		return nil
	}

	if err := s.ScrapeWithHandler(ctx, handler); err != nil {
		_ = database.FinishScrapeRun(context.Background(), runID, "failed", int(totalProducts), 0, err)
		return false, fmt.Errorf("scrape %s failed: %w", s.Name(), err)
	}

	// Consistency guards: only delete stale products when the scrape is complete
	// and trustworthy. Three checks prevent data loss:
	// 1. No per-service failures (a failed service means its products weren't refreshed)
	// 2. Product count didn't drop >50% vs previous run (catastrophic regression)
	// 3. We actually got products (empty scrape = total failure)
	failedSvcs := s.FailedServices()
	var prevCount int
	_ = database.Pool.QueryRow(ctx,
		`SELECT products FROM scrape_runs WHERE vendor=$1 AND status='success' ORDER BY finished_at DESC LIMIT 1`,
		vendorName,
	).Scan(&prevCount)

	var deleted int64
	currentCount := int(atomic.LoadInt64(&totalProducts))

	// An empty scrape is a failure, and has to be checked before the
	// cleanup switch below: per-service errors are deliberately swallowed
	// so one flaky service can't abort a run, which means a wholly broken
	// credential surfaces here as "every service failed, zero products"
	// rather than as a returned error. Recording that as success is what
	// let a dead GCP key sit unnoticed for a month.
	if emptyErr := emptyScrapeError(currentCount, failedSvcs); emptyErr != nil {
		slog.Error("scrape produced zero products, recording run as failed",
			"vendor", s.Name(), "failed_services", failedSvcs, "run_id", runID)
		if ferr := database.FinishScrapeRun(ctx, runID, "failed", 0, 0, emptyErr); ferr != nil {
			slog.Warn("failed to record scrape failure", "vendor", s.Name(), "run_id", runID, "error", ferr)
		}
		// Deliberately no SetScrapeLastSuccess: freshness must not
		// advance on a run that ingested nothing.
		return false, nil
	}

	switch {
	case failedSvcs > 0:
		slog.Warn("some services failed during scrape, skipping stale cleanup to preserve their data",
			"vendor", s.Name(), "failed_services", failedSvcs, "products", currentCount)
	case prevCount > 0 && currentCount < prevCount/2:
		slog.Error("scrape produced significantly fewer products than previous run, skipping stale cleanup",
			"vendor", s.Name(), "current", currentCount, "previous", prevCount)
	default:
		var err error
		deleted, err = database.DeleteStaleProducts(ctx, vendorName, scrapeStart)
		if err != nil {
			slog.Warn("failed to delete stale products", "vendor", s.Name(), "error", err)
			deleted = 0
		} else if deleted > 0 {
			slog.Info("deleted stale products", "vendor", s.Name(), "deleted", deleted)
		}
	}

	if err := database.FinishScrapeRun(ctx, runID, "success", int(totalProducts), deleted, nil); err != nil {
		slog.Warn("failed to record scrape success", "vendor", s.Name(), "run_id", runID, "error", err)
	}
	server.SetScrapeLastSuccess(vendorName, time.Now())

	// Opportunistically prune old scrape_runs rows. Best-effort: failures here
	// never abort the scrape itself.
	if pruned, pErr := database.PruneScrapeRuns(ctx, scrapeRunRetention()); pErr != nil {
		slog.Warn("failed to prune scrape_runs", "error", pErr)
	} else if pruned > 0 {
		slog.Info("pruned old scrape_runs", "rows", pruned)
	}

	slog.Info("scrape complete", "vendor", s.Name(), "products", totalProducts, "deleted", deleted)
	return true, nil
}

// scrapeRunRetention returns the retention window for scrape_runs rows.
// Defaults to 30 days; override with SCRAPE_RUNS_RETENTION_DAYS. A value of
// 0 disables pruning entirely.
func scrapeRunRetention() time.Duration {
	days := 30
	if v := os.Getenv("SCRAPE_RUNS_RETENTION_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			days = n
		} else {
			slog.Warn("SCRAPE_RUNS_RETENTION_DAYS is not a valid non-negative integer, using default", //nolint:gosec // G706: env var value in structured log, no injection risk
				slog.String("value", v), "default", days)
		}
	}
	return time.Duration(days) * 24 * time.Hour
}

func seedCmd() *cobra.Command {
	var filePath string

	cmd := &cobra.Command{
		Use:   "seed",
		Short: "Seed the database with pricing data from a JSON file",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := config.Load()
			ctx := context.Background()

			database, err := db.New(ctx, cfg.DatabaseURL)
			if err != nil {
				return fmt.Errorf("failed to connect to database: %w", err)
			}
			defer database.Close()

			if err := database.RunMigrations(ctx); err != nil {
				return fmt.Errorf("failed to run migrations: %w", err)
			}

			return database.SeedFromFile(ctx, filePath)
		},
	}

	cmd.Flags().StringVar(&filePath, "file", "", "Path to seed JSON file")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}
