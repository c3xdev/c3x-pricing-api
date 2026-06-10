package scraper

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/c3xdev/c3x-pricing-api/internal/config"
	"github.com/c3xdev/c3x-pricing-api/internal/db"
	"golang.org/x/sync/errgroup"
	"golang.org/x/time/rate"
)

const azureBaseURL = "https://prices.azure.com/api/retail/prices"

type AzureScraper struct {
	cfg            *config.Config
	client         *http.Client
	rateLimiter    *rate.Limiter
	failedServices int64
}

func NewAzureScraper(cfg *config.Config) *AzureScraper {
	return &AzureScraper{
		cfg:         cfg,
		client:      &http.Client{Timeout: 120 * time.Second},
		rateLimiter: rate.NewLimiter(rate.Limit(20), 5), // 20 req/s, burst 5
	}
}

func (s *AzureScraper) Name() string          { return "Azure" }
func (s *AzureScraper) FailedServices() int64 { return atomic.LoadInt64(&s.failedServices) }

// azureServices lists all Azure services to scrape.
var azureServices = []string{
	"Virtual Machines", "Azure App Service", "Functions",
	"Azure Kubernetes Service", "Container Instances", "Azure Databricks",
	"Storage", "Backup",
	"SQL Database", "SQL Managed Instance",
	"Azure Database for PostgreSQL", "Azure Database for MySQL",
	"Azure Database for MariaDB", "Azure Cosmos DB",
	"Azure Synapse Analytics", "Redis Cache",
	"Load Balancer", "VPN Gateway", "Virtual WAN", "Azure DNS",
	"Azure Firewall", "Application Gateway", "Azure Front Door Service",
	"Content Delivery Network", "Traffic Manager", "Virtual Network",
	"NAT Gateway", "Azure Bastion", "Azure DDOS Protection", "Network Watcher",
	"HDInsight", "Azure Data Factory v2", "Azure Cognitive Search",
	"Cognitive Services", "Power BI Embedded",
	"Azure Monitor", "Log Analytics", "Application Insights", "Automation",
	"Microsoft Defender for Cloud", "Advanced Threat Protection", "Key Vault",
	"API Management", "Service Bus", "Event Hubs", "Event Grid",
	"Logic Apps", "SignalR", "Notification Hubs",
	"Azure Active Directory for External Identities", "Microsoft Entra Domain Services",
	"Container Registry", "App Configuration", "IoT Hub",
}

// ScrapeWithHandler streams Azure products to the handler per-service instead of
// collecting all products in memory first. This means DB writes begin within
// seconds of starting the scrape, not after all services complete.
func (s *AzureScraper) ScrapeWithHandler(ctx context.Context, handler ProductHandler) error {
	concurrency := s.cfg.ConcurrencyForVendor("azure")
	if concurrency < 1 {
		concurrency = 1
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)
	for _, svc := range azureServices {
		svc := svc
		g.Go(func() error {
			slog.Info("scraping service", "vendor", "azure", "service", svc)
			products, err := s.scrapeService(gctx, svc)
			if err != nil {
				if gctx.Err() != nil {
					return gctx.Err()
				}
				atomic.AddInt64(&s.failedServices, 1)
				slog.Warn("scrape failed", "vendor", "azure", "service", svc, "error", err)
				return nil
			}
			slog.Info("scraped service", "vendor", "azure", "service", svc, "products", len(products))
			if len(products) > 0 {
				if err := handler(gctx, products); err != nil {
					return fmt.Errorf("upsert azure %s: %w", svc, err)
				}
			}
			return nil
		})
	}
	return g.Wait()
}

func (s *AzureScraper) Scrape(ctx context.Context) ([]db.Product, error) {
	var allProducts []db.Product
	var mu sync.Mutex
	handler := func(ctx context.Context, products []db.Product) error {
		mu.Lock()
		allProducts = append(allProducts, products...)
		mu.Unlock()
		return nil
	}
	if err := s.ScrapeWithHandler(ctx, handler); err != nil {
		return nil, err
	}
	return allProducts, nil
}

