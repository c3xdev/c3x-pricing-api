package graphql

import (
	"sort"

	"github.com/c3xdev/c3x-pricing-api/internal/db"
	gql "github.com/graphql-go/graphql"
)

func NewSchema(database *db.DB) (gql.Schema, error) {
	attributeType := gql.NewObject(gql.ObjectConfig{
		Name: "Attribute",
		Fields: gql.Fields{
			"key":   &gql.Field{Type: gql.NewNonNull(gql.String)},
			"value": &gql.Field{Type: gql.NewNonNull(gql.String)},
		},
	})

	priceFilterInput := gql.NewInputObject(gql.InputObjectConfig{
		Name: "PriceFilter",
		Fields: gql.InputObjectConfigFieldMap{
			"purchaseOption":     &gql.InputObjectFieldConfig{Type: gql.String},
			"unit":               &gql.InputObjectFieldConfig{Type: gql.String},
			"description":        &gql.InputObjectFieldConfig{Type: gql.String},
			"description_regex":  &gql.InputObjectFieldConfig{Type: gql.String},
			"startUsageAmount":   &gql.InputObjectFieldConfig{Type: gql.String},
			"endUsageAmount":     &gql.InputObjectFieldConfig{Type: gql.String},
			"termLength":         &gql.InputObjectFieldConfig{Type: gql.String},
			"termPurchaseOption": &gql.InputObjectFieldConfig{Type: gql.String},
			"termOfferingClass":  &gql.InputObjectFieldConfig{Type: gql.String},
		},
	})

	priceType := gql.NewObject(gql.ObjectConfig{
		Name: "Price",
		Fields: gql.Fields{
			"priceHash":          &gql.Field{Type: gql.NewNonNull(gql.String)},
			"purchaseOption":     &gql.Field{Type: gql.String},
			"unit":               &gql.Field{Type: gql.NewNonNull(gql.String)},
			"USD":                &gql.Field{Type: gql.NewNonNull(gql.String)},
			"startUsageAmount":   &gql.Field{Type: gql.String},
			"endUsageAmount":     &gql.Field{Type: gql.String},
			"description":        &gql.Field{Type: gql.String},
			"termLength":         &gql.Field{Type: gql.String},
			"termPurchaseOption": &gql.Field{Type: gql.String},
			"termOfferingClass":  &gql.Field{Type: gql.String},
		},
	})

	attributeFilterInput := gql.NewInputObject(gql.InputObjectConfig{
		Name: "AttributeFilter",
		Fields: gql.InputObjectConfigFieldMap{
			"key":         &gql.InputObjectFieldConfig{Type: gql.NewNonNull(gql.String)},
			"value":       &gql.InputObjectFieldConfig{Type: gql.String},
			"value_regex": &gql.InputObjectFieldConfig{Type: gql.String},
		},
	})

	productFilterInput := gql.NewInputObject(gql.InputObjectConfig{
		Name: "ProductFilter",
		Fields: gql.InputObjectConfigFieldMap{
			"vendorName":       &gql.InputObjectFieldConfig{Type: gql.String},
			"service":          &gql.InputObjectFieldConfig{Type: gql.String},
			"productFamily":    &gql.InputObjectFieldConfig{Type: gql.String},
			"region":           &gql.InputObjectFieldConfig{Type: gql.String},
			"sku":              &gql.InputObjectFieldConfig{Type: gql.String},
			"attributeFilters": &gql.InputObjectFieldConfig{Type: gql.NewList(attributeFilterInput)},
		},
	})

	productType := gql.NewObject(gql.ObjectConfig{
		Name: "Product",
		Fields: gql.Fields{
			"productHash":   &gql.Field{Type: gql.NewNonNull(gql.String)},
			"vendorName":    &gql.Field{Type: gql.NewNonNull(gql.String)},
			"service":       &gql.Field{Type: gql.NewNonNull(gql.String)},
			"productFamily": &gql.Field{Type: gql.String},
			"region":        &gql.Field{Type: gql.String},
			"sku":           &gql.Field{Type: gql.NewNonNull(gql.String)},
			"attributes": &gql.Field{
				Type: gql.NewList(attributeType),
				Resolve: func(p gql.ResolveParams) (interface{}, error) {
					product := p.Source.(db.Product)
					keys := make([]string, 0, len(product.Attributes))
					for k := range product.Attributes {
						keys = append(keys, k)
					}
					sort.Strings(keys)
					attrs := make([]map[string]string, 0, len(product.Attributes))
					for _, k := range keys {
						attrs = append(attrs, map[string]string{"key": k, "value": product.Attributes[k]})
					}
					return attrs, nil
				},
			},
			"prices": &gql.Field{
				Type: gql.NewList(priceType),
				Args: gql.FieldConfigArgument{
					"filter": &gql.ArgumentConfig{Type: priceFilterInput},
				},
				Resolve: func(p gql.ResolveParams) (interface{}, error) {
					product := p.Source.(db.Product)
					prices := product.Prices

					filterArg, ok := p.Args["filter"]
					if ok && filterArg != nil {
						pf := parsePriceFilter(filterArg.(map[string]interface{}))
						prices = db.FilterPrices(prices, pf)
					}

					return prices, nil
				},
			},
		},
	})

	queryType := gql.NewObject(gql.ObjectConfig{
		Name: "Query",
		Fields: gql.Fields{
			"products": &gql.Field{
				Type: gql.NewList(productType),
				Args: gql.FieldConfigArgument{
					"filter": &gql.ArgumentConfig{Type: gql.NewNonNull(productFilterInput)},
					// M3: expose pagination args. Limit is server-capped at maxProductLimit
					// inside QueryProducts; offset supports stable cursor-less pagination.
					"limit":  &gql.ArgumentConfig{Type: gql.Int},
					"offset": &gql.ArgumentConfig{Type: gql.Int},
				},
				Resolve: func(p gql.ResolveParams) (interface{}, error) {
					filterArg := p.Args["filter"].(map[string]interface{})
					filter := parseProductFilter(filterArg)
					if v, ok := p.Args["limit"].(int); ok {
						filter.Limit = v
					}
					if v, ok := p.Args["offset"].(int); ok {
						filter.Offset = v
					}
					return database.QueryProducts(p.Context, filter)
				},
			},
		},
	})

	return gql.NewSchema(gql.SchemaConfig{
		Query: queryType,
	})
}

