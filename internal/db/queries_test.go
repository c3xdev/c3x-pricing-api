package db

import (
	"strings"
	"testing"
)

func TestMatchRegexPattern_CaseInsensitive(t *testing.T) {
	if !MatchRegexPattern("/test/i", "Test Value") {
		t.Error("expected case-insensitive match")
	}
	if !MatchRegexPattern("/test/i", "TEST") {
		t.Error("expected case-insensitive match for TEST")
	}
	if MatchRegexPattern("/test/i", "no match here") {
		t.Error("expected no match")
	}
}

func TestMatchRegexPattern_NormalRegex(t *testing.T) {
	if !MatchRegexPattern("/^aws/", "aws_instance") {
		t.Error("expected match for /^aws/")
	}
	if MatchRegexPattern("/^aws/", "not_aws") {
		t.Error("expected no match for /^aws/ against not_aws")
	}
}

func TestMatchRegexPattern_NegativeLookahead(t *testing.T) {
	// Negative lookahead: exclude values containing "Excluded"
	pattern := "/^(?!.*Excluded).*$/"
	if !MatchRegexPattern(pattern, "included value") {
		t.Error("expected match for value without excluded term")
	}
	if MatchRegexPattern(pattern, "this is Excluded") {
		t.Error("expected no match for value containing excluded term")
	}
}

func TestMatchRegexPattern_TooLongRegex(t *testing.T) {
	long := "/" + strings.Repeat("a", maxRegexLength+1) + "/"
	if MatchRegexPattern(long, "anything") {
		t.Error("expected false for too-long regex")
	}
}

func TestMatchRegexPattern_BareRegex(t *testing.T) {
	if !MatchRegexPattern("aws.*instance", "aws_instance_type") {
		t.Error("expected match for bare regex")
	}
	if MatchRegexPattern("aws.*instance", "gcp_instance") {
		t.Error("expected no match for bare regex")
	}
}

func TestMatchRegexPattern_InvalidSlashFormat(t *testing.T) {
	// Single slash without closing slash
	if MatchRegexPattern("/noclose", "noclose") {
		t.Error("expected false for malformed /pattern without closing slash")
	}
}

func TestParseRegexForPostgres(t *testing.T) {
	tests := []struct {
		input           string
		wantRegex       string
		wantInsensitive bool
	}{
		{"/^aws/i", "^aws", true},
		{"/test/", "test", false},
		{"/foo|bar/i", "foo|bar", true},
		{"bare", "bare", false},
		{"/noclosing", "", false},
	}

	for _, tt := range tests {
		regex, insensitive := parseRegexForPostgres(tt.input)
		if regex != tt.wantRegex || insensitive != tt.wantInsensitive {
			t.Errorf("parseRegexForPostgres(%q) = (%q, %v), want (%q, %v)",
				tt.input, regex, insensitive, tt.wantRegex, tt.wantInsensitive)
		}
	}
}

