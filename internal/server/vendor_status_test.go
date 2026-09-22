package server

import (
	"testing"
	"time"
)

// TestVendorStatus pins the states /status reports. The motivating case is
// the third one: a vendor whose credential is revoked scrapes nothing, and
// under the previous logic kept reporting "ready" forever because readiness
// was read off the newest successful run and nothing ever downgraded it.
func TestVendorStatus(t *testing.T) {
	t.Parallel()
	now := time.Now()
	recent := now.Add(-2 * time.Hour)
	old := now.Add(-72 * time.Hour)

	cases := []struct {
		name            string
		sawSuccess      bool
		successProducts int64
		latestStatus    string
		finishedAt      *time.Time
		want            string
	}{
		{"no runs at all", false, 0, "", nil, "never"},
		{"run in flight", false, 0, "running", nil, "scraping"},
		{"healthy", true, 2_400_000, "success", &recent, "ready"},

		// The GCP case: a run recorded success while ingesting nothing.
		{"last success ingested nothing", true, 0, "success", &recent, "empty"},

		// Once the scraper started failing honestly, the failure must win
		// over the older successful run that still has a product count.
		{"latest run failed, older success exists", true, 2_400_000, "failed", &recent, "failed"},

		// A failure outranks staleness: it is the more actionable fact.
		{"failed and old", true, 2_400_000, "failed", &old, "failed"},

		{"nothing finished in 48h", true, 2_400_000, "success", &old, "stale"},

		// Running outranks everything: the picture is mid-change.
		{"scraping over a previous failure", true, 10, "running", &recent, "scraping"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := vendorStatus(tc.sawSuccess, tc.successProducts, tc.latestStatus, tc.finishedAt)
			if got != tc.want {
				t.Errorf("vendorStatus(sawSuccess=%v, products=%d, latest=%q) = %q, want %q",
					tc.sawSuccess, tc.successProducts, tc.latestStatus, got, tc.want)
			}
		})
	}
}

// TestVendorStatusNeverReportsReadyWithoutData is the invariant that matters:
// no combination of inputs may report "ready" while the last successful
// scrape ingested nothing.
func TestVendorStatusNeverReportsReadyWithoutData(t *testing.T) {
	t.Parallel()
	now := time.Now()
	for _, latest := range []string{"", "success", "failed", "running"} {
		for _, fin := range []*time.Time{nil, &now} {
			if got := vendorStatus(true, 0, latest, fin); got == "ready" {
				t.Errorf("reported ready with 0 products (latest=%q, finishedAt=%v)", latest, fin)
			}
		}
	}
}
