package db

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

type Product struct {
	ProductHash   string            `json:"productHash"`
	SKU           string            `json:"sku"`
	VendorName    string            `json:"vendorName"`
	Region        string            `json:"region"`
	Service       string            `json:"service"`
	ProductFamily string            `json:"productFamily"`
	Attributes    map[string]string `json:"attributes"`
	Prices        []Price           `json:"prices"`
}

type Price struct {
	PriceHash          string `json:"priceHash"`
	PurchaseOption     string `json:"purchaseOption,omitempty"`
	Unit               string `json:"unit"`
	USD                string `json:"USD"`
	StartUsageAmount   string `json:"startUsageAmount,omitempty"`
	EndUsageAmount     string `json:"endUsageAmount,omitempty"`
	Description        string `json:"description,omitempty"`
	TermLength         string `json:"termLength,omitempty"`
	TermPurchaseOption string `json:"termPurchaseOption,omitempty"`
	TermOfferingClass  string `json:"termOfferingClass,omitempty"`
}

type ProductFilter struct {
	VendorName       *string
	Service          *string
	ProductFamily    *string
	Region           *string
	SKU              *string
	AttributeFilters []AttributeFilter
	// Pagination: Limit caps the number of rows. AfterHash enables keyset
	// pagination (WHERE product_hash > cursor ORDER BY product_hash LIMIT N),
	// which is O(1) at any depth vs OFFSET's O(N) scan-and-discard.
	// Offset is kept for backward compatibility but AfterHash is preferred.
	Limit     int
	Offset    int
	AfterHash string
}

type AttributeFilter struct {
	Key        string
	Value      *string
	ValueRegex *string
}

type PriceFilter struct {
	PurchaseOption     *string
	Unit               *string
	Description        *string
	DescriptionRegex   *string
	StartUsageAmount   *string
	EndUsageAmount     *string
	TermLength         *string
	TermPurchaseOption *string
	TermOfferingClass  *string
}

const maxRegexLength = 200

// M3: default and maximum page sizes for the products query.
const (
	defaultProductLimit = 5000
	maxProductLimit     = 5000
)

// H8: compiled regex cache. Bounded to avoid unbounded memory growth from
// distinct attacker-supplied patterns; when full, callers fall through to
// compile-without-cache. This keeps regex compilation cost O(1) for normal
// filter patterns issued repeatedly by the CLI.
const maxRegexCacheSize = 1024

var (
	regexCacheMu sync.RWMutex
	regexCache   = make(map[string]*regexp.Regexp, maxRegexCacheSize)
)

