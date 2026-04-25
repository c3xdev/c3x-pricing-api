//go:build integration

package db

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestIntegration_SIGTERM_MidScrape models what runOneScrape does when the
// process receives SIGTERM mid-Scrape: the context is cancelled, the
// scrape_runs row is marked "failed", and the per-vendor advisory lock is
// released so the next scrape can proceed immediately.
func TestIntegration_SIGTERM_MidScrape(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	rootCtx, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()

	// Acquire the lock exactly as runOneScrape does.
	locked, unlock, err := db.AcquireScrapeLock(rootCtx, "aws")
	if err != nil || !locked {
		t.Fatalf("acquire lock: locked=%v err=%v", locked, err)
	}

	runID, err := db.StartScrapeRun(rootCtx, "aws", time.Now())
	if err != nil {
		t.Fatalf("StartScrapeRun: %v", err)
	}

	// Simulate the blocking scrape goroutine.
	scrapeErrCh := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-rootCtx.Done()
		scrapeErrCh <- rootCtx.Err()
	}()

	// Simulate SIGTERM.
	cancelRoot()
	wg.Wait()
	scrapeErr := <-scrapeErrCh
	if !errors.Is(scrapeErr, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", scrapeErr)
	}

	// runOneScrape always uses context.Background() for the post-cancel bookkeeping
	// so the DB write isn't itself cancelled. Replicate that here.
	if err := db.FinishScrapeRun(context.Background(), runID, "failed", 0, 0, scrapeErr); err != nil {
		t.Fatalf("FinishScrapeRun after cancel: %v", err)
	}
	unlock() // defers always run, including on the cancel path.

	// Verify the row is recorded as failed with the cancellation error.
	var status string
	var errMsg *string
	err = db.Pool.QueryRow(context.Background(),
		`SELECT status, error FROM scrape_runs WHERE id = $1`, runID).Scan(&status, &errMsg)
	if err != nil {
		t.Fatalf("select row: %v", err)
	}
	if status != "failed" {
		t.Fatalf("status = %q, want failed", status)
	}
	if errMsg == nil || *errMsg == "" {
		t.Fatal("expected non-empty error message")
	}

	// The next scrape for the same vendor must be immediately acquirable
	// because unlock ran. This is the critical regression: pre-advisory-lock
	// crashes used to leave "zombie" scrapes that blocked subsequent runs.
	locked2, unlock2, err := db.AcquireScrapeLock(context.Background(), "aws")
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	if !locked2 {
		t.Fatal("second acquire should succeed after unlock")
	}
	unlock2()
}
