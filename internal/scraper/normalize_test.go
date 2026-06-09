package scraper

// Per-service attribute normaliser tests. Each test covers one
// upstream-AWS attribute quirk the c3x catalog can't address without
// a small transform on the way into the DB.

import (
	"testing"
)

func TestNormaliseSageMakerStripsRoleSuffix(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"ml.c6g.large-Hosting":       "ml.c6g.large",
		"ml.r6id.xlarge-Notebook":    "ml.r6id.xlarge",
		"ml.c6i.12xlarge-Processing": "ml.c6i.12xlarge",
		// Preserve hyphens in instance type generations like p6-b200.
		"ml.p6-b200.48xlarge": "ml.p6-b200.48xlarge",
		// No-op when no role suffix.
		"ml.t3.xlarge": "ml.t3.xlarge",
	}
	for in, want := range cases {
		attrs := map[string]string{"instanceType": in}
		normalizeAWSAttributes("AmazonSageMaker", attrs)
		if got := attrs["instanceType"]; got != want {
			t.Errorf("instanceType=%q → %q, want %q", in, got, want)
		}
	}
}

func TestNormaliseEKSAddsModeDiscriminator(t *testing.T) {
	t.Parallel()
	cases := []struct {
		usagetype string
		wantMode  string
	}{
		{"EKS-Auto:m5.xlarge-management-hours", "Auto"},
		{"USE1-EKS-Auto:r7g.large", "Auto"},
		{"USE1-AmazonEKS-Hours:perCluster", "Classic"},
		{"USE2-EKS-Cluster-Hours", "Classic"},
		{"USE1-DataTransfer-Regional-Bytes", ""}, // not classified
	}
	for _, tc := range cases {
		attrs := map[string]string{"usagetype": tc.usagetype}
		normalizeAWSAttributes("AmazonEKS", attrs)
		if got := attrs["mode"]; got != tc.wantMode {
			t.Errorf("usagetype=%q → mode=%q, want %q", tc.usagetype, got, tc.wantMode)
		}
	}
}

func TestNormaliseEKSDoesNotOverrideExistingMode(t *testing.T) {
	t.Parallel()
	attrs := map[string]string{
		"usagetype": "EKS-Auto:m5.xlarge-management-hours",
		"mode":      "explicit-from-upstream",
	}
	normalizeAWSAttributes("AmazonEKS", attrs)
	if attrs["mode"] != "explicit-from-upstream" {
		t.Errorf("mode was overridden: %q", attrs["mode"])
	}
}

func TestNormaliseAthenaBackfillsProductFamily(t *testing.T) {
	t.Parallel()
	attrs := map[string]string{
		"operation":     "Query",
		"productFamily": "",
		"usagetype":     "USE1-DataScannedBytes",
	}
	normalizeAWSAttributes("AmazonAthena", attrs)
	if attrs["productFamily"] != "Query" {
		t.Errorf("productFamily not backfilled: %q", attrs["productFamily"])
	}
}

func TestNormaliseAthenaPreservesExistingProductFamily(t *testing.T) {
	t.Parallel()
	attrs := map[string]string{
		"operation":     "Query",
		"productFamily": "AlreadySet",
	}
	normalizeAWSAttributes("AmazonAthena", attrs)
	if attrs["productFamily"] != "AlreadySet" {
		t.Errorf("productFamily overridden: %q", attrs["productFamily"])
	}
}

func TestNormaliseSkipsUnknownService(t *testing.T) {
	t.Parallel()
	// Unknown service code: function must not panic or mutate
	// unrelated attributes.
	attrs := map[string]string{
		"instanceType": "ml.c6g.large-Hosting",
		"usagetype":    "EKS-Auto:foo",
	}
	normalizeAWSAttributes("UnknownServiceCode", attrs)
	if attrs["instanceType"] != "ml.c6g.large-Hosting" {
		t.Errorf("unrelated attr mutated: %q", attrs["instanceType"])
	}
}