func parseProductFilter(m map[string]interface{}) *db.ProductFilter {
	f := &db.ProductFilter{}

	if v, ok := m["vendorName"].(string); ok {
		f.VendorName = &v
	}
	if v, ok := m["service"].(string); ok {
		f.Service = &v
	}
	if v, ok := m["productFamily"].(string); ok {
		f.ProductFamily = &v
	}
	if v, ok := m["region"].(string); ok {
		f.Region = &v
	}
	if v, ok := m["sku"].(string); ok {
		f.SKU = &v
	}

	if attrs, ok := m["attributeFilters"].([]interface{}); ok {
		for _, a := range attrs {
			am, ok := a.(map[string]interface{})
			if !ok {
				continue
			}
			key, _ := am["key"].(string)
			af := db.AttributeFilter{
				Key: key,
			}
			if v, ok := am["value"].(string); ok {
				af.Value = &v
			}
			if v, ok := am["value_regex"].(string); ok {
				af.ValueRegex = &v
			}
			f.AttributeFilters = append(f.AttributeFilters, af)
		}
	}

	return f
}

func parsePriceFilter(m map[string]interface{}) *db.PriceFilter {
	f := &db.PriceFilter{}

	if v, ok := m["purchaseOption"].(string); ok {
		f.PurchaseOption = &v
	}
	if v, ok := m["unit"].(string); ok {
		f.Unit = &v
	}
	if v, ok := m["description"].(string); ok {
		f.Description = &v
	}
	if v, ok := m["description_regex"].(string); ok {
		f.DescriptionRegex = &v
	}
	if v, ok := m["startUsageAmount"].(string); ok {
		f.StartUsageAmount = &v
	}
	if v, ok := m["endUsageAmount"].(string); ok {
		f.EndUsageAmount = &v
	}
	if v, ok := m["termLength"].(string); ok {
		f.TermLength = &v
	}
	if v, ok := m["termPurchaseOption"].(string); ok {
		f.TermPurchaseOption = &v
	}
	if v, ok := m["termOfferingClass"].(string); ok {
		f.TermOfferingClass = &v
	}

	return f
}