func compileRegex(pattern string) (*regexp.Regexp, error) {
	regexCacheMu.RLock()
	re, ok := regexCache[pattern]
	regexCacheMu.RUnlock()
	if ok {
		return re, nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	regexCacheMu.Lock()
	if len(regexCache) < maxRegexCacheSize {
		regexCache[pattern] = re
	}
	regexCacheMu.Unlock()
	return re, nil
}

func (d *DB) QueryProducts(ctx context.Context, filter *ProductFilter) ([]Product, error) {
	// Validate regex lengths
	for _, af := range filter.AttributeFilters {
		if af.ValueRegex != nil && len(*af.ValueRegex) > maxRegexLength {
			return nil, fmt.Errorf("regex pattern too long: %d chars exceeds maximum of %d", len(*af.ValueRegex), maxRegexLength)
		}
	}

	query := `SELECT product_hash, sku, vendor_name, region, service, product_family, attributes, prices FROM products WHERE 1=1`
	args := []interface{}{}
	argIdx := 1

	if filter.VendorName != nil {
		query += fmt.Sprintf(" AND vendor_name = $%d", argIdx)
		args = append(args, *filter.VendorName)
		argIdx++
	}
	if filter.Service != nil {
		query += fmt.Sprintf(" AND service = $%d", argIdx)
		args = append(args, *filter.Service)
		argIdx++
	}
	if filter.ProductFamily != nil {
		query += fmt.Sprintf(" AND product_family = $%d", argIdx)
		args = append(args, *filter.ProductFamily)
		argIdx++
	}
	if filter.Region != nil {
		query += fmt.Sprintf(" AND region = $%d", argIdx)
		args = append(args, *filter.Region)
		argIdx++
		// When querying by region, exclude global products to avoid duplicates.
		// Global products have usagetype starting with "Global" and overlap with regional products.
		query += " AND (attributes->>'usagetype' IS NULL OR attributes->>'usagetype' NOT LIKE 'Global%')"
	}
	if filter.SKU != nil {
		query += fmt.Sprintf(" AND sku = $%d", argIdx)
		args = append(args, *filter.SKU)
		argIdx++
	}

	// Push attribute matches into SQL for performance
	for _, af := range filter.AttributeFilters {
		if af.Value != nil {
			query += fmt.Sprintf(" AND attributes->>$%d = $%d", argIdx, argIdx+1)
			args = append(args, af.Key, *af.Value)
			argIdx += 2
		} else if af.ValueRegex != nil {
			pgRegex, caseInsensitive := parseRegexForPostgres(*af.ValueRegex)
			if pgRegex != "" {
				pgRegex = normalizeRegexForPrefix(pgRegex)
				op := "~"
				if caseInsensitive {
					op = "~*"
				}
				query += fmt.Sprintf(" AND attributes->>$%d %s $%d", argIdx, op, argIdx+1)
				args = append(args, af.Key, pgRegex)
				argIdx += 2
			}
		}
	}

	// Pagination: keyset (AfterHash) is preferred over OFFSET for O(1) deep pages.
	limit := filter.Limit
	if limit <= 0 || limit > maxProductLimit {
		limit = defaultProductLimit
	}
	if filter.AfterHash != "" {
		query += fmt.Sprintf(" AND product_hash > $%d", argIdx)
		args = append(args, filter.AfterHash)
		argIdx++
	} else if filter.Offset > 0 {
		query += fmt.Sprintf(" ORDER BY product_hash LIMIT $%d OFFSET $%d", argIdx, argIdx+1)
		args = append(args, limit, filter.Offset)
		argIdx += 2
		// Early return for legacy offset path
		rows, err := d.Pool.Query(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("query failed: %w", err)
		}
		defer rows.Close()
		return scanProducts(rows)
	}
	query += fmt.Sprintf(" ORDER BY product_hash LIMIT $%d", argIdx)
	args = append(args, limit)
	argIdx++

	rows, err := d.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query failed: %w", err)
	}
	defer rows.Close()

	return scanProducts(rows)
}

func scanProducts(rows pgx.Rows) ([]Product, error) {
	var products []Product

	for rows.Next() {
		var p Product
		var attrsJSON, pricesJSON []byte

		err := rows.Scan(&p.ProductHash, &p.SKU, &p.VendorName, &p.Region, &p.Service, &p.ProductFamily, &attrsJSON, &pricesJSON)
		if err != nil {
			return nil, fmt.Errorf("scan failed: %w", err)
		}

		if err := json.Unmarshal(attrsJSON, &p.Attributes); err != nil {
			return nil, fmt.Errorf("unmarshal attributes failed: %w", err)
		}
		if err := json.Unmarshal(pricesJSON, &p.Prices); err != nil {
			return nil, fmt.Errorf("unmarshal prices failed: %w", err)
		}

		// All regex attribute filters are pushed to PostgreSQL SQL (which supports
		// POSIX ARE including lookahead/lookbehind). No Go-side re-matching needed.
		products = append(products, p)
	}

	return products, rows.Err()
}


// MatchRegexPattern matches a value against a /PATTERN/ or /PATTERN/i regex pattern.
// Handles negative lookaheads (?!...) which Go's RE2 doesn't support natively.
func MatchRegexPattern(pattern, value string) bool {
	// H8: enforce a maximum pattern length everywhere (not just the SQL path).
	if len(pattern) > maxRegexLength {
		return false
	}
	if !strings.HasPrefix(pattern, "/") {
		re, err := compileRegex(pattern)
		if err != nil {
			return false
		}
		return re.MatchString(value)
	}

	lastSlash := strings.LastIndex(pattern[1:], "/")
	if lastSlash == -1 {
		return false
	}
	lastSlash++

	regexBody := pattern[1:lastSlash]
	flags := pattern[lastSlash+1:]
	caseInsensitive := strings.Contains(flags, "i")

	// Normalize leading \- to match both region-prefixed and non-prefixed values.
	regexBody = normalizeRegexForPrefix(regexBody)

	// Handle negative lookahead: ^(?!.*EXCLUDED).*$
	if strings.Contains(regexBody, "(?!") {
		return matchWithNegativeLookahead(regexBody, value, caseInsensitive)
	}

	// Handle negative lookbehind: (?<!PREFIX)PATTERN$
	// Go's RE2 doesn't support lookbehinds, so we simulate them.
	if strings.Contains(regexBody, "(?<!") {
		return matchWithNegativeLookbehind(regexBody, value, caseInsensitive)
	}

	if caseInsensitive {
		regexBody = "(?i)" + regexBody
	}

	re, err := compileRegex(regexBody)
	if err != nil {
		return false
	}
	return re.MatchString(value)
}

