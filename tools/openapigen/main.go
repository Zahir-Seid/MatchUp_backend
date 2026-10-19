// Command openapigen converts the Swagger 2.0 document swag emits from handler
// annotations into the OpenAPI 3 document the servers embed, so the artifact
// committed under internal/openapi is the format clients consume.
//
// swag has no OpenAPI 3 output; conversion happens here once per spec at
// development time rather than on every request. The converted document is
// validated before it is written, so `make docs` fails loudly instead of
// committing a spec that only looks right.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/getkin/kin-openapi/openapi2"
	"github.com/getkin/kin-openapi/openapi2conv"
	"github.com/getkin/kin-openapi/openapi3"
)

func main() {
	in := flag.String("in", "", "path to the Swagger 2.0 JSON swag generated")
	out := flag.String("out", "", "path to write the OpenAPI 3 JSON to")
	server := flag.String("server", "", "server URL recorded in the spec, e.g. /v1")
	stripTags := flag.String("strip-tags", "", "comma-separated operation tags to drop from the output document")
	flag.Parse()
	if *in == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "openapigen: -in and -out are required")
		os.Exit(2)
	}
	if err := run(*in, *out, *server, splitList(*stripTags)); err != nil {
		fmt.Fprintf(os.Stderr, "openapigen: %v\n", err)
		os.Exit(1)
	}
}

func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func run(in, out, server string, stripTags []string) error {
	raw, err := os.ReadFile(in)
	if err != nil {
		return err
	}

	var doc2 openapi2.T
	if err := json.Unmarshal(raw, &doc2); err != nil {
		return fmt.Errorf("parse swagger 2.0 document: %w", err)
	}

	doc3, err := openapi2conv.ToV3(&doc2)
	if err != nil {
		return fmt.Errorf("convert to OpenAPI 3: %w", err)
	}

	// A relative server URL keeps "try it out" requests pointed at whichever
	// origin serves the docs, in development and production alike. The caller
	// passes the binary's base path ("/v1", "/admin/v1") because the Swagger
	// 2.0 document does not reliably carry it through conversion.
	if server != "" {
		doc3.Servers = openapi3.Servers{{URL: server}}
	}
	stripOperationTags(doc3, stripTags)
	bearerSecurity(doc3)

	if err := doc3.Validate(context.Background()); err != nil {
		return fmt.Errorf("validate converted spec: %w", err)
	}

	// swag names the instantiation of admin.Page[T] after the parameter
	// ("Page-ReviewItem"), which reads badly in a generated client and differs
	// from every other model. Rename the generated envelope schemas to
	// "<T>Page" on the JSON tree, then reparse and validate so a $ref left
	// pointing at a stale key cannot be written out.
	marshaled, err := json.Marshal(doc3)
	if err != nil {
		return fmt.Errorf("marshal OpenAPI 3 document: %w", err)
	}
	var tree map[string]any
	if err := json.Unmarshal(marshaled, &tree); err != nil {
		return fmt.Errorf("reparse OpenAPI 3 document as a tree: %w", err)
	}
	renamePaginatedSchemas(tree)

	converted, err := json.MarshalIndent(tree, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal OpenAPI 3 document: %w", err)
	}

	// Parse the renamed document the way a consumer does — with a loader, so
	// local refs are resolved into their targets — and validate it. A rename
	// that leaves a $ref dangling fails the build here, not in a client.
	loader := openapi3.NewLoader()
	renamed, err := loader.LoadFromData(converted)
	if err != nil {
		return fmt.Errorf("reparse renamed spec: %w", err)
	}
	if err := renamed.Validate(context.Background()); err != nil {
		return fmt.Errorf("validate renamed spec: %w", err)
	}

	if err := os.WriteFile(out, append(converted, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("openapigen: wrote %s\n", out)
	return nil
}

// envelopeSchemaName matches the schema swag generates for an instantiated
// envelope type — admin.Page[T] or admin.Report[T] — such as
// "Page-ReviewItem", "admin.Report-DailyVolume".
var envelopeSchemaName = regexp.MustCompile(`^(?:[A-Za-z0-9_]+\.)?(Page|Report)-([A-Za-z0-9_]+)$`)

// renamePaginatedSchemas renames every instantiated envelope schema to
// "<T><Envelope>" — "ReviewItemPage", "DailyVolumeReport" — updating the
// component key and every $ref that pointed at it.
func renamePaginatedSchemas(doc map[string]any) {
	rename := func(name string) string {
		match := envelopeSchemaName.FindStringSubmatch(name)
		if match == nil {
			return name
		}
		return match[2] + match[1]
	}

	if components, ok := doc["components"].(map[string]any); ok {
		if schemas, ok := components["schemas"].(map[string]any); ok {
			for name, schema := range schemas {
				renamed := rename(name)
				if renamed == name {
					continue
				}
				delete(schemas, name)
				schemas[renamed] = schema
			}
		}
	}
	rewriteRefs(doc, rename)
}

// rewriteRefs rewrites every "$ref" string in the document tree.
func rewriteRefs(node any, rename func(string) string) {
	switch value := node.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "$ref" {
				if ref, ok := child.(string); ok {
					value[key] = renameRef(ref, rename)
				}
				continue
			}
			rewriteRefs(child, rename)
		}
	case []any:
		for _, child := range value {
			rewriteRefs(child, rename)
		}
	}
}

// renameRef applies rename to the schema segment of a local $ref, leaving
// refs into other component types (parameters, responses) alone.
func renameRef(ref string, rename func(string) string) string {
	const prefix = "#/components/schemas/"
	if !strings.HasPrefix(ref, prefix) {
		return ref
	}
	return prefix + rename(strings.TrimPrefix(ref, prefix))
}

// stripOperationTags removes bookkeeping tags from every operation. swag's
// scans are scoped with a marker tag ("!public" for the admin scan) because
// its --exclude flag only understands directories; the marker has no business
// appearing in the document the client reads, so the caller strips it here.
func stripOperationTags(doc *openapi3.T, strip []string) {
	if len(strip) == 0 || doc.Paths == nil {
		return
	}
	drop := make(map[string]struct{}, len(strip))
	for _, tag := range strip {
		drop[tag] = struct{}{}
	}
	for _, item := range doc.Paths.Map() {
		for _, op := range item.Operations() {
			if op == nil || len(op.Tags) == 0 {
				continue
			}
			kept := make([]string, 0, len(op.Tags))
			for _, tag := range op.Tags {
				if _, ok := drop[tag]; !ok {
					kept = append(kept, tag)
				}
			}
			op.Tags = kept
		}
	}
}

// bearerSecurity rewrites the Authorization apiKey scheme swag produces (Swagger
// 2.0 has no bearer type) into the http/bearer scheme the API actually accepts,
// so Swagger UI's Authorize box prepends "Bearer " rather than asking the user
// to type the header value verbatim.
func bearerSecurity(doc *openapi3.T) {
	if doc.Components == nil {
		return
	}
	for name, ref := range doc.Components.SecuritySchemes {
		if ref == nil || ref.Value == nil {
			continue
		}
		scheme := ref.Value
		if scheme.Type == "apiKey" &&
			strings.EqualFold(scheme.In, "header") &&
			strings.EqualFold(scheme.Name, "Authorization") {
			doc.Components.SecuritySchemes[name] = &openapi3.SecuritySchemeRef{
				Value: &openapi3.SecurityScheme{
					Type:         "http",
					Scheme:       "bearer",
					BearerFormat: "JWT",
					Description:  scheme.Description,
				},
			}
		}
	}
}
