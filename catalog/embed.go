// Package catalog is the c3x resource knowledge base: declarative
// TOML definitions describing how each Terraform resource kind maps
// to billable dimensions and pricing-catalog filters.
//
// THIS REPOSITORY IS THE SOURCE OF TRUTH. The c3x CLI is a thin
// client: it fetches this catalog from the /catalog endpoint (with
// an embedded snapshot only as the offline fallback), so new
// resource support ships by deploying this service — no CLI release
// required.
//
// Authoring a resource = one TOML file under catalog/<provider>/.
// The schema is documented in the c3x repository's ARCHITECTURE.md.
package catalog

import "embed"

//go:embed aws/*.toml azure/*.toml gcp/*.toml
var FS embed.FS
