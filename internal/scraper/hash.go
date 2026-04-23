package scraper

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// ProductHash generates a deterministic hash for a pricing product.
// Fields are NUL-delimited to prevent collisions when field values contain
// the separator character (e.g., descriptions with colons).
func ProductHash(vendorName, region, service, sku string) string {
	data := strings.Join([]string{vendorName, region, service, sku}, "\x00")
	return fmt.Sprintf("%x", sha256.Sum256([]byte(data)))[:32]
}

// PriceHash generates a deterministic hash for a pricing tier within a product.
func PriceHash(productHash string, purchaseOption, unit, startUsageAmount, description, termLength, termPurchaseOption, termOfferingClass string) string {
	data := strings.Join([]string{productHash, purchaseOption, unit, startUsageAmount, description, termLength, termPurchaseOption, termOfferingClass}, "\x00")
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(data)))[:32]
	return fmt.Sprintf("%s-%s", productHash, hash)
}