type azureResponse struct {
	Items        []azureItem `json:"Items"`
	NextPageLink string      `json:"NextPageLink"`
}

type azureItem struct {
	CurrencyCode         string  `json:"currencyCode"`
	RetailPrice          float64 `json:"retailPrice"`
	UnitOfMeasure        string  `json:"unitOfMeasure"`
	ArmRegionName        string  `json:"armRegionName"`
	Location             string  `json:"location"`
	ServiceName          string  `json:"serviceName"`
	ServiceFamily        string  `json:"serviceFamily"`
	ProductName          string  `json:"productName"`
	SkuName              string  `json:"skuName"`
	ArmSkuName           string  `json:"armSkuName"`
	MeterName            string  `json:"meterName"`
	Type                 string  `json:"type"`
	IsPrimaryMeterRegion bool    `json:"isPrimaryMeterRegion"`
	ReservationTerm      string  `json:"reservationTerm"`
	TierMinimumUnits     float64 `json:"tierMinimumUnits"`
}

func (s *AzureScraper) scrapeService(ctx context.Context, serviceName string) ([]db.Product, error) {
	// L8: Escape single quotes in service names to prevent OData injection
	escapedName := strings.ReplaceAll(serviceName, "'", "''")
	filter := fmt.Sprintf("serviceName eq '%s'", escapedName)
	pageURL := fmt.Sprintf("%s?$filter=%s", azureBaseURL, url.QueryEscape(filter))

	productMap := make(map[string]*db.Product)

	for pageURL != "" {
		azureResp, err := s.fetchPage(ctx, pageURL)
		if err != nil {
			// L9: Return the error instead of swallowing it so the caller can
			// decide whether to keep partial data.
			slog.Warn("pagination error", "vendor", "azure", "service", serviceName, "products_so_far", len(productMap), "error", err)
			return nil, fmt.Errorf("pagination error for %s after %d products: %w", serviceName, len(productMap), err)
		}

		for _, item := range azureResp.Items {
			// Only filter by isPrimaryMeterRegion for services that use actual regions.
			// Services using virtual regions (Zone 1, Global) often have isPrimaryMeterRegion=false
			// for their relevant entries. Some product types (like IP Addresses) also have
			// isPrimaryMeterRegion=false for region-specific items.
			if !item.IsPrimaryMeterRegion && !usesVirtualRegions(item.ServiceName) && !skipPrimaryMeterFilter(item) {
				continue
			}

			// Determine the regions to store this product under
			regions := azureProductRegions(item)

			// Normalize Azure VM product names by stripping version suffixes
			// (e.g., "Virtual Machines DSv3 Series v8" → "Virtual Machines DSv3 Series").
			// Azure recently added these suffixes but the pricing is identical and
			// CLI filters expect the original format.
			productName := normalizeAzureProductName(item.ProductName)

			for _, region := range regions {
				productKey := fmt.Sprintf("%s|%s|%s|%s", productName, item.SkuName, item.MeterName, region)
				sku := ProductHash("azure-sku", productName, item.SkuName, item.MeterName)

				if _, ok := productMap[productKey]; !ok {
					prodHash := ProductHash("azure", region, item.ServiceName, sku+region)
					productMap[productKey] = &db.Product{
						ProductHash:   prodHash,
						SKU:           sku,
						VendorName:    "azure",
						Region:        region,
						Service:       item.ServiceName,
						ProductFamily: item.ServiceFamily,
						Attributes:    azureAttributes(item.ServiceName, productName, item.SkuName, item.MeterName, item.ArmSkuName, item.ServiceFamily),
						Prices:        []db.Price{},
					}
				}

				purchaseOption := item.Type

				// Use tierMinimumUnits as startUsageAmount for tiered pricing
				startUsageAmount := ""
				if item.TierMinimumUnits > 0 {
					startUsageAmount = fmt.Sprintf("%g", item.TierMinimumUnits)
				} else {
					startUsageAmount = "0"
				}

				priceH := PriceHash(productMap[productKey].ProductHash, purchaseOption, item.UnitOfMeasure, startUsageAmount, item.MeterName, item.ReservationTerm, "", "")

				productMap[productKey].Prices = append(productMap[productKey].Prices, db.Price{
					PriceHash:        priceH,
					PurchaseOption:   purchaseOption,
					Unit:             item.UnitOfMeasure,
					USD:              fmt.Sprintf("%.10f", item.RetailPrice),
					StartUsageAmount: startUsageAmount,
					Description:      item.MeterName,
					TermLength:       item.ReservationTerm,
				})
			}
		}

		pageURL = azureResp.NextPageLink
	}

	// Deduplicate by product_hash and deduplicate prices within each product
	seen := make(map[string]bool)
	products := make([]db.Product, 0, len(productMap))
	for _, p := range productMap {
		if len(p.Prices) > 0 && !seen[p.ProductHash] {
			seen[p.ProductHash] = true
			// For virtual-region products (Zone 1, Global, etc.), aggressively deduplicate
			// prices to keep only one per tier. These accumulate duplicate prices from
			// multiple source regions that all map to the same zone.
			if isVirtualRegion(p.Region) {
				p.Prices = deduplicatePricesByTier(p.Prices)
			} else {
				p.Prices = deduplicatePrices(p.Prices)
			}
			products = append(products, *p)
		}
	}

	return products, nil
}

