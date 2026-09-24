package scraper

import (
	"slices"
	"sort"
	"testing"

	"github.com/c3xdev/c3x-pricing-api/internal/db"
)

// azurePricesByKey runs rows through the builder and indexes the result
// by "region|meterName", each with its sorted USD prices.
func azurePricesByKey(t *testing.T, rows []azureItem) map[string][]string {
	t.Helper()
	b := newAzureProductBuilder()
	for _, r := range rows {
		b.add(r)
	}
	out := map[string][]string{}
	for _, p := range b.products() {
		key := p.Region + "|" + p.Attributes["meterName"]
		if _, dup := out[key]; dup {
			t.Fatalf("two products for %s", key)
		}
		var usd []string
		for _, pr := range p.Prices {
			usd = append(usd, pr.PurchaseOption+" "+pr.USD)
		}
		sort.Strings(usd)
		out[key] = usd
	}
	return out
}

func firewallRow(region, meter string, price float64, primary bool) azureItem {
	return azureItem{
		RetailPrice: price, UnitOfMeasure: "1 GB", ArmRegionName: region,
		ServiceName: "Azure Firewall", ServiceFamily: "Networking",
		ProductName: "Azure Firewall", SkuName: "Standard", MeterName: meter,
		Type: "Consumption", IsPrimaryMeterRegion: primary,
	}
}

// A meter that is primary in one region and re-listed as non-primary in
// others must be priced in every region it is listed in.
func TestAzureNonPrimaryRowFillsMissingRegion(t *testing.T) {
	got := azurePricesByKey(t, []azureItem{
		firewallRow("", "Standard Data Processed", 0.016, true), // armRegionName "" → Global
		firewallRow("eastus", "Standard Data Processed", 0.016, false),
		firewallRow("westeurope", "Standard Data Processed", 0.016, false),
	})
	for _, key := range []string{"Global|Standard Data Processed", "eastus|Standard Data Processed", "westeurope|Standard Data Processed"} {
		if want := []string{"Consumption 0.0160000000"}; !slices.Equal(got[key], want) {
			t.Errorf("%s = %v, want %v", key, got[key], want)
		}
	}
}

// When a slot has a primary row, a non-primary row for the same slot is
// dropped, so a max-price consumer cannot pick it up.
func TestAzurePrimaryRowWinsItsSlot(t *testing.T) {
	got := azurePricesByKey(t, []azureItem{
		firewallRow("eastus", "Standard Data Processed", 0.99, false), // arrives first
		firewallRow("eastus", "Standard Data Processed", 0.016, true),
	})
	if want := []string{"Consumption 0.0160000000"}; !slices.Equal(got["eastus|Standard Data Processed"], want) {
		t.Errorf("got %v, want only the primary price %v", got["eastus|Standard Data Processed"], want)
	}
}

// A primary row only covers its own slot: a non-primary Consumption row
// still fills in next to a primary Reservation row of the same meter.
func TestAzurePrimaryRowCoversOnlyItsSlot(t *testing.T) {
	cons := firewallRow("eastus", "Standard Data Processed", 0.016, false)
	resv := firewallRow("eastus", "Standard Data Processed", 100, true)
	resv.Type, resv.ReservationTerm = "Reservation", "1 Year"
	got := azurePricesByKey(t, []azureItem{cons, resv})
	want := []string{"Consumption 0.0160000000", "Reservation 100.0000000000"}
	if !slices.Equal(got["eastus|Standard Data Processed"], want) {
		t.Errorf("got %v, want %v", got["eastus|Standard Data Processed"], want)
	}
}

// Rows the scraper already kept regardless of the flag keep their
// previous behaviour.
func TestAzureBypassListUnchanged(t *testing.T) {
	a := azureItem{
		RetailPrice: 0.0104, UnitOfMeasure: "1 Hour", ArmRegionName: "eastus",
		ServiceName: "Virtual Machines", ProductName: "Virtual Machines BS Series",
		SkuName: "B1s", MeterName: "B1s", Type: "Consumption", IsPrimaryMeterRegion: false,
	}
	b := a
	b.RetailPrice, b.IsPrimaryMeterRegion = 0.0105, true
	got := azurePricesByKey(t, []azureItem{a, b})
	want := []string{"Consumption 0.0104000000", "Consumption 0.0105000000"}
	if !slices.Equal(got["eastus|B1s"], want) {
		t.Errorf("got %v, want %v", got["eastus|B1s"], want)
	}
}

func TestAzurePriceSlot(t *testing.T) {
	a := db.Price{PurchaseOption: "Consumption", Unit: "1 GB", StartUsageAmount: "0"}
	b := a
	b.StartUsageAmount = "51200"
	if azurePriceSlot(a) == azurePriceSlot(b) {
		t.Error("different tiers share a slot")
	}
}
