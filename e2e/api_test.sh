#!/usr/bin/env bash
# Integration test suite for the C3X Pricing API.
# Runs against a live API instance (default: http://localhost:4000).
#
# Usage:
#   ./e2e/api_test.sh                          # test against localhost
#   API_URL=https://pricing.example.com ./e2e/api_test.sh  # test against remote
#
# Requires: curl, python3 (for JSON construction)
set -euo pipefail

API_URL="${API_URL:-http://localhost:4000}"
PASS=0
FAIL=0
ERRORS=""

# Helper: send a GraphQL query with variables and check for expected results.
# Args: test_name, expected_usd, expected_unit, product_filter_json, price_filter_json
run_price_test() {
    local name="$1"
    local expected_usd="$2"
    local expected_unit="$3"
    local product_filter="$4"
    local price_filter="${5:-null}"

    local payload
    payload=$(python3 -c "
import json, sys
pf = json.loads('''$product_filter''')
pr = json.loads('''$price_filter''') if '''$price_filter''' != 'null' else None
payload = {
    'query': 'query(\$f: ProductFilter!, \$p: PriceFilter) { products(filter: \$f) { productHash prices(filter: \$p) { USD unit } } }',
    'variables': {'f': pf, 'p': pr}
}
json.dump(payload, sys.stdout)
")

    local response
    response=$(curl -s -X POST "$API_URL/graphql" -H "Content-Type: application/json" -d "$payload")

    # Extract first product's first price
    local usd unit product_count
    product_count=$(echo "$response" | python3 -c "import json,sys; d=json.load(sys.stdin); print(len(d['data']['products']) if d['data'] else 0)" 2>/dev/null || echo "0")

    if [ "$product_count" = "0" ]; then
        FAIL=$((FAIL + 1))
        ERRORS="$ERRORS\n  FAIL: $name -no products returned"
        echo "  FAIL: $name -no products returned"
        return
    fi

    usd=$(echo "$response" | python3 -c "
import json,sys
d=json.load(sys.stdin)
prices = d['data']['products'][0]['prices']
if prices:
    print(f'{float(prices[0][\"USD\"]):.10f}')
else:
    print('NO_PRICES')
" 2>/dev/null || echo "PARSE_ERROR")

    unit=$(echo "$response" | python3 -c "
import json,sys
d=json.load(sys.stdin)
prices = d['data']['products'][0]['prices']
if prices:
    print(prices[0]['unit'])
else:
    print('NO_UNIT')
" 2>/dev/null || echo "PARSE_ERROR")

    # Compare USD (trim trailing zeros for comparison)
    local usd_trimmed expected_trimmed
    usd_trimmed=$(python3 -c "print(f'{float(\"$usd\"):.4f}')")
    expected_trimmed=$(python3 -c "print(f'{float(\"$expected_usd\"):.4f}')")

    if [ "$usd_trimmed" = "$expected_trimmed" ] && [ "$unit" = "$expected_unit" ]; then
        PASS=$((PASS + 1))
        echo "  PASS: $name -\$$usd_trimmed/$unit"
    else
        FAIL=$((FAIL + 1))
        ERRORS="$ERRORS\n  FAIL: $name -got \$$usd_trimmed/$unit, expected \$$expected_trimmed/$expected_unit"
        echo "  FAIL: $name -got \$$usd_trimmed/$unit, expected \$$expected_trimmed/$expected_unit"
    fi
}

# Helper: test that a query returns products (without checking specific prices)
run_exists_test() {
    local name="$1"
    local product_filter="$2"

    local payload
    payload=$(python3 -c "
import json, sys
pf = json.loads('''$product_filter''')
payload = {
    'query': 'query(\$f: ProductFilter!) { products(filter: \$f) { productHash } }',
    'variables': {'f': pf}
}
json.dump(payload, sys.stdout)
")

    local response
    response=$(curl -s -X POST "$API_URL/graphql" -H "Content-Type: application/json" -d "$payload")

    local product_count
    product_count=$(echo "$response" | python3 -c "import json,sys; d=json.load(sys.stdin); print(len(d['data']['products']) if d['data'] else 0)" 2>/dev/null || echo "0")

    if [ "$product_count" -gt 0 ]; then
        PASS=$((PASS + 1))
        echo "  PASS: $name -$product_count products"
    else
        FAIL=$((FAIL + 1))
        ERRORS="$ERRORS\n  FAIL: $name -no products returned"
        echo "  FAIL: $name -no products returned"
    fi
}

echo "============================================"
echo "C3X Pricing API Integration Tests"
echo "API: $API_URL"
echo "============================================"

# ------------------------------------------------------------------
# Health checks
# ------------------------------------------------------------------
echo ""
echo "--- Health Checks ---"

http_code=$(curl -s -o /dev/null -w "%{http_code}" "$API_URL/healthz")
if [ "$http_code" = "200" ]; then
    PASS=$((PASS + 1)); echo "  PASS: /healthz returns 200"
else
    FAIL=$((FAIL + 1)); echo "  FAIL: /healthz returned $http_code"
    ERRORS="$ERRORS\n  FAIL: /healthz returned $http_code"
fi

http_code=$(curl -s -o /dev/null -w "%{http_code}" "$API_URL/readyz")
if [ "$http_code" = "200" ]; then
    PASS=$((PASS + 1)); echo "  PASS: /readyz returns 200"
else
    FAIL=$((FAIL + 1)); echo "  FAIL: /readyz returned $http_code"
    ERRORS="$ERRORS\n  FAIL: /readyz returned $http_code"
fi

# ------------------------------------------------------------------
# Section 1: Example project resources (must match $650.44)
# ------------------------------------------------------------------
echo ""
echo "--- Example Project Resources (target: \$650.44) ---"

# 1. EC2 m5.xlarge
run_price_test "EC2 m5.xlarge (on-demand, Linux)" \
    "0.192" "Hrs" \
    '{"vendorName":"aws","service":"AmazonEC2","region":"us-east-1","productFamily":"Compute Instance","attributeFilters":[{"key":"instanceType","value":"m5.xlarge"},{"key":"operatingSystem","value":"Linux"},{"key":"tenancy","value":"Shared"},{"key":"preInstalledSw","value":"NA"},{"key":"capacitystatus","value":"Used"}]}' \
    '{"purchaseOption":"on_demand"}'

# 2. RDS db.r5.large Multi-AZ
run_price_test "RDS db.r5.large (Multi-AZ, PostgreSQL)" \
    "0.5" "Hrs" \
    '{"vendorName":"aws","service":"AmazonRDS","region":"us-east-1","productFamily":"Database Instance","attributeFilters":[{"key":"instanceType","value":"db.r5.large"},{"key":"databaseEngine","value":"PostgreSQL"},{"key":"deploymentOption","value":"Multi-AZ"}]}' \
    '{"purchaseOption":"on_demand"}'

# 3. RDS GP3 Storage Multi-AZ (the regex fix)
run_price_test "RDS GP3 Storage (Multi-AZ, regex filter)" \
    "0.23" "GB-Mo" \
    '{"vendorName":"aws","service":"AmazonRDS","region":"us-east-1","productFamily":"Database Storage","attributeFilters":[{"key":"deploymentOption","value":"Multi-AZ"},{"key":"databaseEngine","value":"Any"},{"key":"volumeType","value":"General Purpose-GP3"},{"key":"usagetype","value_regex":"/\\\\-RDS\\\\:Multi\\\\-AZ\\\\-GP3\\\\-Storage$/"}]}' \
    '{"purchaseOption":"on_demand"}'

# 4. NAT Gateway hours
run_price_test "NAT Gateway (hours)" \
    "0.045" "Hrs" \
    '{"vendorName":"aws","service":"AmazonEC2","region":"us-east-1","productFamily":"NAT Gateway","attributeFilters":[{"key":"usagetype","value_regex":"/NatGateway-Hours/"}]}' \
    '{"purchaseOption":"on_demand"}'

# 5. ALB hours
run_price_test "ALB (hours)" \
    "0.0225" "Hrs" \
    '{"vendorName":"aws","service":"AmazonEC2","region":"us-east-1","productFamily":"Load Balancer-Application","attributeFilters":[]}' \
    '{"purchaseOption":"on_demand"}'

# 6. EBS gp2
run_price_test "EBS gp2 (storage)" \
    "0.10" "GB-Mo" \
    '{"vendorName":"aws","service":"AmazonEC2","region":"us-east-1","productFamily":"Storage","attributeFilters":[{"key":"volumeApiName","value":"gp2"}]}' \
    '{"purchaseOption":"on_demand"}'

# 7. EBS gp3
run_price_test "EBS gp3 (storage)" \
    "0.08" "GB-Mo" \
    '{"vendorName":"aws","service":"AmazonEC2","region":"us-east-1","productFamily":"Storage","attributeFilters":[{"key":"volumeApiName","value":"gp3"}]}' \
    '{"purchaseOption":"on_demand"}'

# ------------------------------------------------------------------
# Section 2: Regex normalization edge cases
# ------------------------------------------------------------------
echo ""
echo "--- Regex Normalization (\\- prefix handling) ---"

# Single-AZ GP3 Storage (us-east-1, no region prefix)
run_price_test "GP3 Storage Single-AZ (us-east-1, no prefix)" \
    "0.115" "GB-Mo" \
    '{"vendorName":"aws","service":"AmazonRDS","region":"us-east-1","productFamily":"Database Storage","attributeFilters":[{"key":"deploymentOption","value":"Single-AZ"},{"key":"databaseEngine","value":"Any"},{"key":"volumeType","value":"General Purpose-GP3"},{"key":"usagetype","value_regex":"/\\\\-RDS\\\\:GP3\\\\-Storage$/"}]}' \
    '{"purchaseOption":"on_demand"}'

# GP3 Storage in eu-west-1 (has EU- prefix)
run_price_test "GP3 Storage Multi-AZ (eu-west-1, EU- prefix)" \
    "0.254" "GB-Mo" \
    '{"vendorName":"aws","service":"AmazonRDS","region":"eu-west-1","productFamily":"Database Storage","attributeFilters":[{"key":"deploymentOption","value":"Multi-AZ"},{"key":"databaseEngine","value":"Any"},{"key":"volumeType","value":"General Purpose-GP3"},{"key":"usagetype","value_regex":"/\\\\-RDS\\\\:Multi\\\\-AZ\\\\-GP3\\\\-Storage$/"}]}' \
    '{"purchaseOption":"on_demand"}'

# GP3 PIOPS Single-AZ
run_price_test "GP3 PIOPS Single-AZ (us-east-1)" \
    "0.02" "IOPS-Mo" \
    '{"vendorName":"aws","service":"AmazonRDS","region":"us-east-1","productFamily":"Provisioned IOPS","attributeFilters":[{"key":"deploymentOption","value":"Single-AZ"},{"key":"databaseEngine","value":"Any"},{"key":"usagetype","value_regex":"/\\\\-RDS\\\\:GP3\\\\-PIOPS$/"}]}' \
    '{"purchaseOption":"on_demand"}'

# GP3 PIOPS Multi-AZ
run_price_test "GP3 PIOPS Multi-AZ (us-east-1)" \
    "0.04" "IOPS-Mo" \
    '{"vendorName":"aws","service":"AmazonRDS","region":"us-east-1","productFamily":"Provisioned IOPS","attributeFilters":[{"key":"deploymentOption","value":"Multi-AZ"},{"key":"databaseEngine","value":"Any"},{"key":"usagetype","value_regex":"/\\\\-RDS\\\\:Multi\\\\-AZ\\\\-GP3\\\\-PIOPS$/"}]}' \
    '{"purchaseOption":"on_demand"}'

# ------------------------------------------------------------------
# Section 3: Purchase option normalization
# ------------------------------------------------------------------
echo ""
echo "--- Purchase Option Normalization ---"

# on_demand (underscore)
run_price_test "PurchaseOption: on_demand (underscore)" \
    "0.192" "Hrs" \
    '{"vendorName":"aws","service":"AmazonEC2","region":"us-east-1","productFamily":"Compute Instance","attributeFilters":[{"key":"instanceType","value":"m5.xlarge"},{"key":"operatingSystem","value":"Linux"},{"key":"tenancy","value":"Shared"},{"key":"preInstalledSw","value":"NA"},{"key":"capacitystatus","value":"Used"}]}' \
    '{"purchaseOption":"on_demand"}'

# on-demand (hyphen)
run_price_test "PurchaseOption: on-demand (hyphen)" \
    "0.192" "Hrs" \
    '{"vendorName":"aws","service":"AmazonEC2","region":"us-east-1","productFamily":"Compute Instance","attributeFilters":[{"key":"instanceType","value":"m5.xlarge"},{"key":"operatingSystem","value":"Linux"},{"key":"tenancy","value":"Shared"},{"key":"preInstalledSw","value":"NA"},{"key":"capacitystatus","value":"Used"}]}' \
    '{"purchaseOption":"on-demand"}'

# OnDemand (PascalCase -GCP style)
run_price_test "PurchaseOption: OnDemand (PascalCase)" \
    "0.192" "Hrs" \
    '{"vendorName":"aws","service":"AmazonEC2","region":"us-east-1","productFamily":"Compute Instance","attributeFilters":[{"key":"instanceType","value":"m5.xlarge"},{"key":"operatingSystem","value":"Linux"},{"key":"tenancy","value":"Shared"},{"key":"preInstalledSw","value":"NA"},{"key":"capacitystatus","value":"Used"}]}' \
    '{"purchaseOption":"OnDemand"}'

# ------------------------------------------------------------------
# Section 4: Regex edge cases
# ------------------------------------------------------------------
echo ""
echo "--- Regex Edge Cases ---"

# Case-insensitive regex
run_exists_test "Case-insensitive regex (/linux/i)" \
    '{"vendorName":"aws","service":"AmazonEC2","region":"us-east-1","productFamily":"Compute Instance","attributeFilters":[{"key":"instanceType","value":"m5.xlarge"},{"key":"operatingSystem","value_regex":"/linux/i"},{"key":"tenancy","value":"Shared"},{"key":"preInstalledSw","value":"NA"},{"key":"capacitystatus","value":"Used"}]}'

# Non-\- regex (simple pattern)
run_exists_test "Simple regex (/NatGateway-Bytes/)" \
    '{"vendorName":"aws","service":"AmazonEC2","region":"us-east-1","productFamily":"NAT Gateway","attributeFilters":[{"key":"usagetype","value_regex":"/NatGateway-Bytes/"}]}'

# ------------------------------------------------------------------
# Section 5: Cross-vendor data presence
# ------------------------------------------------------------------
echo ""
echo "--- Data Presence (vendor coverage) ---"

run_exists_test "AWS: EC2 instances exist" \
    '{"vendorName":"aws","service":"AmazonEC2","region":"us-east-1","productFamily":"Compute Instance"}'

run_exists_test "AWS: RDS instances exist" \
    '{"vendorName":"aws","service":"AmazonRDS","region":"us-east-1","productFamily":"Database Instance"}'

run_exists_test "AWS: S3 storage exists" \
    '{"vendorName":"aws","service":"AmazonS3","region":"us-east-1"}'

# Azure/GCP tests are conditional -skip if no data has been scraped for that vendor.
azure_count=$(curl -s -X POST "$API_URL/graphql" -H "Content-Type: application/json" \
    -d '{"query":"{ products(filter: {vendorName: \"azure\"}, limit: 1) { productHash } }"}' | \
    python3 -c "import json,sys; d=json.load(sys.stdin); print(len(d['data']['products']) if d['data'] else 0)" 2>/dev/null || echo "0")

if [ "$azure_count" -gt 0 ]; then
    run_exists_test "Azure: Virtual Machines exist" \
        '{"vendorName":"azure","service":"Virtual Machines","region":"eastus"}'
    run_exists_test "Azure: SQL Database exists" \
        '{"vendorName":"azure","service":"SQL Database","region":"eastus"}'
else
    echo "  SKIP: Azure tests (no Azure data scraped)"
fi

# ------------------------------------------------------------------
# Section 6: Error handling
# ------------------------------------------------------------------
echo ""
echo "--- Error Handling ---"

# Invalid method
http_code=$(curl -s -o /dev/null -w "%{http_code}" "$API_URL/graphql")
if [ "$http_code" = "405" ]; then
    PASS=$((PASS + 1)); echo "  PASS: GET /graphql returns 405"
else
    FAIL=$((FAIL + 1)); echo "  FAIL: GET /graphql returned $http_code, expected 405"
    ERRORS="$ERRORS\n  FAIL: GET /graphql returned $http_code, expected 405"
fi

# ------------------------------------------------------------------
# Summary
# ------------------------------------------------------------------
echo ""
echo "============================================"
echo "Results: $PASS passed, $FAIL failed"
echo "============================================"

if [ "$FAIL" -gt 0 ]; then
    echo ""
    echo "Failures:"
    echo -e "$ERRORS"
    exit 1
fi
