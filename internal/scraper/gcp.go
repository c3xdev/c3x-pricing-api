package scraper

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/c3xdev/c3x-pricing-api/internal/config"
	"github.com/c3xdev/c3x-pricing-api/internal/db"
	"golang.org/x/sync/errgroup"
)

const gcpBaseURL = "https://cloudbilling.googleapis.com/v1"

// Key GCP service IDs
var gcpServices = map[string]string{
	"Compute Engine":    "6F81-5844-456A",
	"Cloud SQL":         "9662-B51E-5089",
	"Cloud Storage":     "95FF-2EF5-5EA1",
	"Cloud Run Functions": "29E7-DA93-CA13",
	"Cloud Run":         "152E-C115-5142",
	"Cloud DNS":         "FA26-5236-B8B5",
	"Networking":        "E505-1604-58F8",
	"Cloud Pub/Sub":     "A1E8-BE35-7EBC",
	"Kubernetes Engine": "CCD8-9BF1-090E",
}

type GCPScraper struct {
	cfg            *config.Config
	client         *http.Client
	failedServices int64
}

func NewGCPScraper(cfg *config.Config) *GCPScraper {
	return &GCPScraper{cfg: cfg, client: &http.Client{Timeout: 120 * time.Second}}
}

func (s *GCPScraper) Name() string              { return "GCP" }
func (s *GCPScraper) FailedServices() int64      { return atomic.LoadInt64(&s.failedServices) }

