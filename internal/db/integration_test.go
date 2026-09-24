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
	"fmt"
	"strings"
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

	// Second run re-sends only the first product, UNCHANGED. The upsert
	// no longer touches it, so its updated_at stays old; the seen-set is
	// what must keep it alive while h2 (not seen) is deleted.
	cutoff := time.Now()
	time.Sleep(50 * time.Millisecond)
	runID, err := db.StartScrapeRun(ctx, "aws", cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertProducts(ctx, products[:1]); err != nil {
		t.Fatalf("second UpsertProducts: %v", err)
	}
	if err := db.MarkSeen(ctx, runID, products[:1]); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}

	deleted, err := db.DeleteStaleProducts(ctx, "aws", runID, cutoff)
	if err != nil {
		t.Fatalf("DeleteStaleProducts: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted=%d, want 1", deleted)
	}
	var left []string
	rows, _ := db.Pool.Query(ctx, `SELECT product_hash FROM products ORDER BY 1`)
	for rows.Next() {
		var h string
		_ = rows.Scan(&h)
		left = append(left, h)
	}
	rows.Close()
	if len(left) != 1 || left[0] != "h1" {
		t.Fatalf("remaining products = %v, want [h1]", left)
	}

	// ClearSeen drops this run's rows once it is no longer running.
	if err := db.FinishScrapeRun(ctx, runID, "success", 1, deleted, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.ClearSeen(ctx, runID); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = db.Pool.QueryRow(ctx, `SELECT count(*) FROM scrape_seen`).Scan(&n)
	if n != 0 {
		t.Fatalf("scrape_seen rows after ClearSeen = %d", n)
	}
}