// matchWithNegativeLookahead handles patterns like ^(?!.*(Excluded1|Excluded2)$).*$
// by extracting the exclusion terms and checking if any match the value.
func matchWithNegativeLookahead(pattern, value string, caseInsensitive bool) bool {
	testValue := value
	if caseInsensitive {
		testValue = strings.ToLower(value)
	}

	// Extract all negative lookahead groups
	remaining := pattern
	for {
		start := strings.Index(remaining, "(?!")
		if start == -1 {
			break
		}

		// Find matching closing paren
		depth := 0
		end := -1
		for i := start; i < len(remaining); i++ {
			if remaining[i] == '(' {
				depth++
			} else if remaining[i] == ')' {
				depth--
				if depth == 0 {
					end = i
					break
				}
			}
		}

		if end == -1 {
			break
		}

		// Extract exclusion pattern: (?!.*(Term1|Term2)$) -> "Term1|Term2"
		exclusion := remaining[start+3 : end]
		exclusion = strings.TrimPrefix(exclusion, ".*")
		exclusion = strings.TrimSuffix(exclusion, "$")

		// Handle alternation: (Term1|Term2) or Term1|Term2
		exclusion = strings.TrimPrefix(exclusion, "(")
		exclusion = strings.TrimSuffix(exclusion, ")")

		if caseInsensitive {
			exclusion = strings.ToLower(exclusion)
		}

		// Split on | and check each alternative
		alternatives := strings.Split(exclusion, "|")
		for _, alt := range alternatives {
			alt = strings.TrimSpace(alt)
			if alt != "" && strings.Contains(testValue, alt) {
				return false
			}
		}

		remaining = remaining[end+1:]
	}

	// Check the positive part of the pattern.
	// Use compileRegex (cached) to avoid re-compiling hot patterns.
	cleanerRe, err := compileRegex(`\(\?![^)]*\)`)
	if err != nil {
		return true
	}
	cleaned := cleanerRe.ReplaceAllString(pattern, "")
	cleaned = strings.ReplaceAll(cleaned, "(?i)", "")
	if cleaned == "" || cleaned == "^.*$" || cleaned == ".*" {
		return true
	}

	prefix := ""
	if caseInsensitive {
		prefix = "(?i)"
	}

	re, err := compileRegex(prefix + cleaned)
	if err != nil {
		return true
	}
	return re.MatchString(value)
}

// matchWithNegativeLookbehind handles patterns like (?<!IA-)TimedStorage-ByteHrs$
// by extracting the lookbehind exclusion and checking it manually. Go's RE2 engine
// doesn't support lookbehinds, so we strip them, match the positive part, then
// verify the lookbehind condition by checking the text immediately before the match.
func matchWithNegativeLookbehind(pattern, value string, caseInsensitive bool) bool {
	testValue := value
	if caseInsensitive {
		testValue = strings.ToLower(value)
	}

	// Extract all negative lookbehind groups and the remaining positive pattern.
	// Pattern format: (?<!EXCLUDED)POSITIVE$
	remaining := pattern
	var exclusions []string

	for {
		start := strings.Index(remaining, "(?<!")
		if start == -1 {
			break
		}

		// Find matching closing paren
		depth := 0
		end := -1
		for i := start; i < len(remaining); i++ {
			if remaining[i] == '(' {
				depth++
			} else if remaining[i] == ')' {
				depth--
				if depth == 0 {
					end = i
					break
				}
			}
		}
		if end == -1 {
			break
		}

		// Extract exclusion: (?<!IA-) -> "IA-"
		exclusion := remaining[start+4 : end]
		if caseInsensitive {
			exclusion = strings.ToLower(exclusion)
		}
		exclusions = append(exclusions, exclusion)

		remaining = remaining[:start] + remaining[end+1:]
	}

	// Match the positive part
	if remaining == "" || remaining == "$" {
		return true
	}

	prefix := ""
	if caseInsensitive {
		prefix = "(?i)"
	}

	re, err := compileRegex(prefix + remaining)
	if err != nil {
		return false
	}

	loc := re.FindStringIndex(testValue)
	if loc == nil {
		return false
	}

	// Check each lookbehind exclusion: the text immediately before the match
	// must NOT end with the exclusion string.
	matchStart := loc[0]
	textBefore := testValue[:matchStart]
	for _, excl := range exclusions {
		if strings.HasSuffix(textBefore, excl) {
			return false
		}
	}

	return true
}

