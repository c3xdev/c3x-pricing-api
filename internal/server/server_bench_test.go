package server

import "testing"

func BenchmarkComputeQueryDepth_Simple(b *testing.B) {
	query := `{ products(filter: {vendorName: "aws"}) { prices { USD } } }`
	for i := 0; i < b.N; i++ {
		computeQueryDepth(query)
	}
}

func BenchmarkComputeQueryDepth_Deep(b *testing.B) {
	query := `{ products(filter: {vendorName: "aws"}) { productHash prices(filter: {purchaseOption: "on_demand"}) { priceHash USD unit startUsageAmount description } } }`
	for i := 0; i < b.N; i++ {
		computeQueryDepth(query)
	}
}