// deduplicatePrices removes exact duplicate prices (same purchaseOption + unit + startUsageAmount + USD + termLength)
func deduplicatePrices(prices []db.Price) []db.Price {
	seen := make(map[string]bool)
	result := make([]db.Price, 0, len(prices))
	for _, p := range prices {
		key := fmt.Sprintf("%s|%s|%s|%s|%s", p.PurchaseOption, p.Unit, p.StartUsageAmount, p.USD, p.TermLength)
		if !seen[key] {
			seen[key] = true
			result = append(result, p)
		}
	}
	return result
}

// deduplicatePricesByTier keeps only one price per (purchaseOption, startUsageAmount, termLength) tier.
// Used for virtual-region products where multiple source regions contribute the same tier at slightly different prices.
func deduplicatePricesByTier(prices []db.Price) []db.Price {
	seen := make(map[string]int) // key -> index in result
	result := make([]db.Price, 0, len(prices))
	for _, p := range prices {
		key := fmt.Sprintf("%s|%s|%s", p.PurchaseOption, p.StartUsageAmount, p.TermLength)
		if idx, exists := seen[key]; exists {
			// Keep the lower USD price for deterministic results (numeric comparison).
			pVal, _ := strconv.ParseFloat(p.USD, 64)
			existingVal, _ := strconv.ParseFloat(result[idx].USD, 64)
			if pVal < existingVal {
				result[idx] = p
			}
		} else {
			seen[key] = len(result)
			result = append(result, p)
		}
	}
	return result
}

// isVirtualRegion returns true for Azure region values that are virtual/zone-based
func isVirtualRegion(region string) bool {
	switch {
	case strings.HasPrefix(region, "Zone"):
		return true
	case region == "Global":
		return true
	case strings.HasPrefix(region, "US Gov"):
		return true
	case strings.HasPrefix(region, "DE "):
		return true
	case strings.Contains(region, "China"):
		return true
	default:
		return false
	}
}

