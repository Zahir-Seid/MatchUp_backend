package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRenamePaginatedSchemas covers the admin.Page[T] envelope rename: the
// component key and every $ref that pointed at it.
func TestRenamePaginatedSchemas(t *testing.T) {
	doc := map[string]any{
		"components": map[string]any{
			"schemas": map[string]any{
				"Page-ReviewItem": map[string]any{
					"properties": map[string]any{
						"items": map[string]any{
							"items": map[string]any{"$ref": "#/components/schemas/Page-ReviewItem"},
						},
					},
				},
				"admin.Page-BookingSummary": map[string]any{},
				"Report-DailyVolume":        map[string]any{},
				"Formats-Report-X":          map[string]any{},
				"Other-Name":                map[string]any{},
			},
		},
		"paths": map[string]any{
			"/x": map[string]any{
				"get": map[string]any{
					"responses": map[string]any{
						"200": map[string]any{
							"content": map[string]any{
								"application/json": map[string]any{
									"schema": map[string]any{"$ref": "#/components/schemas/admin.Page-BookingSummary"},
								},
							},
						},
					},
				},
			},
		},
	}

	renamePaginatedSchemas(doc)

	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	for _, want := range []string{"ReviewItemPage", "BookingSummaryPage", "DailyVolumeReport", "Formats-Report-X", "Other-Name"} {
		if _, ok := schemas[want]; !ok {
			t.Errorf("schema %q is missing after rename; have %v", want, keys(schemas))
		}
	}
	for _, gone := range []string{"Page-ReviewItem", "admin.Page-BookingSummary", "Report-DailyVolume"} {
		if _, ok := schemas[gone]; ok {
			t.Errorf("schema %q should have been renamed away", gone)
		}
	}

	marshaled, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var tree map[string]any
	if err := json.Unmarshal(marshaled, &tree); err != nil {
		t.Fatal(err)
	}

	// The self-ref inside Page-ReviewItem must have followed the key.
	got := digString(t, tree, "components", "schemas", "ReviewItemPage", "properties", "items", "items", "$ref")
	if want := "#/components/schemas/ReviewItemPage"; got != want {
		t.Errorf("self ref = %q, want %q", got, want)
	}

	// The ref from a path into a renamed envelope must have followed too.
	got = digString(t, tree, "paths", "/x", "get", "responses", "200", "content", "application/json", "schema", "$ref")
	if want := "#/components/schemas/BookingSummaryPage"; got != want {
		t.Errorf("response ref = %q, want %q", got, want)
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func digString(t *testing.T, tree map[string]any, path ...string) string {
	t.Helper()
	var node any = tree
	for _, key := range path {
		m, ok := node.(map[string]any)
		if !ok {
			t.Fatalf("path %v is not an object at %q", path, key)
		}
		node = m[key]
	}
	s, _ := node.(string)
	if s == "" {
		t.Fatalf("path %v resolved to %v, want a string", path, node)
	}
	return strings.TrimSpace(s)
}
