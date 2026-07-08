package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
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
			g, gctx := errgroup.WithContext(ctx)
			for _, s := range scrapers {
				s := s
				g.Go(func() error {
					return runOneScrape(gctx, database, s, cfg.EnablePriceSnapshots)
				})
			}
			if err := g.Wait(); err != nil {
				return err
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&vendor, "vendor", "all", "Vendor to scrape: aws, azure, gcp, or all")
	return cmd
}

// runOneScrape executes a single vendor's scrape run under a pg advisory lock,
// records its progress in scrape_runs, and updates stale row counts. It returns
// an error only for unrecoverable problems; per-service errors within a scraper
// are logged and the run is marked 'success' with whatever products came back,
// which matches the existing partial-data semantics.
func runOneScrape(ctx context.Context, database *db.DB, s scraper.Scraper, recordSnapshots bool) error {
	vendorName := strings.ToLower(s.Name())

	locked, unlock, err := database.AcquireScrapeLock(ctx, vendorName)
	if err != nil {
		return fmt.Errorf("acquire scrape lock for %s: %w", s.Name(), err)
	}
	if !locked {
		slog.Warn("another scrape is already running for this vendor; skipping", "vendor", s.Name())
		return nil
	}
	defer unlock()

	var scrapeStart time.Time
	if err := database.Pool.QueryRow(ctx, "SELECT now()").Scan(&scrapeStart); err != nil {
		return fmt.Errorf("failed to get DB time: %w", err)
	}

	runID, err := database.StartScrapeRun(ctx, vendorName, scrapeStart)
	if err != nil {
		return fmt.Errorf("start scrape run record: %w", err)
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
		return fmt.Errorf("scrape %s failed: %w", s.Name(), err)
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

	switch {
	case failedSvcs > 0:
		slog.Warn("some services failed during scrape, skipping stale cleanup to preserve their data",
			"vendor", s.Name(), "failed_services", failedSvcs, "products", currentCount)
	case prevCount > 0 && currentCount < prevCount/2:
		slog.Error("scrape produced significantly fewer products than previous run, skipping stale cleanup",
			"vendor", s.Name(), "current", currentCount, "previous", prevCount)
	case currentCount == 0:
		slog.Warn("scrape produced zero products, skipping stale cleanup", "vendor", s.Name())
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
	return nil
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