// skipPrimaryMeterFilter returns true for Azure items that should be included even
// when isPrimaryMeterRegion is false. Some product types like IP Addresses and
// managed disk operations have isPrimaryMeterRegion=false for region-specific entries.
// skipPrimaryMeterFilter returns true for Azure items that should be included even
// when isPrimaryMeterRegion is false. Many Azure product types set this flag to
// false for region-specific entries that are still valid pricing data.
func skipPrimaryMeterFilter(item azureItem) bool {
	switch item.ProductName {
	case "IP Addresses":
		return true
	}
	switch {
	// Cosmos DB Standard Provisioned RU has isPrimaryMeterRegion=false
	// across every region (only the Free Tier entry is primary).
	// HasPrefix (not exact match) so the "Azure Cosmos DB autoscale" /
	// "Azure Cosmos DB serverless" product-name variants surface too.
	case strings.HasPrefix(item.ProductName, "Azure Cosmos DB"):
		return true
	case strings.Contains(item.MeterName, "Disk Operations"):
		return true
	case strings.HasSuffix(item.ProductName, "Managed Disks") && strings.Contains(item.MeterName, "Disk Transactions"):
		return true
	// SQL Database DTU-based products (Standard, Basic, Premium tiers) have
	// isPrimaryMeterRegion=false for all regional entries.
	case strings.HasPrefix(item.ProductName, "SQL Database Single") || strings.HasPrefix(item.ProductName, "SQL Database Basic") || strings.HasPrefix(item.ProductName, "SQL Database Premium"):
		return true
	// Virtual Machines: the Consumption pricing for many VM types has
	// isPrimaryMeterRegion=false while only the Reservation entry for the
	// newer "v8" product line is marked as primary.
	case strings.HasPrefix(item.ProductName, "Virtual Machines"):
		return true
	// Container Registry Standard / Basic tier rows are flagged
	// non-primary across regions; only Premium passes the default
	// filter. Bypass so all three tiers surface.
	case strings.HasPrefix(item.ProductName, "Container Registry"):
		return true
	// Logic Apps Consumption Actions / Standard plan rows are
	// non-primary in most regions.
	case strings.HasPrefix(item.ProductName, "Logic Apps"):
		return true
	// Key Vault Operations meter is non-primary across regions; only
	// HSM Pool Standard B1 is primary, missing the standard Operations
	// per-10k charge entirely.
	case strings.HasPrefix(item.ProductName, "Key Vault"):
		return true
	// Event Hubs Standard / Premium throughput-unit hour rows are
	// non-primary in most regions.
	case strings.HasPrefix(item.ProductName, "Event Hubs"):
		return true
	}
	return false
}

// usesVirtualRegions returns true for Azure services that use virtual region names
// (Zone 1, Global, etc.) instead of actual ARM region names.
func usesVirtualRegions(serviceName string) bool {
	switch serviceName {
	case "Azure DNS", "VPN Gateway", "Load Balancer":
		return true
	default:
		return false
	}
}

// azureProductRegions returns the regions to index a product under.
// The CLI queries some services with virtual regions (e.g., "Global", "Zone 1")
// instead of actual Azure regions, so we store products under both.
func azureProductRegions(item azureItem) []string {
	regions := []string{}

	// Always store under the actual ARM region if available
	if item.ArmRegionName != "" {
		regions = append(regions, item.ArmRegionName)
	}

	// Services that use virtual regions in the CLI
	switch item.ServiceName {
	case "Load Balancer":
		// CLI queries Load Balancer with convertRegion(): "Global", "US Gov", "China"
		regions = append(regions, azureConvertRegion(item.ArmRegionName))
	case "Azure DNS":
		// CLI queries DNS with dnsZoneRegion(): "Zone 1", "US Gov Zone 1", etc.
		regions = append(regions, azureDNSZoneRegion(item.ArmRegionName))
	case "VPN Gateway":
		// Data transfer uses zone regions
		regions = append(regions, azureConvertRegion(item.ArmRegionName))
		regions = append(regions, azureDNSZoneRegion(item.ArmRegionName))
	}

	// If no region at all, use Global
	if len(regions) == 0 {
		regions = []string{"Global"}
	}

	return regions
}

// azureVMVersionSuffix matches trailing version suffixes like " v8", " v7" in Azure VM product names.
var azureVMVersionSuffix = regexp.MustCompile(` v\d+$`)

// normalizeAzureProductName strips version suffixes from Azure product names.
// Azure recently started appending version numbers to VM series product names
// (e.g., "Virtual Machines DSv3 Series v8") but the pricing is identical to the
// base name and CLI filters expect the original format without the suffix.
func normalizeAzureProductName(name string) string {
	if strings.HasPrefix(name, "Virtual Machines") {
		return azureVMVersionSuffix.ReplaceAllString(name, "")
	}
	return name
}

// azureConvertRegion matches the CLI's convertRegion() function
func azureConvertRegion(region string) string {
	lower := strings.ToLower(region)
	if strings.Contains(lower, "usgov") {
		return "US Gov"
	} else if strings.Contains(lower, "china") {
		return "China"
	}
	return "Global"
}

