// Package openapi serves the generated OpenAPI 3 specifications for the public
// and admin APIs, and the Swagger UI that renders them.
//
// The specifications are built at development time: swag turns the handler
// annotations into a Swagger 2.0 document, tools/openapigen converts it to
// OpenAPI 3, and the result is committed here (api.json, admin.json) so a build
// needs neither tool. Regenerate them with `make docs`. The specs cannot drift
// from the routers unnoticed: a test in each cmd package walks the real route
// table and fails when it and the committed spec disagree.
package openapi

import (
	_ "embed"
	"net/http"

	"github.com/flowchartsman/swaggerui"
)

//go:embed api.json
var apiSpec []byte

//go:embed admin.json
var adminSpec []byte

// APISpec returns the public API's OpenAPI 3 document.
func APISpec() []byte { return apiSpec }

// AdminSpec returns the admin API's OpenAPI 3 document.
func AdminSpec() []byte { return adminSpec }

// JSON serves spec as the raw OpenAPI document.
func JSON(spec []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(spec)
	}
}

// UI serves a self-hosted Swagger UI that renders spec. Mount it under a path
// prefix, e.g. r.Handle("/docs/*", http.StripPrefix("/docs", openapi.UI(spec))).
func UI(spec []byte) http.Handler { return swaggerui.Handler(spec) }
