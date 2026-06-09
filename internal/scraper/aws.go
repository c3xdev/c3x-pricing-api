package scraper

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/c3xdev/c3x-pricing-api/internal/config"
	"github.com/c3xdev/c3x-pricing-api/internal/db"
	"golang.org/x/sync/errgroup"
)

const awsBaseURL = "https://pricing.us-east-1.amazonaws.com"

// All AWS services needed by the C3X CLI test suite.
//
// Service codes are AWS's own offer-index keys. Adding one here makes
// the scraper pull `pricing.us-east-1.amazonaws.com/offers/v1.0/aws/
// <code>/current/index.json`. New services land as additional rows in
// the products table without schema changes — only the scraper
// awareness gates ingestion.
var awsServices = []string{
	// Compute
	"AmazonEC2",
	"AWSLambda",
	"AmazonECS",
	"AmazonEKS",
	"AmazonLightsail",
	"AmazonElasticBeanstalk",
	"CodeBuild",
	"AWSAppRunner",     // App Runner (vCPU/memory hours, build minutes)
	"ElasticMapReduce", // EMR service charge per-node-hour
	// Database
	"AmazonRDS",
	"AmazonDynamoDB",
	"AmazonElastiCache",
	"AmazonRedshift",
	"AmazonDocDB",
	"AmazonNeptune",
	"AmazonES",       // OpenSearch/Elasticsearch
	"AmazonDAX",      // DynamoDB Accelerator
	"AmazonMemoryDB", // MemoryDB for Redis
	// Storage
	"AmazonS3",
	"AmazonEFS",
	"AmazonFSx",
	"AmazonECR",
	"AmazonGlacier",
	"AmazonS3GlacierDeepArchive",
	// AmazonStorageGateway: AWS does not publish a bulk-pricing
	// offer file (returns HTTP 404 as of 2026-06-09). Kept in
	// c3x-go as a STATIC entry with hand-curated $0.01/GB-written
	// rate — documented in docs/upstream-gaps.md.
	// Networking
	"AWSELB",
	"AmazonVPC",
	"AmazonRoute53",
	"AmazonCloudFront",
	"AmazonApiGateway",
	"AWSGlobalAccelerator",
	"AWSDirectConnect",
	"AWSNetworkFirewall",
	"AWSDataTransfer",
	"AWSAppSync", // AppSync GraphQL (queries, real-time updates)
	// Messaging & Streaming
	"AmazonSNS",
	"AWSQueueService",
	"AmazonMQ",
	"AmazonKinesis",
	"AmazonKinesisFirehose",
	"AmazonKinesisAnalytics",
	"AmazonMSK",
	// Monitoring & Management
	"AmazonCloudWatch",
	"AWSConfig",
	"AWSCloudTrail",
	"AWSEvents",
	"AWSSystemsManager",
	"AWSBackup",
	"AWSXRay", // X-Ray (traces recorded/scanned/insights)
	// Security & Identity
	"awskms",
	"CloudHSM",
	"awswaf",
	"AWSCertificateManager",
	"AmazonCognito", // Cognito User Pools (MAUs, M2M tokens)
	// Developer Tools & Frontend
	"AWSAmplify", // Amplify Hosting (build minutes, storage, transfer)
	// Integration & Other
	"AWSGlue",
	"AmazonAthena", // per-TB-scanned query pricing (normaliser backfills productFamily)
	"AWSSecretsManager",
	"AWSTransfer",
	"AmazonStates", // Step Functions
	"AWSDatabaseMigrationSvc",
	"AWSDirectoryService",
	"AmazonGrafana",
	"AmazonMWAA",
	"AWSCloudFormation",
}

type AWSScraper struct {
	cfg            *config.Config
	client         *http.Client
	cnyUSDRate     float64
	failedServices int64
}

// defaultCNYUSDRate is the fallback CNY/USD exchange rate when not configured.
const defaultCNYUSDRate = 6.2069