func (s *GCPScraper) ScrapeWithHandler(ctx context.Context, handler ProductHandler) error {
	if s.cfg.GCPAPIKey == "" {
		return fmt.Errorf("GCP_API_KEY is required for GCP scraping")
	}

	concurrency := s.cfg.ConcurrencyForVendor("gcp")
	if concurrency < 1 {
		concurrency = 1
	}

	// Collect Compute Engine products during streaming for machine type synthesis.
	var computeProducts []db.Product
	var mu sync.Mutex

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)
	for serviceName, serviceID := range gcpServices {
		serviceName, serviceID := serviceName, serviceID
		g.Go(func() error {
			slog.Info("scraping service", "vendor", "gcp", "service", serviceName, "service_id", serviceID)
			products, err := s.scrapeService(gctx, serviceName, serviceID)
			if err != nil {
				if gctx.Err() != nil {
					return gctx.Err()
				}
				atomic.AddInt64(&s.failedServices, 1)
				slog.Warn("scrape failed", "vendor", "gcp", "service", serviceName, "error", err)
				return nil
			}
			slog.Info("scraped service", "vendor", "gcp", "service", serviceName, "products", len(products))
			if len(products) > 0 {
				if err := handler(gctx, products); err != nil {
					return fmt.Errorf("upsert gcp %s: %w", serviceName, err)
				}
			}
			// Keep a copy of Compute Engine products for machine type synthesis.
			if serviceName == "Compute Engine" {
				mu.Lock()
				computeProducts = append(computeProducts, products...)
				mu.Unlock()
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}

	// Synthesize predefined machine type products from CPU+RAM SKUs.
	// GCP prices CPU and RAM separately, but the CLI queries by machineType (e.g., n1-standard-2).
	slog.Info("synthesizing predefined machine type products", "vendor", "gcp")
	machineTypeProducts := s.synthesizeMachineTypes(computeProducts)
	slog.Info("synthesized machine type products", "vendor", "gcp", "products", len(machineTypeProducts))
	if len(machineTypeProducts) > 0 {
		if err := handler(ctx, machineTypeProducts); err != nil {
			return fmt.Errorf("upsert gcp machine types: %w", err)
		}
	}

	return nil
}

func (s *GCPScraper) Scrape(ctx context.Context) ([]db.Product, error) {
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

type gcpSKUListResponse struct {
	SKUs          []gcpSKU `json:"skus"`
	NextPageToken string   `json:"nextPageToken"`
}

type gcpSKU struct {
	SKUID          string      `json:"skuId"`
	Name           string      `json:"name"`
	Description    string      `json:"description"`
	Category       gcpCategory `json:"category"`
	ServiceRegions []string    `json:"serviceRegions"`
	PricingInfo    []gcpPrice  `json:"pricingInfo"`
}

type gcpCategory struct {
	ServiceDisplayName string `json:"serviceDisplayName"`
	ResourceFamily     string `json:"resourceFamily"`
	ResourceGroup      string `json:"resourceGroup"`
	UsageType          string `json:"usageType"`
}

type gcpPrice struct {
	PricingExpression gcpPricingExpression `json:"pricingExpression"`
}

type gcpPricingExpression struct {
	UsageUnit string          `json:"usageUnit"`
	TieredRates []gcpTieredRate `json:"tieredRates"`
}

type gcpTieredRate struct {
	StartUsageAmount float64      `json:"startUsageAmount"`
	UnitPrice        gcpUnitPrice `json:"unitPrice"`
}

type gcpUnitPrice struct {
	CurrencyCode string `json:"currencyCode"`
	Units        string `json:"units"`
	Nanos        int64  `json:"nanos"`
}

func (s *GCPScraper) scrapeService(ctx context.Context, serviceName, serviceID string) ([]db.Product, error) {
	var allProducts []db.Product
	pageToken := ""

	for {
		skuResp, err := s.fetchGCPPage(ctx, serviceID, pageToken)
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", serviceName, err)
		}

		for _, sku := range skuResp.SKUs {
			products := s.transformSKU(sku)
			allProducts = append(allProducts, products...)
		}

		if skuResp.NextPageToken == "" {
			break
		}
		pageToken = skuResp.NextPageToken
	}

	return allProducts, nil
}

func (s *GCPScraper) transformSKU(sku gcpSKU) []db.Product {
	var products []db.Product

	if len(sku.PricingInfo) == 0 {
		return nil
	}

	pricing := sku.PricingInfo[0]
	purchaseOption := sku.Category.UsageType // OnDemand, Preemptible, Commit1Yr, Commit3Yr

	for _, region := range sku.ServiceRegions {
		// Remap GCP service names to match CLI expectations. The CLI queries
		// networking products (forwarding rules, NAT) under "Compute Engine"
		// but the GCP Billing API returns them under "Networking".
		serviceName := normalizeGCPServiceName(sku.Category.ServiceDisplayName)
		prodHash := ProductHash("gcp", region, serviceName, sku.SKUID)

		product := db.Product{
			ProductHash:   prodHash,
			SKU:           sku.SKUID,
			VendorName:    "gcp",
			Region:        region,
			Service:       serviceName,
			ProductFamily: sku.Category.ResourceFamily,
			Attributes: map[string]string{
				"description":   sku.Description,
				"resourceGroup": sku.Category.ResourceGroup,
			},
			Prices: []db.Price{},
		}

		tiers := pricing.PricingExpression.TieredRates
		for i, tier := range tiers {
			// L5: Parse Units as int64 for precision, then combine with Nanos
			var unitVal int64
			if tier.UnitPrice.Units != "" && tier.UnitPrice.Units != "0" {
				if v, err := strconv.ParseInt(tier.UnitPrice.Units, 10, 64); err == nil {
					unitVal = v
				}
			}
			usd := float64(unitVal) + float64(tier.UnitPrice.Nanos)/1e9

			// L6: Use strconv.FormatFloat for clean string representation
			startUsage := strconv.FormatFloat(tier.StartUsageAmount, 'f', -1, 64)

			// Compute endUsageAmount from the next tier's startUsageAmount
			endUsage := ""
			if i+1 < len(tiers) {
				endUsage = strconv.FormatFloat(tiers[i+1].StartUsageAmount, 'f', -1, 64)
			}

			priceH := PriceHash(prodHash, purchaseOption, pricing.PricingExpression.UsageUnit, startUsage, sku.Description, "", "", "")

			product.Prices = append(product.Prices, db.Price{
				PriceHash:        priceH,
				PurchaseOption:   purchaseOption,
				Unit:             pricing.PricingExpression.UsageUnit,
				USD:              fmt.Sprintf("%.10f", usd),
				StartUsageAmount: startUsage,
				EndUsageAmount:   endUsage,
				Description:      sku.Description,
			})
		}

		if len(product.Prices) > 0 {
			products = append(products, product)
		}
	}

	return products
}

// GCP predefined machine type specs: family → map[machineType → {cpus, ramGB}]
var gcpMachineTypes = map[string]map[string][2]float64{
	"n1": {
		"n1-standard-1": {1, 3.75}, "n1-standard-2": {2, 7.5}, "n1-standard-4": {4, 15},
		"n1-standard-8": {8, 30}, "n1-standard-16": {16, 60}, "n1-standard-32": {32, 120},
		"n1-standard-64": {64, 240}, "n1-standard-96": {96, 360},
		"n1-highmem-2": {2, 13}, "n1-highmem-4": {4, 26}, "n1-highmem-8": {8, 52},
		"n1-highmem-16": {16, 104}, "n1-highmem-32": {32, 208}, "n1-highmem-64": {64, 416}, "n1-highmem-96": {96, 624},
		"n1-highcpu-2": {2, 1.8}, "n1-highcpu-4": {4, 3.6}, "n1-highcpu-8": {8, 7.2},
		"n1-highcpu-16": {16, 14.4}, "n1-highcpu-32": {32, 28.8}, "n1-highcpu-64": {64, 57.6}, "n1-highcpu-96": {96, 86.4},
	},
	"n2": {
		"n2-standard-2": {2, 8}, "n2-standard-4": {4, 16}, "n2-standard-8": {8, 32},
		"n2-standard-16": {16, 64}, "n2-standard-32": {32, 128}, "n2-standard-48": {48, 192},
		"n2-standard-64": {64, 256}, "n2-standard-80": {80, 320}, "n2-standard-96": {96, 384}, "n2-standard-128": {128, 512},
		"n2-highmem-2": {2, 16}, "n2-highmem-4": {4, 32}, "n2-highmem-8": {8, 64},
		"n2-highmem-16": {16, 128}, "n2-highmem-32": {32, 256}, "n2-highmem-48": {48, 384},
		"n2-highmem-64": {64, 512}, "n2-highmem-80": {80, 640}, "n2-highmem-96": {96, 768}, "n2-highmem-128": {128, 864},
		"n2-highcpu-2": {2, 2}, "n2-highcpu-4": {4, 4}, "n2-highcpu-8": {8, 8},
		"n2-highcpu-16": {16, 16}, "n2-highcpu-32": {32, 32}, "n2-highcpu-48": {48, 48},
		"n2-highcpu-64": {64, 64}, "n2-highcpu-80": {80, 80}, "n2-highcpu-96": {96, 96},
	},
	"e2": {
		"e2-micro": {0.25, 1}, "e2-small": {0.5, 2}, "e2-medium": {1, 4},
		"e2-standard-2": {2, 8}, "e2-standard-4": {4, 16}, "e2-standard-8": {8, 32},
		"e2-standard-16": {16, 64}, "e2-standard-32": {32, 128},
		"e2-highmem-2": {2, 16}, "e2-highmem-4": {4, 32}, "e2-highmem-8": {8, 64}, "e2-highmem-16": {16, 128},
		"e2-highcpu-2": {2, 2}, "e2-highcpu-4": {4, 4}, "e2-highcpu-8": {8, 8},
		"e2-highcpu-16": {16, 16}, "e2-highcpu-32": {32, 32},
	},
	"n2d": {
		"n2d-standard-2": {2, 8}, "n2d-standard-4": {4, 16}, "n2d-standard-8": {8, 32},
		"n2d-standard-16": {16, 64}, "n2d-standard-32": {32, 128}, "n2d-standard-48": {48, 192},
		"n2d-standard-64": {64, 256}, "n2d-standard-80": {80, 320}, "n2d-standard-96": {96, 384},
		"n2d-standard-128": {128, 512}, "n2d-standard-224": {224, 896},
		"n2d-highmem-2": {2, 16}, "n2d-highmem-4": {4, 32}, "n2d-highmem-8": {8, 64},
		"n2d-highmem-16": {16, 128}, "n2d-highmem-32": {32, 256}, "n2d-highmem-48": {48, 384},
		"n2d-highmem-64": {64, 512}, "n2d-highmem-80": {80, 640}, "n2d-highmem-96": {96, 768},
		"n2d-highcpu-2": {2, 2}, "n2d-highcpu-4": {4, 4}, "n2d-highcpu-8": {8, 8},
		"n2d-highcpu-16": {16, 16}, "n2d-highcpu-32": {32, 32}, "n2d-highcpu-48": {48, 48},
		"n2d-highcpu-64": {64, 64}, "n2d-highcpu-80": {80, 80}, "n2d-highcpu-96": {96, 96},
		"n2d-highcpu-128": {128, 128}, "n2d-highcpu-224": {224, 224},
	},
	"c2": {
		"c2-standard-4": {4, 16}, "c2-standard-8": {8, 32}, "c2-standard-16": {16, 64},
		"c2-standard-30": {30, 120}, "c2-standard-60": {60, 240},
	},
	"c2d": {
		"c2d-standard-2": {2, 8}, "c2d-standard-4": {4, 16}, "c2d-standard-8": {8, 32},
		"c2d-standard-16": {16, 64}, "c2d-standard-32": {32, 128}, "c2d-standard-56": {56, 224},
		"c2d-standard-112": {112, 448},
		"c2d-highmem-2": {2, 16}, "c2d-highmem-4": {4, 32}, "c2d-highmem-8": {8, 64},
		"c2d-highmem-16": {16, 128}, "c2d-highmem-32": {32, 256}, "c2d-highmem-56": {56, 448},
		"c2d-highmem-112": {112, 896},
		"c2d-highcpu-2": {2, 2}, "c2d-highcpu-4": {4, 4}, "c2d-highcpu-8": {8, 8},
		"c2d-highcpu-16": {16, 16}, "c2d-highcpu-32": {32, 32}, "c2d-highcpu-56": {56, 56},
		"c2d-highcpu-112": {112, 112},
	},
	"t2d": {
		"t2d-standard-1": {1, 4}, "t2d-standard-2": {2, 8}, "t2d-standard-4": {4, 16},
		"t2d-standard-8": {8, 32}, "t2d-standard-16": {16, 64}, "t2d-standard-32": {32, 128},
		"t2d-standard-48": {48, 192}, "t2d-standard-60": {60, 240},
	},
	"t2a": {
		"t2a-standard-1": {1, 4}, "t2a-standard-2": {2, 8}, "t2a-standard-4": {4, 16},
		"t2a-standard-8": {8, 32}, "t2a-standard-16": {16, 64}, "t2a-standard-32": {32, 128},
		"t2a-standard-48": {48, 192},
	},
}

// CPU/RAM description patterns for each family.
// L10: These are substring-based matches. If multiple SKUs match (e.g. with and
// without SUD), the map-overwrite behavior means last match wins, which in practice
// gives the non-SUD price (correct for on-demand estimation).
var gcpFamilyCPUPatterns = map[string]string{
	"n1":  "N1 Predefined Instance Core",
	"n2":  "N2 Instance Core",
	"n2d": "N2D AMD Instance Core",
	"e2":  "E2 Instance Core",
	"c2":  "Compute optimized Core",
	"c2d": "C2D AMD Instance Core",
	"t2d": "T2D AMD Instance Core",
	"t2a": "T2A Arm Instance Core",
}
var gcpFamilyRAMPatterns = map[string]string{
	"n1":  "N1 Predefined Instance Ram",
	"n2":  "N2 Instance Ram",
	"n2d": "N2D AMD Instance Ram",
	"e2":  "E2 Instance Ram",
	"c2":  "Compute optimized Ram",
	"c2d": "C2D AMD Instance Ram",
	"t2d": "T2D AMD Instance Ram",
	"t2a": "T2A Arm Instance Ram",
}

// fetchGCPPage fetches a single page of SKUs from the GCP Cloud Billing API with
// retry logic and proper error handling.
func (s *GCPScraper) fetchGCPPage(ctx context.Context, serviceID, pageToken string) (*gcpSKUListResponse, error) {
	maxRetries := 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		reqURL := fmt.Sprintf("%s/services/%s/skus?pageSize=5000", gcpBaseURL, serviceID)
		if pageToken != "" {
			reqURL += "&pageToken=" + url.QueryEscape(pageToken)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		req.Header.Set("X-Goog-Api-Key", s.cfg.GCPAPIKey)

		resp, err := s.client.Do(req)
		if err != nil {
			if attempt < maxRetries-1 {
				slog.Warn("GCP network error, retrying", "service_id", serviceID, "attempt", attempt+1, "error", err)
				select {
				case <-time.After(time.Duration(1<<uint(attempt)) * 2 * time.Second):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				continue
			}
			return nil, fmt.Errorf("fetch after %d retries: %w", maxRetries, err)
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if attempt < maxRetries-1 && (resp.StatusCode == 429 || resp.StatusCode >= 500) {
				slog.Warn("GCP HTTP error, retrying", "service_id", serviceID, "status", resp.StatusCode, "attempt", attempt+1)
				select {
				case <-time.After(time.Duration(1<<uint(attempt)) * 2 * time.Second):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				continue
			}
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
		}

		var skuResp gcpSKUListResponse
		if err := json.NewDecoder(io.LimitReader(resp.Body, 100*1024*1024)).Decode(&skuResp); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("decode response: %w", err)
		}
		resp.Body.Close()
		return &skuResp, nil
	}
	return nil, fmt.Errorf("max retries exceeded")
}

// normalizeGCPServiceName remaps GCP Billing API service names to match what the
// CLI expects. The CLI queries all networking products (forwarding rules, NAT,
// load balancing) under "Compute Engine", but the GCP Billing API returns them
// under "Networking".
func normalizeGCPServiceName(name string) string {
	if name == "Networking" {
		return "Compute Engine"
	}
	return name
}

type priceKey struct {
	region, purchaseOption string
}

// indexCPURAMPrices scans Compute Engine products and builds per-family maps of
// CPU and RAM unit prices indexed by (region, purchaseOption).
func indexCPURAMPrices(products []db.Product) (
	cpuPrices map[string]map[priceKey]float64,
	ramPrices map[string]map[priceKey]float64,
) {
	cpuPrices = map[string]map[priceKey]float64{}
	ramPrices = map[string]map[priceKey]float64{}

	for _, p := range products {
		if p.Service != "Compute Engine" {
			continue
		}
		desc := p.Attributes["description"]
		if strings.Contains(desc, "SUD") || strings.Contains(desc, "Commitment") {
			continue
		}

		for family, pattern := range gcpFamilyCPUPatterns {
			if strings.Contains(desc, pattern) {
				if cpuPrices[family] == nil {
					cpuPrices[family] = map[priceKey]float64{}
				}
				for _, price := range p.Prices {
					if price.USD != "0.0000000000" {
						if v, err := strconv.ParseFloat(price.USD, 64); err == nil {
							k := priceKey{p.Region, price.PurchaseOption}
							if existing, ok := cpuPrices[family][k]; !ok || v < existing {
								cpuPrices[family][k] = v
							}
						}
					}
				}
			}
		}
		for family, pattern := range gcpFamilyRAMPatterns {
			if strings.Contains(desc, pattern) {
				if ramPrices[family] == nil {
					ramPrices[family] = map[priceKey]float64{}
				}
				for _, price := range p.Prices {
					if price.USD != "0.0000000000" {
						if v, err := strconv.ParseFloat(price.USD, 64); err == nil {
							k := priceKey{p.Region, price.PurchaseOption}
							if existing, ok := ramPrices[family][k]; !ok || v < existing {
								ramPrices[family][k] = v
							}
						}
					}
				}
			}
		}
	}
	return
}

func (s *GCPScraper) synthesizeMachineTypes(products []db.Product) []db.Product {
	cpuPrices, ramPrices := indexCPURAMPrices(products)

	var result []db.Product
	for family, machineTypes := range gcpMachineTypes {
		cpuMap := cpuPrices[family]
		ramMap := ramPrices[family]
		if cpuMap == nil || ramMap == nil {
			continue
		}

		for machineType, spec := range machineTypes {
			cpus, ramGB := spec[0], spec[1]

			seen := map[string]bool{}
			for key := range cpuMap {
				if _, ok := ramMap[key]; !ok {
					continue
				}
				if seen[key.region] {
					continue
				}
				seen[key.region] = true

				prodHash := ProductHash("gcp", key.region, "Compute Engine", "mt-"+machineType)
				product := db.Product{
					ProductHash:   prodHash,
					SKU:           "mt-" + machineType,
					VendorName:    "gcp",
					Region:        key.region,
					Service:       "Compute Engine",
					ProductFamily: "Compute Instance",
					Attributes: map[string]string{
						"machineType": machineType,
						"description": fmt.Sprintf("%s predefined instance", machineType),
					},
				}

				for pKey, cpuPrice := range cpuMap {
					if pKey.region != key.region {
						continue
					}
					ramPrice, ok := ramMap[pKey]
					if !ok {
						continue
					}
					totalPrice := cpuPrice*cpus + ramPrice*ramGB
					priceH := PriceHash(prodHash, pKey.purchaseOption, "h", "0", machineType, "", "", "")
					product.Prices = append(product.Prices, db.Price{
						PriceHash:      priceH,
						PurchaseOption: pKey.purchaseOption,
						Unit:           "h",
						USD:            fmt.Sprintf("%.10f", totalPrice),
						Description:    fmt.Sprintf("%s in %s", machineType, key.region),
					})
				}

				if len(product.Prices) > 0 {
					result = append(result, product)
				}
			}
		}
	}
	return result
}
