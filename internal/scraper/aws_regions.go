package scraper

var awsLocationToRegion = map[string]string{
	"US East (N. Virginia)":      "us-east-1",
	"US East (Ohio)":             "us-east-2",
	"US West (N. California)":    "us-west-1",
	"US West (Oregon)":           "us-west-2",
	"Africa (Cape Town)":         "af-south-1",
	"Asia Pacific (Hong Kong)":   "ap-east-1",
	"Asia Pacific (Hyderabad)":   "ap-south-2",
	"Asia Pacific (Jakarta)":     "ap-southeast-3",
	"Asia Pacific (Melbourne)":   "ap-southeast-4",
	"Asia Pacific (Mumbai)":      "ap-south-1",
	"Asia Pacific (Osaka)":       "ap-northeast-3",
	"Asia Pacific (Seoul)":       "ap-northeast-2",
	"Asia Pacific (Singapore)":   "ap-southeast-1",
	"Asia Pacific (Sydney)":      "ap-southeast-2",
	"Asia Pacific (Tokyo)":       "ap-northeast-1",
	"Canada (Central)":           "ca-central-1",
	"Canada West (Calgary)":      "ca-west-1",
	"EU (Frankfurt)":             "eu-central-1",
	"EU (Ireland)":               "eu-west-1",
	"EU (London)":                "eu-west-2",
	"EU (Milan)":                 "eu-south-1",
	"EU (Paris)":                 "eu-west-3",
	"EU (Spain)":                 "eu-south-2",
	"Europe (Spain)":             "eu-south-2",
	"EU (Stockholm)":             "eu-north-1",
	"EU (Zurich)":                "eu-central-2",
	"Europe (Zurich)":            "eu-central-2",
	"Israel (Tel Aviv)":          "il-central-1",
	"Middle East (Bahrain)":      "me-south-1",
	"Middle East (UAE)":          "me-central-1",
	"South America (Sao Paulo)":  "sa-east-1",
	"AWS GovCloud (US-East)":     "us-gov-east-1",
	"AWS GovCloud (US-West)":     "us-gov-west-1",
	"Asia Pacific (Malaysia)":    "ap-southeast-5",
	"Asia Pacific (Thailand)":    "ap-southeast-7",
	"Asia Pacific (Taipei)":      "ap-east-2",
	"Asia Pacific (New Zealand)": "ap-southeast-6",
	"Mexico (Central)":           "mx-central-1",
	// China regions
	"China (Beijing)": "cn-north-1",
	"China (Ningxia)": "cn-northwest-1",
}

// awsProductRegion returns the region code a product is stored under.
// The offer file's own regionCode is preferred for products in a region
// proper, so a region AWS launches later doesn't need a map entry; the
// name map covers files without one. Local Zones, Wavelength Zones and
// Outposts also carry their parent's regionCode, and must not be folded
// into it, or a lookup in the parent region would match their (higher)
// prices too; they keep their location name, as before.
func awsProductRegion(attrs map[string]string) string {
	if code := attrs["regionCode"]; code != "" && attrs["locationType"] == "AWS Region" {
		return code
	}
	location := attrs["location"]
	if r := AWSLocationToRegion(location); r != "" {
		return r
	}
	if location == "Any" || location == "Global" {
		return ""
	}
	return location
}

func AWSLocationToRegion(location string) string {
	if r, ok := awsLocationToRegion[location]; ok {
		return r
	}
	return ""
}
