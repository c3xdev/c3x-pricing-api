package scraper

var awsLocationToRegion = map[string]string{
	"US East (N. Virginia)":       "us-east-1",
	"US East (Ohio)":              "us-east-2",
	"US West (N. California)":     "us-west-1",
	"US West (Oregon)":            "us-west-2",
	"Africa (Cape Town)":          "af-south-1",
	"Asia Pacific (Hong Kong)":    "ap-east-1",
	"Asia Pacific (Hyderabad)":    "ap-south-2",
	"Asia Pacific (Jakarta)":      "ap-southeast-3",
	"Asia Pacific (Melbourne)":    "ap-southeast-4",
	"Asia Pacific (Mumbai)":       "ap-south-1",
	"Asia Pacific (Osaka)":        "ap-northeast-3",
	"Asia Pacific (Seoul)":        "ap-northeast-2",
	"Asia Pacific (Singapore)":    "ap-southeast-1",
	"Asia Pacific (Sydney)":       "ap-southeast-2",
	"Asia Pacific (Tokyo)":        "ap-northeast-1",
	"Canada (Central)":            "ca-central-1",
	"Canada West (Calgary)":       "ca-west-1",
	"EU (Frankfurt)":              "eu-central-1",
	"EU (Ireland)":                "eu-west-1",
	"EU (London)":                 "eu-west-2",
	"EU (Milan)":                  "eu-south-1",
	"EU (Paris)":                  "eu-west-3",
	"EU (Spain)":                  "eu-south-2",
	"EU (Stockholm)":              "eu-north-1",
	"EU (Zurich)":                 "eu-central-2",
	"Israel (Tel Aviv)":           "il-central-1",
	"Middle East (Bahrain)":       "me-south-1",
	"Middle East (UAE)":           "me-central-1",
	"South America (Sao Paulo)":   "sa-east-1",
	"AWS GovCloud (US-East)":      "us-gov-east-1",
	"AWS GovCloud (US-West)":      "us-gov-west-1",
	"Asia Pacific (Malaysia)":     "ap-southeast-5",
	"Asia Pacific (Thailand)":     "ap-southeast-7",
	// China regions
	"China (Beijing)":             "cn-north-1",
	"China (Ningxia)":             "cn-northwest-1",
}

func AWSLocationToRegion(location string) string {
	if r, ok := awsLocationToRegion[location]; ok {
		return r
	}
	return ""
}
