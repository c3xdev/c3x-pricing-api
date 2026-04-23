package scraper

import "testing"

func BenchmarkProductHash(b *testing.B) {
	for i := 0; i < b.N; i++ {
		ProductHash("aws", "us-east-1", "AmazonEC2", "ABCDEFGH12345678")
	}
}

func BenchmarkPriceHash(b *testing.B) {
	prodHash := ProductHash("aws", "us-east-1", "AmazonEC2", "ABCDEFGH12345678")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		PriceHash(prodHash, "on_demand", "Hrs", "0", "On Demand Linux m5.xlarge", "", "", "")
	}
}
