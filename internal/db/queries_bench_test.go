package db

import "testing"

func BenchmarkFilterPrices(b *testing.B) {
	prices := []Price{
		{PurchaseOption: "on_demand", Unit: "Hrs", USD: "0.192", StartUsageAmount: "0"},
		{PurchaseOption: "reserved", Unit: "Hrs", USD: "0.120", TermLength: "1yr"},
		{PurchaseOption: "reserved", Unit: "Hrs", USD: "0.080", TermLength: "3yr"},
		{PurchaseOption: "on_demand", Unit: "Hrs", USD: "0.096", StartUsageAmount: "1"},
	}
	filter := &PriceFilter{
		PurchaseOption: strPtr("on_demand"),
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		FilterPrices(prices, filter)
	}
}

func BenchmarkMatchRegexPattern_Simple(b *testing.B) {
	for i := 0; i < b.N; i++ {
		MatchRegexPattern("/NatGateway-Hours/", "USW2-NatGateway-Hours")
	}
}

func BenchmarkMatchRegexPattern_CaseInsensitive(b *testing.B) {
	for i := 0; i < b.N; i++ {
		MatchRegexPattern("/linux/i", "Linux")
	}
}

func BenchmarkMatchRegexPattern_RegionPrefix(b *testing.B) {
	for i := 0; i < b.N; i++ {
		MatchRegexPattern(`/\-RDS\:Multi\-AZ\-GP3\-Storage$/`, "RDS:Multi-AZ-GP3-Storage")
	}
}

func BenchmarkMatchRegexPattern_NegativeLookahead(b *testing.B) {
	for i := 0; i < b.N; i++ {
		MatchRegexPattern("/^(?!.*(Expired|Free)$).*$/i", "Standard_D2s_v3")
	}
}

func BenchmarkNormalizePurchaseOption(b *testing.B) {
	for i := 0; i < b.N; i++ {
		normalizePurchaseOption("OnDemand")
	}
}

func strPtr(s string) *string { return &s }