// parseRegexForPostgres converts /PATTERN/i format to a bare regex string and case flag
// suitable for PostgreSQL's ~ and ~* operators.
func parseRegexForPostgres(pattern string) (string, bool) {
	if !strings.HasPrefix(pattern, "/") {
		return pattern, false
	}
	lastSlash := strings.LastIndex(pattern[1:], "/")
	if lastSlash == -1 {
		return "", false
	}
	lastSlash++
	regexBody := pattern[1:lastSlash]
	flags := pattern[lastSlash+1:]
	caseInsensitive := strings.Contains(flags, "i")
	return regexBody, caseInsensitive
}

// normalizeRegexForPrefix transforms a regex that starts with an escaped hyphen
// (\-) into one that matches both a hyphen and the start of string. AWS pricing
// usagetype values have a region prefix in most regions (e.g., "USE1-RDS:GP3-Storage")
// but no prefix in us-east-1 (e.g., "RDS:GP3-Storage"). CLI regexes like
// \-RDS\:GP3\-Storage$ assume the prefix exists. This transformation makes them
// match both forms by replacing the leading \- with (^|-).
func normalizeRegexForPrefix(pattern string) string {
	if strings.HasPrefix(pattern, `\-`) {
		return "(^|-)" + pattern[2:]
	}
	return pattern
}

// normalizePurchaseOption normalizes purchase option strings for comparison.
// The CLI may send "on_demand", "on-demand", or "OnDemand" depending on the
// cloud provider. Stripping all separators after lowercasing ensures all
// formats map to the same canonical form: "ondemand", "reserved", etc.
func normalizePurchaseOption(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, "_", "")
	return s
}

func FilterPrices(prices []Price, filter *PriceFilter) []Price {
	if filter == nil {
		return prices
	}

	var result []Price
	for _, p := range prices {
		if filter.PurchaseOption != nil && normalizePurchaseOption(p.PurchaseOption) != normalizePurchaseOption(*filter.PurchaseOption) {
			continue
		}
		if filter.Unit != nil && p.Unit != *filter.Unit {
			continue
		}
		if filter.Description != nil && p.Description != *filter.Description {
			continue
		}
		if filter.DescriptionRegex != nil {
			if !MatchRegexPattern(*filter.DescriptionRegex, p.Description) {
				continue
			}
		}
		if filter.StartUsageAmount != nil {
			pStart := p.StartUsageAmount
			if pStart == "" {
				pStart = "0" // Treat empty startUsageAmount as "0"
			}
			if pStart != *filter.StartUsageAmount {
				continue
			}
		}
		if filter.EndUsageAmount != nil && p.EndUsageAmount != *filter.EndUsageAmount {
			continue
		}
		if filter.TermLength != nil && p.TermLength != *filter.TermLength {
			continue
		}
		if filter.TermPurchaseOption != nil && p.TermPurchaseOption != *filter.TermPurchaseOption {
			continue
		}
		if filter.TermOfferingClass != nil && p.TermOfferingClass != *filter.TermOfferingClass {
			continue
		}
		result = append(result, p)
	}
	return result
}

// maxPostgresParams is the maximum number of parameters PostgreSQL supports in a
// single prepared statement. We use this to cap the batch size so we never exceed it.
const maxPostgresParams = 65535

// colsPerRow is the number of columns inserted per product row.
const colsPerRow = 8

// maxBatchSize is the largest batch that fits within the PostgreSQL parameter limit.
var maxBatchSize = maxPostgresParams / colsPerRow // 8191

// UpsertProducts inserts or updates products in batches using efficient multi-row INSERT
func (d *DB) UpsertProducts(ctx context.Context, products []Product) error {
	const batchSize = 1000 // well within maxBatchSize (8191)
	total := len(products)

	for i := 0; i < total; i += batchSize {
		end := i + batchSize
		if end > total {
			end = total
		}
		if err := d.upsertBatch(ctx, products[i:end]); err != nil {
			return fmt.Errorf("upsert batch %d-%d: %w", i, end, err)
		}
		if (i/batchSize)%100 == 0 && i > 0 {
			slog.InfoContext(ctx, "upsert progress", "upserted", i, "total", total)
		}
	}
	return nil
}