func TestNormalizeRegexForPrefix(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{`\-RDS\:Multi\-AZ\-GP3\-Storage$`, `(^|-)RDS\:Multi\-AZ\-GP3\-Storage$`},
		{`\-RDS\:GP3\-Storage$`, `(^|-)RDS\:GP3\-Storage$`},
		{`GP3-Storage$`, `GP3-Storage$`}, // no leading \-, unchanged
		{`^aws.*$`, `^aws.*$`},           // no leading \-, unchanged
		{``, ``},                         // empty string
		{`\-`, `(^|-)`},                  // just the escaped hyphen
	}

	for _, tt := range tests {
		got := normalizeRegexForPrefix(tt.input)
		if got != tt.want {
			t.Errorf("normalizeRegexForPrefix(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestMatchRegexPattern_RegionPrefixNormalization(t *testing.T) {
	// The CLI sends /\-RDS\:Multi\-AZ\-GP3\-Storage$/ which should match both:
	// - "USE1-RDS:Multi-AZ-GP3-Storage" (most regions, with prefix)
	// - "RDS:Multi-AZ-GP3-Storage" (us-east-1, no prefix)
	pattern := `/\-RDS\:Multi\-AZ\-GP3\-Storage$/`

	if !MatchRegexPattern(pattern, "USE1-RDS:Multi-AZ-GP3-Storage") {
		t.Error("expected match for region-prefixed usagetype")
	}
	if !MatchRegexPattern(pattern, "RDS:Multi-AZ-GP3-Storage") {
		t.Error("expected match for us-east-1 usagetype (no prefix)")
	}
	if MatchRegexPattern(pattern, "RDS:GP3-Storage") {
		t.Error("expected no match for non-Multi-AZ usagetype")
	}
}

func strp(s string) *string { return &s }

func TestBuildProductQuery_EqualityUsesContainment(t *testing.T) {
	f := &ProductFilter{
		VendorName: strp("aws"), Service: strp("AmazonEC2"),
		ProductFamily: strp("Compute Instance"), Region: strp("us-east-1"),
		AttributeFilters: []AttributeFilter{
			{Key: "instanceType", Value: strp("m5.xlarge")},
			{Key: "volumeType", Value: strp(`Quote"d\Back`)},
			{Key: "usagetype", ValueRegex: strp(`/\-RDS\:GP3\-Storage$/i`)},
		},
		Limit: 50,
	}
	q, args, err := buildProductQuery(f)
	if err != nil {
		t.Fatal(err)
	}
	want := `SELECT product_hash, sku, vendor_name, region, service, product_family, attributes, prices FROM products WHERE 1=1` +
		` AND vendor_name = $1 AND service = $2 AND product_family = $3 AND region = $4` +
		` AND (attributes->>'usagetype' IS NULL OR attributes->>'usagetype' NOT LIKE 'Global%')` +
		` AND attributes @> $5::jsonb AND attributes @> $6::jsonb` +
		` AND attributes->>$7 ~* $8` +
		` ORDER BY product_hash LIMIT $9`
	if q != want {
		t.Fatalf("SQL mismatch\n got: %s\nwant: %s", q, want)
	}
	wantArgs := []interface{}{"aws", "AmazonEC2", "Compute Instance", "us-east-1",
		`{"instanceType":"m5.xlarge"}`, `{"volumeType":"Quote\"d\\Back"}`,
		"usagetype", `(^|-)RDS\:GP3\-Storage$`, 50}
	if len(args) != len(wantArgs) {
		t.Fatalf("args = %#v", args)
	}
	for i := range wantArgs {
		if args[i] != wantArgs[i] {
			t.Errorf("arg %d = %#v, want %#v", i+1, args[i], wantArgs[i])
		}
	}
	if strings.Contains(q, "attributes->>$5 =") {
		t.Fatal("equality must not use ->> (it cannot use the GIN index)")
	}
}

// Two equality filters on one key must stay two clauses (and so match
// nothing when the values differ), exactly as the old ->> form did.
func TestBuildProductQuery_SameKeyTwiceStaysTwoClauses(t *testing.T) {
	f := &ProductFilter{VendorName: strp("aws"), Service: strp("s"),
		AttributeFilters: []AttributeFilter{{Key: "k", Value: strp("a")}, {Key: "k", Value: strp("b")}}}
	q, args, err := buildProductQuery(f)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(q, "attributes @>") != 2 || args[2] != `{"k":"a"}` || args[3] != `{"k":"b"}` {
		t.Fatalf("q=%s args=%v", q, args)
	}
}

func TestBuildProductQuery_LimitAndPagination(t *testing.T) {
	base := func() *ProductFilter { return &ProductFilter{VendorName: strp("aws"), Service: strp("s")} }

	_, args, _ := buildProductQuery(base())
	if args[len(args)-1] != maxProductLimit {
		t.Fatalf("unset limit should default to %d, got %v", maxProductLimit, args[len(args)-1])
	}

	f := base()
	f.Limit = 999999
	_, args, _ = buildProductQuery(f)
	if args[len(args)-1] != maxProductLimit {
		t.Fatalf("oversized limit should clamp, got %v", args[len(args)-1])
	}

	f = base()
	f.Limit, f.Offset = 10, 20
	q, args, _ := buildProductQuery(f)
	if !strings.HasSuffix(q, "ORDER BY product_hash LIMIT $3 OFFSET $4") || args[2] != 10 || args[3] != 20 {
		t.Fatalf("offset path: %s %v", q, args)
	}

	f = base()
	f.AfterHash = "abc"
	q, _, _ = buildProductQuery(f)
	if !strings.HasSuffix(q, "AND product_hash > $3 ORDER BY product_hash LIMIT $4") {
		t.Fatalf("keyset path: %s", q)
	}
}

func TestBuildProductQuery_RegexTooLong(t *testing.T) {
	f := &ProductFilter{AttributeFilters: []AttributeFilter{{Key: "k", ValueRegex: strp(strings.Repeat("a", maxRegexLength+1))}}}
	if _, _, err := buildProductQuery(f); err == nil {
		t.Fatal("expected error")
	}
}

func TestUpsertSkipsUnchangedRows(t *testing.T) {
	for _, col := range []string{"prices", "attributes", "sku"} {
		if !strings.Contains(upsertChangedOnly, "products."+col+" IS DISTINCT FROM EXCLUDED."+col) {
			t.Errorf("upsert WHERE clause must compare %s", col)
		}
	}
}
