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
		{`GP3-Storage$`, `GP3-Storage$`},         // no leading \-, unchanged
		{`^aws.*$`, `^aws.*$`},                    // no leading \-, unchanged
		{``, ``},                                  // empty string
		{`\-`, `(^|-)`},                           // just the escaped hyphen
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