// azureDNSZoneRegion matches the CLI's dnsZoneRegion() function
func azureDNSZoneRegion(region string) string {
	lower := strings.ToLower(region)
	switch {
	case strings.HasPrefix(lower, "usgov"):
		return "US Gov Zone 1"
	case strings.HasPrefix(lower, "germany"):
		return "DE Zone 1"
	case strings.HasPrefix(lower, "china"):
		return "Zone 1 (China)"
	default:
		return "Zone 1"
	}
}

// contextSleep waits for the given duration or until the context is cancelled.
func contextSleep(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// retryableError wraps an error with a flag indicating whether the operation should be retried.
type retryableError struct {
	err       error
	retryable bool
}

// retryWithBackoff executes fn up to maxRetries times with exponential backoff.
// fn returns (result, retryableError). If the error is retryable and attempts
// remain, it logs a warning, sleeps, and retries.
func retryWithBackoff[T any](ctx context.Context, maxRetries int, baseBackoff time.Duration, fn func(attempt int) (T, retryableError)) (T, error) {
	var zero T
	for attempt := 0; attempt < maxRetries; attempt++ {
		result, re := fn(attempt)
		if re.err == nil {
			return result, nil
		}
		if !re.retryable || attempt >= maxRetries-1 {
			return zero, re.err
		}
		backoff := time.Duration(1<<uint(attempt)) * baseBackoff
		slog.Warn("retrying after error", "attempt", attempt+1, "backoff", backoff.String(), "error", re.err)
		if err := contextSleep(ctx, backoff); err != nil {
			return zero, err
		}
	}
	return zero, fmt.Errorf("max retries exceeded")
}

func (s *AzureScraper) fetchPage(ctx context.Context, pageURL string) (*azureResponse, error) {
	result, err := retryWithBackoff(ctx, 5, 10*time.Second, func(attempt int) (*azureResponse, retryableError) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
		if err != nil {
			return nil, retryableError{fmt.Errorf("create request: %w", err), false}
		}

		resp, err := s.client.Do(req)
		if err != nil {
			return nil, retryableError{fmt.Errorf("fetch: %w", err), true}
		}

		if resp.StatusCode != http.StatusOK {
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			return nil, retryableError{fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(errBody)), true}
		}

		body, err := io.ReadAll(io.LimitReader(resp.Body, 100*1024*1024))
		_ = resp.Body.Close()
		if err != nil {
			return nil, retryableError{fmt.Errorf("read body: %w", err), false}
		}

		if len(body) > 0 && body[0] != '{' {
			return nil, retryableError{fmt.Errorf("non-JSON response"), true}
		}

		var azureResp azureResponse
		if err := json.Unmarshal(body, &azureResp); err != nil {
			return nil, retryableError{fmt.Errorf("decode: %w", err), true}
		}

		return &azureResp, retryableError{}
	})
	if err != nil {
		return nil, err
	}

	// Token-bucket rate limiter (20 req/s, burst 5). Pages proceed at max throughput
	// and only pause when the bucket empties, eliminating idle time between fast pages.
	if err := s.rateLimiter.Wait(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

// azureAttributes builds the product attribute map, deriving
// normalised discriminators the raw feed lacks.
//
// Virtual Machines: Linux and Windows rates share skuName/meterName
// and differ only by a " Windows" productName suffix — without an
// explicit `os` attribute, a consumer filtering on skuName alone
// gets both rows and (worse) any max-price picker quotes the
// Windows rate for a Linux VM.
func azureAttributes(serviceName, productName, skuName, meterName, armSkuName, serviceFamily string) map[string]string {
	attrs := map[string]string{
		"productName":   productName,
		"skuName":       skuName,
		"meterName":     meterName,
		"armSkuName":    armSkuName,
		"serviceFamily": serviceFamily,
	}
	if serviceName == "Virtual Machines" {
		if strings.HasSuffix(productName, " Windows") {
			attrs["os"] = "Windows"
		} else {
			attrs["os"] = "Linux"
		}
	}
	return attrs
}
