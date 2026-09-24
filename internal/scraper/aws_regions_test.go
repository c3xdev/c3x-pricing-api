package scraper

import "testing"

func TestAWSProductRegion(t *testing.T) {
	cases := []struct {
		name  string
		attrs map[string]string
		want  string
	}{
		{"region code wins for a region", map[string]string{"location": "Europe (Spain)", "locationType": "AWS Region", "regionCode": "eu-south-2"}, "eu-south-2"},
		{"new region without a map entry", map[string]string{"location": "Asia Pacific (Somewhere)", "locationType": "AWS Region", "regionCode": "ap-new-1"}, "ap-new-1"},
		{"renamed region without regionCode", map[string]string{"location": "Europe (Zurich)"}, "eu-central-2"},
		{"legacy name", map[string]string{"location": "EU (Ireland)"}, "eu-west-1"},
		// A Local Zone carries its parent's regionCode; folding it in would
		// make us-east-1 lookups match Boston's prices as well.
		{"local zone keeps its own location", map[string]string{"location": "US East (Boston)", "locationType": "AWS Local Zone", "regionCode": "us-east-1"}, "US East (Boston)"},
		{"global", map[string]string{"location": "Global"}, ""},
		{"no location", map[string]string{}, ""},
	}
	for _, c := range cases {
		if got := awsProductRegion(c.attrs); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