func (d *DB) upsertBatch(ctx context.Context, products []Product) error {
	if len(products) == 0 {
		return nil
	}

	// Deduplicate within batch to avoid "ON CONFLICT DO UPDATE cannot affect row a second time"
	seen := make(map[string]bool)
	deduped := make([]Product, 0, len(products))
	for _, p := range products {
		if !seen[p.ProductHash] {
			seen[p.ProductHash] = true
			deduped = append(deduped, p)
		}
	}
	products = deduped

	// Safety: cap at the PostgreSQL parameter limit after dedup.
	if len(products) > maxBatchSize {
		return fmt.Errorf("batch size %d exceeds maximum safe size %d", len(products), maxBatchSize)
	}

	valueStrings := make([]string, 0, len(products))
	args := make([]interface{}, 0, len(products)*8)

	for i, p := range products {
		base := i * 8
		valueStrings = append(valueStrings, fmt.Sprintf(
			"($%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d)",
			base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8,
		))

		attrsJSON, err := json.Marshal(p.Attributes)
		if err != nil {
			return fmt.Errorf("marshal attributes for product %s: %w", p.ProductHash, err)
		}
		pricesJSON, err := json.Marshal(p.Prices)
		if err != nil {
			return fmt.Errorf("marshal prices for product %s: %w", p.ProductHash, err)
		}

		args = append(args,
			p.ProductHash, p.SKU, p.VendorName, p.Region,
			p.Service, p.ProductFamily, string(attrsJSON), string(pricesJSON),
		)
	}

	query := fmt.Sprintf(`
		INSERT INTO products (product_hash, sku, vendor_name, region, service, product_family, attributes, prices)
		VALUES %s
		ON CONFLICT (product_hash) DO UPDATE SET
			sku = EXCLUDED.sku,
			attributes = EXCLUDED.attributes,
			prices = EXCLUDED.prices,
			updated_at = NOW()
	`, strings.Join(valueStrings, ","))

	_, err := d.Pool.Exec(ctx, query, args...)
	return err
}

// DeleteStaleProducts removes products for a vendor that were not updated after the given time.
// This cleans up products that no longer exist in the upstream pricing data after a re-scrape.
func (d *DB) DeleteStaleProducts(ctx context.Context, vendor string, before time.Time) (int64, error) {
	result, err := d.Pool.Exec(ctx,
		"DELETE FROM products WHERE vendor_name = $1 AND updated_at < $2",
		vendor, before,
	)
	if err != nil {
		return 0, fmt.Errorf("delete stale products for %s: %w", vendor, err)
	}
	return result.RowsAffected(), nil
}

// RecordPriceSnapshots inserts a price snapshot for each product into the
// price_snapshots table. This creates an append-only audit trail of price
// changes over time, keyed by scrape run.
func (d *DB) RecordPriceSnapshots(ctx context.Context, products []Product, scrapeRunID int64) error {
	if scrapeRunID <= 0 || len(products) == 0 {
		return nil
	}

	const snapshotBatchSize = 1000
	const snapshotCols = 3 // product_hash, prices, scrape_run_id

	for i := 0; i < len(products); i += snapshotBatchSize {
		end := i + snapshotBatchSize
		if end > len(products) {
			end = len(products)
		}
		batch := products[i:end]

		valueStrings := make([]string, 0, len(batch))
		args := make([]interface{}, 0, len(batch)*snapshotCols)

		for _, p := range batch {
			pricesJSON, err := json.Marshal(p.Prices)
			if err != nil {
				continue
			}
			base := len(valueStrings) * snapshotCols
			valueStrings = append(valueStrings, fmt.Sprintf("($%d, $%d, $%d)", base+1, base+2, base+3))
			args = append(args, p.ProductHash, string(pricesJSON), scrapeRunID)
		}

		if len(valueStrings) == 0 {
			continue
		}

		query := fmt.Sprintf(
			`INSERT INTO price_snapshots (product_hash, prices, scrape_run_id) VALUES %s`,
			strings.Join(valueStrings, ","),
		)
		if _, err := d.Pool.Exec(ctx, query, args...); err != nil {
			return fmt.Errorf("record price snapshots batch %d-%d: %w", i, end, err)
		}
	}
	return nil
}