// ClearSeen must not drop the seen-set of another vendor's run that is
// still in flight (vendors scrape concurrently), but must sweep rows
// orphaned by a run that crashed.
func TestIntegration_ClearSeenKeepsRunningRuns(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	p := []Product{{ProductHash: "x"}}
	running, _ := db.StartScrapeRun(ctx, "azure", time.Now())
	crashed, _ := db.StartScrapeRun(ctx, "gcp", time.Now())
	mine, _ := db.StartScrapeRun(ctx, "aws", time.Now())
	for _, id := range []int64{running, crashed, mine} {
		if err := db.MarkSeen(ctx, id, p); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.FinishScrapeRun(ctx, crashed, "failed", 0, 0, nil)

	if err := db.ClearSeen(ctx, mine); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	rows, _ := db.Pool.Query(ctx, `SELECT run_id FROM scrape_seen`)
	for rows.Next() {
		var id int64
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 1 || ids[0] != running {
		t.Fatalf("scrape_seen run_ids = %v, want only the running run %d", ids, running)
	}
}

// An unchanged product must not be rewritten (no new tuple, updated_at
// kept); a changed one must be.
func TestIntegration_UpsertSkipsUnchangedRows(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	p := Product{
		ProductHash: "h1", SKU: "SKU-1", VendorName: "aws", Region: "us-east-1", Service: "AmazonEC2",
		Attributes: map[string]string{"instanceType": "t3.micro", "operatingSystem": "Linux"},
		Prices:     []Price{{PriceHash: "p1", Unit: "Hrs", USD: "0.0104"}},
	}
	if err := db.UpsertProducts(ctx, []Product{p}); err != nil {
		t.Fatal(err)
	}
	state := func() (xmin string, updated time.Time) {
		t.Helper()
		if err := db.Pool.QueryRow(ctx,
			`SELECT xmin::text, updated_at FROM products WHERE product_hash = 'h1'`).Scan(&xmin, &updated); err != nil {
			t.Fatal(err)
		}
		return
	}
	x0, u0 := state()

	time.Sleep(20 * time.Millisecond)
	if err := db.UpsertProducts(ctx, []Product{p}); err != nil {
		t.Fatal(err)
	}
	if x1, u1 := state(); x1 != x0 || !u1.Equal(u0) {
		t.Fatalf("unchanged product was rewritten: xmin %s->%s updated_at %v->%v", x0, x1, u0, u1)
	}

	p.Prices = []Price{{PriceHash: "p1", Unit: "Hrs", USD: "0.0200"}}
	if err := db.UpsertProducts(ctx, []Product{p}); err != nil {
		t.Fatal(err)
	}
	x2, u2 := state()
	if x2 == x0 || !u2.After(u0) {
		t.Fatalf("changed product was not rewritten: xmin %s->%s", x0, x2)
	}
	var usd string
	_ = db.Pool.QueryRow(ctx, `SELECT prices->0->>'USD' FROM products WHERE product_hash='h1'`).Scan(&usd)
	if usd != "0.0200" {
		t.Fatalf("USD=%s", usd)
	}
}

// Containment and ->> equality must select the same rows for the values
// the scrapers store (all attribute values are JSON strings), including
// escaping-sensitive values, empty strings and absent keys.
func TestIntegration_AttributeContainmentMatchesTextEquality(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	values := []string{"m5.xlarge", "M5.XLARGE", `Quote"d\Back`, "", "unicode é ü", "<&>", " padded "}
	var products []Product
	for i, v := range values {
		products = append(products, Product{
			ProductHash: fmt.Sprintf("h%d", i), SKU: "s", VendorName: "aws", Service: "svc",
			Attributes: map[string]string{"k": v, "other": "x"},
			Prices:     []Price{},
		})
	}
	products = append(products, Product{ProductHash: "nokey", SKU: "s", VendorName: "aws", Service: "svc",
		Attributes: map[string]string{"other": "x"}, Prices: []Price{}})
	if err := db.UpsertProducts(ctx, products); err != nil {
		t.Fatal(err)
	}

	for _, v := range append(values, "absent-value") {
		v := v
		got, err := db.QueryProducts(ctx, &ProductFilter{VendorName: strp("aws"), Service: strp("svc"),
			AttributeFilters: []AttributeFilter{{Key: "k", Value: &v}}})
		if err != nil {
			t.Fatal(err)
		}
		var want int
		if err := db.Pool.QueryRow(ctx,
			`SELECT count(*) FROM products WHERE vendor_name='aws' AND service='svc' AND attributes->>'k' = $1`, v).Scan(&want); err != nil {
			t.Fatal(err)
		}
		if len(got) != want {
			t.Errorf("value %q: containment matched %d rows, ->> matched %d", v, len(got), want)
		}
	}
	// Every stored attribute value is a JSON string.
	var nonString int
	_ = db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM products, jsonb_each(attributes) e WHERE jsonb_typeof(e.value) <> 'string'`).Scan(&nonString)
	if nonString != 0 {
		t.Fatalf("%d non-string attribute values stored", nonString)
	}
}

// EXPLAIN evidence: on a table shaped like production (one huge
// vendor+service partition), containment is served by the GIN index while
// the old ->> form has to filter every row of the partition.
func TestIntegration_AttributeEqualityUsesGINIndex(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	if _, err := db.Pool.Exec(ctx, `
		INSERT INTO products (product_hash, sku, vendor_name, region, service, product_family, attributes, prices)
		SELECT 'h' || n, 'sku' || n, 'aws', 'us-east-1', 'AmazonEC2', 'Compute Instance',
		       jsonb_build_object(
		         'instanceType', 'i' || (n % 700),
		         'operatingSystem', (ARRAY['Linux','Windows','RHEL','SUSE'])[1 + n % 4],
		         'tenancy', (ARRAY['Shared','Dedicated','Host'])[1 + n % 3],
		         'capacitystatus', (ARRAY['Used','UnusedCapacityReservation','AllocatedCapacityReservation'])[1 + n % 3],
		         'usagetype', 'BoxUsage:i' || (n % 700)),
		       '[{"USD":"0.1","unit":"Hrs","priceHash":"p"}]'::jsonb
		FROM generate_series(1, 200000) AS n`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `ANALYZE products`); err != nil {
		t.Fatal(err)
	}

	explain := func(q string, args ...interface{}) string {
		t.Helper()
		rows, err := db.Pool.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) "+q, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var b strings.Builder
		for rows.Next() {
			var line string
			_ = rows.Scan(&line)
			b.WriteString(line + "\n")
		}
		return b.String()
	}

	f := &ProductFilter{VendorName: strp("aws"), Service: strp("AmazonEC2"),
		ProductFamily: strp("Compute Instance"), Region: strp("us-east-1"), Limit: 50,
		AttributeFilters: []AttributeFilter{
			{Key: "instanceType", Value: strp("i42")},
			// 700 is a multiple of 4, so every i42 row is RHEL.
			{Key: "operatingSystem", Value: strp("RHEL")},
			{Key: "tenancy", Value: strp("Shared")},
		}}
	newSQL, newArgs, err := buildProductQuery(f)
	if err != nil {
		t.Fatal(err)
	}
	newPlan := explain(newSQL, newArgs...)
	oldPlan := explain(`SELECT product_hash, sku, vendor_name, region, service, product_family, attributes, prices FROM products
		WHERE vendor_name = $1 AND service = $2 AND product_family = $3 AND region = $4
		AND (attributes->>'usagetype' IS NULL OR attributes->>'usagetype' NOT LIKE 'Global%')
		AND attributes->>$5 = $6 AND attributes->>$7 = $8 AND attributes->>$9 = $10
		ORDER BY product_hash LIMIT 50`,
		"aws", "AmazonEC2", "Compute Instance", "us-east-1",
		"instanceType", "i42", "operatingSystem", "RHEL", "tenancy", "Shared")
	t.Logf("containment (new) plan:\n%s", newPlan)
	t.Logf("->> equality (old) plan:\n%s", oldPlan)

	if !strings.Contains(newPlan, "idx_products_attributes") {
		t.Fatalf("containment query did not use the GIN index:\n%s", newPlan)
	}
	if strings.Contains(oldPlan, "idx_products_attributes") {
		t.Fatalf("->> query unexpectedly used the GIN index:\n%s", oldPlan)
	}

	got, err := db.QueryProducts(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	var want int
	_ = db.Pool.QueryRow(ctx, `SELECT count(*) FROM products WHERE attributes->>'instanceType'='i42'
		AND attributes->>'operatingSystem'='RHEL' AND attributes->>'tenancy'='Shared'`).Scan(&want)
	if want > f.Limit {
		want = f.Limit
	}
	if len(got) != want || want == 0 {
		t.Fatalf("containment returned %d rows, ->> %d (limit %d)", len(got), want, f.Limit)
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