func NewAWSScraper(cfg *config.Config) *AWSScraper {
	rate := defaultCNYUSDRate
	if cfg.CNYUSDRate > 0 {
		rate = cfg.CNYUSDRate
	}
	// No global client timeout. EC2 pricing files are large and take minutes to
	// download. Transport-level timeouts guard against hung sockets without
	// aborting legitimate long-lived downloads.
	transport := &http.Transport{
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConnsPerHost:   16,
		MaxConnsPerHost:       16,
		// Disable auto-gzip so we control decompression ourselves.
		// We explicitly set Accept-Encoding: gzip on requests and wrap
		// the response body in a gzip.Reader for ~6-10x bandwidth reduction.
		DisableCompression: true,
	}
	return &AWSScraper{cfg: cfg, client: &http.Client{Transport: transport}, cnyUSDRate: rate}
}

func (s *AWSScraper) Name() string          { return "AWS" }
func (s *AWSScraper) FailedServices() int64 { return atomic.LoadInt64(&s.failedServices) }

// ScrapeWithHandler streams products to the handler in batches, avoiding OOM for large services.
func (s *AWSScraper) ScrapeWithHandler(ctx context.Context, handler ProductHandler) error {
	// First scrape non-EC2 services (they fit in memory).
	// EC2 is handled separately via streamEC2ByRegion for per-region streaming.
	products, err := s.scrapeServices(ctx, true)
	if err != nil {
		return err
	}
	if len(products) > 0 {
		if err := handler(ctx, products); err != nil {
			return fmt.Errorf("upsert non-EC2 products: %w", err)
		}
	}

	// Then scrape EC2 per-region, upserting each region immediately to avoid OOM
	return s.streamEC2ByRegion(ctx, handler)
}

