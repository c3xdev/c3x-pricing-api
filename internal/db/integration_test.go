//go:build integration

// Package db integration tests spin up a real Postgres via testcontainers and
// exercise migrations, advisory locks, scrape_run lifecycle, product upserts,
// stale-product deletion, and scrape_runs retention. Run with:
//
//	go test -tags=integration ./internal/db/...
//
// Requires Docker. On CI we rely on the Docker-in-Docker runners; locally,
// ensure `docker ps` succeeds before running.
package db

import (
	"context"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

func newTestDB(t *testing.T) (*DB, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	container, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("c3x_test"),
		tcpostgres.WithUsername("c3x"),
		tcpostgres.WithPassword("c3x"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	database, err := New(context.Background(), connStr)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	if err := database.RunMigrations(context.Background()); err != nil {
		t.Fatalf("migrations: %v", err)
	}

	cleanup := func() {
		database.Close()
		_ = container.Terminate(context.Background())
	}
	return database, cleanup
}

func TestIntegration_AdvisoryLock_IsExclusivePerVendor(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	locked1, unlock1, err := db.AcquireScrapeLock(ctx, "aws")
	if err != nil || !locked1 {
		t.Fatalf("first acquire: locked=%v err=%v", locked1, err)
	}
	defer unlock1()

	// Second attempt for the same vendor must fail fast.
	locked2, unlock2, err := db.AcquireScrapeLock(ctx, "aws")
	if err != nil {
		t.Fatalf("second acquire: err=%v", err)
	}
	if locked2 {
		unlock2()
		t.Fatal("second acquire should have returned locked=false")
	}

	// A different vendor is unaffected.
	locked3, unlock3, err := db.AcquireScrapeLock(ctx, "azure")
	if err != nil || !locked3 {
		t.Fatalf("azure acquire: locked=%v err=%v", locked3, err)
	}
	unlock3()
}

func TestIntegration_ScrapeRunLifecycle(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	start := time.Now().Add(-time.Minute)
	id, err := db.StartScrapeRun(ctx, "aws", start)
	if err != nil || id <= 0 {
		t.Fatalf("StartScrapeRun: id=%d err=%v", id, err)
	}

	if err := db.FinishScrapeRun(ctx, id, "success", 42, 3, nil); err != nil {
		t.Fatalf("FinishScrapeRun: %v", err)
	}

	var status string
	var products int
	var deleted int64
	err = db.Pool.QueryRow(ctx,
		`SELECT status, products, deleted FROM scrape_runs WHERE id = $1`, id,
	).Scan(&status, &products, &deleted)
	if err != nil {
		t.Fatalf("select scrape_run: %v", err)
	}
	if status != "success" || products != 42 || deleted != 3 {
		t.Fatalf("unexpected row: status=%s products=%d deleted=%d", status, products, deleted)
	}
}

func TestIntegration_UpsertAndDeleteStale(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	products := []Product{
		{
			ProductHash: "h1", SKU: "SKU-1", VendorName: "aws", Region: "us-east-1",
			Service: "AmazonEC2", ProductFamily: "Compute",
			Attributes: map[string]string{"instanceType": "t3.micro"},
			Prices:     []Price{{PriceHash: "p1", Unit: "Hrs", USD: "0.0104"}},
		},
		{
			ProductHash: "h2", SKU: "SKU-2", VendorName: "aws", Region: "us-east-1",
			Service: "AmazonEC2", ProductFamily: "Compute",
			Attributes: map[string]string{"instanceType": "t3.small"},
			Prices:     []Price{{PriceHash: "p2", Unit: "Hrs", USD: "0.0208"}},
		},
	}
	if err := db.UpsertProducts(ctx, products); err != nil {
		t.Fatalf("UpsertProducts: %v", err)
	}

	// Bump only the first product; the second should be eligible for stale delete.
	cutoff := time.Now()
	time.Sleep(50 * time.Millisecond)
	if err := db.UpsertProducts(ctx, products[:1]); err != nil {
		t.Fatalf("second UpsertProducts: %v", err)
	}

	deleted, err := db.DeleteStaleProducts(ctx, "aws", cutoff)
	if err != nil {
		t.Fatalf("DeleteStaleProducts: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted=%d, want 1", deleted)
	}
}

func TestIntegration_PruneScrapeRuns(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	// Insert a finished run with an old finished_at value.
	id, err := db.StartScrapeRun(ctx, "aws", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishScrapeRun(ctx, id, "success", 0, 0, nil); err != nil {
		t.Fatal(err)
	}
	_, err = db.Pool.Exec(ctx,
		`UPDATE scrape_runs SET finished_at = now() - interval '40 days' WHERE id = $1`, id)
	if err != nil {
		t.Fatal(err)
	}

	pruned, err := db.PruneScrapeRuns(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("PruneScrapeRuns: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("pruned=%d, want 1", pruned)
	}

	// retain=0 must short-circuit without error.
	if n, err := db.PruneScrapeRuns(ctx, 0); err != nil || n != 0 {
		t.Fatalf("retain=0: n=%d err=%v", n, err)
	}
}
