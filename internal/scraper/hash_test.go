package scraper

import (
	"testing"
)

func TestProductHash_Deterministic(t *testing.T) {
	h1 := ProductHash("aws", "us-east-1", "AmazonEC2", "SKU123")
	h2 := ProductHash("aws", "us-east-1", "AmazonEC2", "SKU123")
	if h1 != h2 {
		t.Error("expected same hash for same input")
	}
}

func TestProductHash_DifferentInputs(t *testing.T) {
	h1 := ProductHash("aws", "us-east-1", "AmazonEC2", "SKU123")
	h2 := ProductHash("aws", "us-west-2", "AmazonEC2", "SKU123")
	if h1 == h2 {
		t.Error("expected different hashes for different regions")
	}
}

func TestPriceHash_IncludesProductHash(t *testing.T) {
	ph := ProductHash("aws", "us-east-1", "AmazonEC2", "SKU123")
	priceH := PriceHash(ph, "on_demand", "Hrs", "", "Compute", "", "", "")
	if len(priceH) < len(ph) {
		t.Error("price hash should be longer than product hash")
	}
}

func TestProductHash_Length(t *testing.T) {
	h := ProductHash("aws", "us-east-1", "AmazonEC2", "SKU123")
	if len(h) != 32 {
		t.Errorf("expected hash length 32, got %d", len(h))
	}
}