func (s *AWSScraper) Scrape(ctx context.Context) ([]db.Product, error) {
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

// chinaServices is the subset of AWS services available in the cn partition.
var chinaServices = []string{
	"AmazonEC2", "AWSLambda", "AmazonRDS", "AmazonS3",
	"AmazonDynamoDB", "AmazonElastiCache", "AmazonCloudFront", "AmazonES",
}

type scrapeTask struct {
	service   string
	partition string
}

func (s *AWSScraper) scrapeServices(ctx context.Context, skipEC2 bool) ([]db.Product, error) {
	var allProducts []db.Product
	var mu sync.Mutex

	concurrency := s.cfg.ConcurrencyForVendor("aws")
	if concurrency <= 0 {
		concurrency = 4
	}

	// Build task list: global and China services in a single pass so the
	// errgroup can schedule them concurrently without waiting for global to finish.
	var tasks []scrapeTask
	for _, svc := range awsServices {
		if skipEC2 && svc == "AmazonEC2" {
			continue
		}
		tasks = append(tasks, scrapeTask{svc, "aws"})
	}
	for _, svc := range chinaServices {
		tasks = append(tasks, scrapeTask{svc, "cn"})
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)
	for _, task := range tasks {
		task := task
		g.Go(func() error {
			slog.Info("scraping service", "vendor", "aws", "partition", task.partition, "service", task.service)
			products, err := s.scrapeService(gctx, task.service, task.partition)
			if err != nil {
				atomic.AddInt64(&s.failedServices, 1)
				slog.Warn("scrape failed", "vendor", "aws", "partition", task.partition, "service", task.service, "error", err)
				return nil
			}
			slog.Info("scraped service", "vendor", "aws", "partition", task.partition, "service", task.service, "products", len(products))
			mu.Lock()
			allProducts = append(allProducts, products...)
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	return allProducts, nil
}

// fetchGzip makes an HTTP GET with Accept-Encoding: gzip and transparently
// decompresses the response. AWS pricing CDN serves gzip, reducing a 1.5GB EC2
// file to ~150-250MB on the wire, the single biggest scrape speedup.
func (s *AWSScraper) fetchGzip(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept-Encoding", "gzip")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != 200 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	body := resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("gzip init: %w", err)
		}
		body = gzipReadCloser{gz, resp.Body}
	}
	return body, nil
}

// gzipReadCloser wraps a gzip.Reader and the underlying response body so both
// are closed properly.
type gzipReadCloser struct {
	gz         io.ReadCloser
	underlying io.Closer
}

func (g gzipReadCloser) Read(p []byte) (int, error) { return g.gz.Read(p) }
func (g gzipReadCloser) Close() error {
	_ = g.gz.Close()
	return g.underlying.Close()
}

func (s *AWSScraper) scrapeService(ctx context.Context, serviceCode string, partition string) ([]db.Product, error) {
	url := fmt.Sprintf("%s/offers/v1.0/%s/%s/current/index.json", awsBaseURL, partition, serviceCode)

	body, err := s.fetchGzip(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", serviceCode, err)
	}
	defer func() { _ = body.Close() }()

	return s.parseAWSPricing(body, serviceCode)
}

type regionEntry struct {
	name string
	url  string
}

// fetchEC2RegionIndex downloads the EC2 region index and returns only standard
// regions (filtering out wavelength zones, local zones, etc.). Standard region
// names have at most 2 hyphens (e.g., "us-east-1"). The region_index.json from
// AWS contains both standard regions and extended zones.
func (s *AWSScraper) fetchEC2RegionIndex(ctx context.Context) ([]regionEntry, error) {
	indexURL := fmt.Sprintf("%s/offers/v1.0/aws/AmazonEC2/current/region_index.json", awsBaseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, indexURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create EC2 region index request: %w", err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch EC2 region index: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("EC2 region index HTTP %d", resp.StatusCode)
	}

	var regionIndex struct {
		Regions map[string]struct {
			CurrentVersionURL string `json:"currentVersionUrl"`
		} `json:"regions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&regionIndex); err != nil {
		return nil, fmt.Errorf("decode EC2 region index: %w", err)
	}

	var regions []regionEntry
	for region, info := range regionIndex.Regions {
		hyphens := 0
		for _, ch := range region {
			if ch == '-' {
				hyphens++
			}
		}
		if hyphens <= 2 {
			regions = append(regions, regionEntry{region, awsBaseURL + info.CurrentVersionURL})
		}
	}
	return regions, nil
}

// streamEC2ByRegion scrapes EC2 per-region with bounded concurrency and upserts
// each region's products to the database immediately. With 3 concurrent regions,
// peak memory stays under ~4.5GB while achieving ~3x speedup over sequential.
func (s *AWSScraper) streamEC2ByRegion(ctx context.Context, handler ProductHandler) error {
	regions, err := s.fetchEC2RegionIndex(ctx)
	if err != nil {
		return err
	}

	slog.Info("streaming EC2 by region", "standard_regions", len(regions))

	// Each region file uses 500MB-1.5GB during parsing. Limit concurrency to
	// bound peak memory. Use SCRAPE_CONCURRENCY env (default 2) to control this.
	maxConcurrentRegions := s.cfg.ConcurrencyForVendor("aws")
	if maxConcurrentRegions < 1 {
		maxConcurrentRegions = 2
	}

	var (
		totalProducts int64
		totalFailed   int64
	)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxConcurrentRegions)

	for _, r := range regions {
		r := r
		g.Go(func() error {
			slog.Info("scraping EC2 region", "region", r.name)

			body, err := s.fetchGzip(gctx, r.url)
			if err != nil {
				atomic.AddInt64(&totalFailed, 1)
				slog.Warn("EC2 region fetch failed", "region", r.name, "error", err)
				return nil
			}

			products, err := s.parseAWSPricing(body, "AmazonEC2")
			_ = body.Close()
			if err != nil {
				atomic.AddInt64(&totalFailed, 1)
				slog.Warn("EC2 region parse failed", "region", r.name, "error", err)
				return nil
			}

			if len(products) > 0 {
				if err := handler(gctx, products); err != nil {
					slog.Warn("EC2 region upsert failed", "region", r.name, "error", err)
					return nil
				}
			}

			atomic.AddInt64(&totalProducts, int64(len(products)))
			slog.Info("scraped EC2 region", "region", r.name, "products", len(products))
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return err
	}

	failed := atomic.LoadInt64(&totalFailed)
	total := atomic.LoadInt64(&totalProducts)
	if failed > 0 && total == 0 {
		return fmt.Errorf("all %d EC2 regions failed to scrape", failed)
	}
	if failed > 0 {
		slog.Warn("some EC2 regions failed", "failed", failed, "total_products", total)
	}

	slog.Info("EC2 streaming complete", "total_products", total, "regions_processed", len(regions), "regions_failed", failed)
	return nil
}

type awsProduct struct {
	SKU           string            `json:"sku"`
	ProductFamily string            `json:"productFamily"`
	Attributes    map[string]string `json:"attributes"`
}

type awsTermOffer struct {
	PriceDimensions map[string]awsPriceDimension `json:"priceDimensions"`
	TermAttributes  map[string]string            `json:"termAttributes"`
}

type awsPriceDimension struct {
	Unit         string            `json:"unit"`
	PricePerUnit map[string]string `json:"pricePerUnit"`
	BeginRange   string            `json:"beginRange"`
	EndRange     string            `json:"endRange"`
	Description  string            `json:"description"`
}

// awsRawPricing holds the raw data extracted from an AWS pricing JSON file
// before it is transformed into db.Product entries.
type awsRawPricing struct {
	products      map[string]awsProduct
	onDemandTerms map[string]map[string]awsTermOffer
	reservedTerms map[string]map[string]awsTermOffer
}

// decodeAWSPricing streams through an AWS pricing JSON file and extracts the
// products map and OnDemand/Reserved term maps without loading the entire file
// into memory at once.
func decodeAWSPricing(reader io.Reader, serviceCode string) (*awsRawPricing, error) {
	decoder := json.NewDecoder(reader)

	raw := &awsRawPricing{
		products: make(map[string]awsProduct),
	}

	// Read opening brace
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("decode %s: %w", serviceCode, err)
	}

	for decoder.More() {
		tok, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("decode %s key: %w", serviceCode, err)
		}
		key, ok := tok.(string)
		if !ok {
			continue
		}

		switch key {
		case "products":
			// Stream products one by one
			if _, err := decoder.Token(); err != nil { // opening {
				return nil, fmt.Errorf("decode %s products open: %w", serviceCode, err)
			}
			for decoder.More() {
				skuTok, err := decoder.Token()
				if err != nil {
					return nil, fmt.Errorf("decode %s product key: %w", serviceCode, err)
				}
				sku := skuTok.(string)
				var prod awsProduct
				if err := decoder.Decode(&prod); err != nil {
					return nil, fmt.Errorf("decode %s product %s: %w", serviceCode, sku, err)
				}
				prod.SKU = sku
				raw.products[sku] = prod
			}
			if _, err := decoder.Token(); err != nil { // closing }
				return nil, fmt.Errorf("decode %s products close: %w", serviceCode, err)
			}

		case "terms":
			// Stream terms: { "OnDemand": {...}, "Reserved": {...} }
			if _, err := decoder.Token(); err != nil { // opening {
				return nil, fmt.Errorf("decode %s terms open: %w", serviceCode, err)
			}
			for decoder.More() {
				termTypeTok, err := decoder.Token()
				if err != nil {
					return nil, fmt.Errorf("decode %s term type: %w", serviceCode, err)
				}
				termType := termTypeTok.(string)
				switch termType {
				case "OnDemand":
					if err := decoder.Decode(&raw.onDemandTerms); err != nil {
						return nil, fmt.Errorf("decode %s OnDemand terms: %w", serviceCode, err)
					}
				case "Reserved":
					if err := decoder.Decode(&raw.reservedTerms); err != nil {
						return nil, fmt.Errorf("decode %s Reserved terms: %w", serviceCode, err)
					}
				default:
					// Skip unknown term types
					var rawMsg json.RawMessage
					if err := decoder.Decode(&rawMsg); err != nil {
						return nil, fmt.Errorf("decode %s skip term %s: %w", serviceCode, termType, err)
					}
				}
			}
			if _, err := decoder.Token(); err != nil { // closing }
				return nil, fmt.Errorf("decode %s terms close: %w", serviceCode, err)
			}

		default:
			// Skip other top-level keys (formatVersion, disclaimer, offerCode, version, publicationDate)
			var rawMsg json.RawMessage
			if err := decoder.Decode(&rawMsg); err != nil {
				return nil, fmt.Errorf("decode %s skip %s: %w", serviceCode, key, err)
			}
		}
	}

	return raw, nil
}

// buildProductList transforms raw AWS pricing data into a list of db.Product
// entries by matching products with their OnDemand and Reserved terms.
func (s *AWSScraper) buildProductList(raw *awsRawPricing, serviceCode string) []db.Product {
	var products []db.Product

	for sku, awsProd := range raw.products {
		location := awsProd.Attributes["location"]
		region := AWSLocationToRegion(location)
		if region == "" {
			if location == "Any" || location == "Global" || location == "" {
				region = ""
			} else {
				region = location
			}
		}

		attrs := make(map[string]string)
		for k, v := range awsProd.Attributes {
			attrs[k] = v
		}
		// Per-service attribute normalisation (SageMaker instanceType
		// suffix strip, EKS mode discriminator, Athena productFamily
		// backfill). Runs before productFamily lookup so an Athena
		// SKU's derived productFamily flows into the final value.
		normalizeAWSAttributes(serviceCode, attrs)

		productHash := ProductHash("aws", region, serviceCode, sku)
		productFamily := normalizeProductFamily(awsProd.ProductFamily)
		// If the normaliser injected a productFamily into attrs and
		// the upstream's was empty, prefer the injected value.
		if productFamily == "" {
			productFamily = attrs["productFamily"]
		}

		product := db.Product{
			ProductHash:   productHash,
			SKU:           sku,
			VendorName:    "aws",
			Region:        region,
			Service:       serviceCode,
			ProductFamily: productFamily,
			Attributes:    attrs,
			Prices:        []db.Price{},
		}

		// On-Demand terms
		if offers, ok := raw.onDemandTerms[sku]; ok {
			for _, offer := range offers {
				for _, dim := range offer.PriceDimensions {
					usd := dim.PricePerUnit["USD"]
					if usd == "" {
						if cny := dim.PricePerUnit["CNY"]; cny != "" {
							usd = s.cnyToUSD(cny)
						}
						if usd == "" {
							continue
						}
					}

					endRange := dim.EndRange
					if endRange == "Inf" {
						endRange = ""
					}

					priceH := PriceHash(productHash, "on_demand", dim.Unit, dim.BeginRange, dim.Description, "", "", "")
					product.Prices = append(product.Prices, db.Price{
						PriceHash:        priceH,
						PurchaseOption:   "on_demand",
						Unit:             dim.Unit,
						USD:              usd,
						StartUsageAmount: dim.BeginRange,
						EndUsageAmount:   endRange,
						Description:      dim.Description,
					})
				}
			}
		}

		// Reserved terms
		if offers, ok := raw.reservedTerms[sku]; ok {
			for _, offer := range offers {
				termLength := offer.TermAttributes["LeaseContractLength"]
				termPurchaseOption := offer.TermAttributes["PurchaseOption"]
				termOfferingClass := offer.TermAttributes["OfferingClass"]

				for _, dim := range offer.PriceDimensions {
					usd := dim.PricePerUnit["USD"]
					if usd == "" {
						if cny := dim.PricePerUnit["CNY"]; cny != "" {
							usd = s.cnyToUSD(cny)
						}
						if usd == "" {
							continue
						}
					}

					if dim.Unit == "Quantity" {
						continue
					}

					endRange := dim.EndRange
					if endRange == "Inf" {
						endRange = ""
					}

					priceH := PriceHash(productHash, "reserved", dim.Unit, dim.BeginRange, dim.Description, termLength, termPurchaseOption, termOfferingClass)
					product.Prices = append(product.Prices, db.Price{
						PriceHash:          priceH,
						PurchaseOption:     "reserved",
						Unit:               dim.Unit,
						USD:                usd,
						StartUsageAmount:   dim.BeginRange,
						EndUsageAmount:     endRange,
						Description:        dim.Description,
						TermLength:         termLength,
						TermPurchaseOption: termPurchaseOption,
						TermOfferingClass:  termOfferingClass,
					})
				}
			}
		}

		if len(product.Prices) > 0 {
			products = append(products, product)
		}
	}

	return products
}

func (s *AWSScraper) parseAWSPricing(reader io.Reader, serviceCode string) ([]db.Product, error) {
	raw, err := decodeAWSPricing(reader, serviceCode)
	if err != nil {
		return nil, err
	}
	return s.buildProductList(raw, serviceCode), nil
}

func (s *AWSScraper) cnyToUSD(cny string) string {
	cnyVal, err := strconv.ParseFloat(cny, 64)
	if err != nil {
		return ""
	}
	if cnyVal == 0 {
		return "0.0000000000"
	}
	usd := cnyVal / s.cnyUSDRate
	return fmt.Sprintf("%.10f", usd)
}

// productFamilyMapping maps AWS product family names to the format the CLI expects.
// Package-level to avoid allocating a new map on every call (called per-product).
var productFamilyMapping = map[string]string{
	"DNS Domain Names": "DNS Zone",
	"DNS Query":        "DNS Query",
}

func normalizeProductFamily(family string) string {
	if mapped, ok := productFamilyMapping[family]; ok {
		return mapped
	}
	return family
}

// normalizeAWSAttributes applies per-service attribute transformations
// that make the upstream data addressable by c3x-go catalog filters.
// AWS publishes pricing JSON with attribute conventions that vary by
// service in ways that make exact-match filtering brittle without a
// small normalization step.
//
// The function mutates attrs in place. Backfills are conservative:
// we only add a derived attribute when the source SKU clearly
// matches the pattern, never overriding an explicit upstream value.
func normalizeAWSAttributes(serviceCode string, attrs map[string]string) {
	switch serviceCode {
	case "AmazonSageMaker":
		// SageMaker SKUs encode the component in instanceType
		// ("ml.c6g.large-Hosting"). Strip the suffix so c3x catalog
		// queries on `instanceType = "ml.c6g.large"` resolve. The
		// component lives in its own attribute already.
		if it := attrs["instanceType"]; it != "" {
			if idx := strings.IndexByte(it, '-'); idx > 0 {
				// Only strip if the suffix is capitalised (a role
				// tag like "Hosting"/"Notebook") — preserves
				// legitimate hyphens like `ml.p6-b200.48xlarge`.
				if suffix := it[idx+1:]; len(suffix) > 0 && suffix[0] >= 'A' && suffix[0] <= 'Z' {
					attrs["instanceType"] = it[:idx]
				}
			}
		}

	case "AmazonEKS":
		// EKS classic control-plane SKUs and Auto-mode SKUs share
		// the same product family; add a `mode` attribute derived
		// from usagetype so catalog filters can target one or the
		// other.
		if ut := attrs["usagetype"]; ut != "" && attrs["mode"] == "" {
			switch {
			case strings.Contains(ut, "EKS-Auto"):
				attrs["mode"] = "Auto"
			case strings.Contains(ut, "AmazonEKS-Hours") ||
				strings.Contains(ut, "EKS-Cluster") ||
				strings.Contains(ut, "Cluster-Hours"):
				attrs["mode"] = "Classic"
			}
		}

	case "AmazonAthena":
		// Athena per-TB-scanned SKUs ship with `productFamily = ""`
		// because the upstream lists them under a Query operation.
		// Backfill the family so catalog mappings can pin
		// `productFamily = "Query"`.
		if attrs["productFamily"] == "" {
			if op := attrs["operation"]; strings.HasPrefix(op, "Query") ||
				strings.Contains(attrs["usagetype"], "DataScannedBytes") {
				attrs["productFamily"] = "Query"
			}
		}
	}
}
