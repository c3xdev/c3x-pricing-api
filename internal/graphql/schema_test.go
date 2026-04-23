package graphql

// NOTE: Full resolver tests require a running database (e.g. via testcontainers).
// The parseProductFilter and parsePriceFilter functions are unexported and tightly
// coupled to the GraphQL schema, so they are best tested via integration tests.
// For now we test only the filter parsing logic with unit-level inputs.

import (
	"testing"
)

func TestParseProductFilter(t *testing.T) {
	m := map[string]interface{}{
		"vendorName":    "aws",
		"service":       "AmazonEC2",
		"productFamily": "Compute Instance",
		"region":        "us-east-1",
	}

	f := parseProductFilter(m)

	if f.VendorName == nil || *f.VendorName != "aws" {
		t.Errorf("expected vendorName=aws, got %v", f.VendorName)
	}
	if f.Service == nil || *f.Service != "AmazonEC2" {
		t.Errorf("expected service=AmazonEC2, got %v", f.Service)
	}
	if f.ProductFamily == nil || *f.ProductFamily != "Compute Instance" {
		t.Errorf("expected productFamily=Compute Instance, got %v", f.ProductFamily)
	}
	if f.Region == nil || *f.Region != "us-east-1" {
		t.Errorf("expected region=us-east-1, got %v", f.Region)
	}
}

func TestParseProductFilter_WithAttributes(t *testing.T) {
	m := map[string]interface{}{
		"vendorName": "aws",
		"attributeFilters": []interface{}{
			map[string]interface{}{
				"key":   "instanceType",
				"value": "m5.xlarge",
			},
			map[string]interface{}{
				"key":         "operatingSystem",
				"value_regex": "/Linux/i",
			},
		},
	}

	f := parseProductFilter(m)

	if len(f.AttributeFilters) != 2 {
		t.Fatalf("expected 2 attribute filters, got %d", len(f.AttributeFilters))
	}
	if f.AttributeFilters[0].Key != "instanceType" {
		t.Errorf("expected key=instanceType, got %s", f.AttributeFilters[0].Key)
	}
	if f.AttributeFilters[0].Value == nil || *f.AttributeFilters[0].Value != "m5.xlarge" {
		t.Error("expected value=m5.xlarge")
	}
	if f.AttributeFilters[1].ValueRegex == nil || *f.AttributeFilters[1].ValueRegex != "/Linux/i" {
		t.Error("expected value_regex=/Linux/i")
	}
}

func TestParsePriceFilter(t *testing.T) {
	m := map[string]interface{}{
		"purchaseOption": "on_demand",
		"unit":           "Hrs",
		"termLength":     "1yr",
	}

	f := parsePriceFilter(m)

	if f.PurchaseOption == nil || *f.PurchaseOption != "on_demand" {
		t.Errorf("expected purchaseOption=on_demand, got %v", f.PurchaseOption)
	}
	if f.Unit == nil || *f.Unit != "Hrs" {
		t.Errorf("expected unit=Hrs, got %v", f.Unit)
	}
	if f.TermLength == nil || *f.TermLength != "1yr" {
		t.Errorf("expected termLength=1yr, got %v", f.TermLength)
	}
}

func TestParsePriceFilter_Empty(t *testing.T) {
	m := map[string]interface{}{}
	f := parsePriceFilter(m)

	if f.PurchaseOption != nil {
		t.Error("expected nil PurchaseOption for empty filter")
	}
}
