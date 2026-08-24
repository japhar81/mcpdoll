// Copyright 2026 The MCPDoll Authors.

package api_test

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/mcpdoll/mcpdoll/internal/api"
	"github.com/mcpdoll/mcpdoll/internal/controlplane/apiserver"
)

// The contract test.
//
// ADR 0004 says api/openapi.yaml is the source of truth for every surface. That
// is only true if something checks it: `make parity` proves each operation has
// a CLI command and a UI route, but nothing there would notice a Go struct
// renaming a field the spec still advertises. A client generated from the spec
// would then ask for a field that no longer exists, and the failure would land
// on whoever reads the console at 2am rather than on whoever made the change.
//
// This walks the spec's schemas and the Go structs they describe and requires
// the JSON field names to agree exactly. It is not full schema validation —
// types and formats are not compared — but field names are where drift actually
// happens, and a missing field is the failure that silently produces undefined.

// schemas maps a schema in api/openapi.yaml to the Go type it describes.
//
// Every schema must appear. A new schema with no Go type is a contract nobody
// implements; a new Go type with no schema is an undocumented response.
var schemas = map[string]any{
	"Health":                api.Health{},
	"HookList":              api.HookList{},
	"Registry":              api.Registry{},
	"RegistrySummary":       api.RegistrySummary{},
	"Namespace":             api.Namespace{},
	"Server":                api.Server{},
	"Toolset":               api.Toolset{},
	"Binding":               api.Binding{},
	"Policy":                api.Policy{},
	"Plugin":                api.Plugin{},
	"PluginList":            api.PluginList{},
	"ServerList":            api.ServerList{},
	"Snapshot":              api.Snapshot{},
	"TenantSnapshotSummary": api.TenantSnapshotSummary{},
	"ToolSummary":           api.ToolSummary{},
	"TenantList":            api.TenantList{},
	"TenantSummary":         api.TenantSummary{},
	"Tenant":                api.Tenant{},
	"User":                  api.User{},
	"UserList":              api.UserList{},
	"Grant":                 api.Grant{},
	"GrantList":             api.GrantList{},
	"APIKey":                api.APIKey{},
	"APIKeyList":            api.APIKeyList{},
	"MintedAPIKey":          api.MintedAPIKey{},
	"Role":                  api.Role{},
	"RoleCatalog":           api.RoleCatalog{},
	"Session":               api.Session{},
	"SessionInfo":           api.SessionInfo{},
	"RevocationReport":      api.RevocationReport{},
	"Revocation":            api.Revocation{},
	"BuildReport":           api.BuildReport{},
	"BackendReport":         api.BackendReport{},
	"VerifyReport":          api.VerifyReport{},
	"SigningKey":            api.SigningKey{},
	"GatewayStatus":         api.GatewayStatus{},
	"BackendHealthReport":   api.BackendHealthReport{},
	"BackendHealthSummary":  api.BackendHealthSummary{},
	"BackendHealth":         api.BackendHealth{},
	"ToolDrift":             api.ToolDrift{},
	"Catalog":               api.Catalog{},
	"CatalogTool":           api.CatalogTool{},
	"CallResult":            api.CallResult{},
	"Error":                 apiserver.Error{},

	// Request bodies live with the handlers that decode them, because they are
	// the server's input contract rather than a shared wire type.
	"ValidateRegistryRequest":   apiserver.ValidateRegistryRequest{},
	"InspectSnapshotRequest":    apiserver.InspectSnapshotRequest{},
	"VerifySnapshotRequest":     apiserver.VerifySnapshotRequest{},
	"BuildSnapshotRequest":      apiserver.BuildSnapshotRequest{},
	"GenerateSigningKeyRequest": apiserver.GenerateSigningKeyRequest{},
	"CallToolRequest":           apiserver.CallToolRequest{},
	"CreateTenantRequest":       apiserver.CreateTenantRequest{},
	"CreateUserRequest":         apiserver.CreateUserRequest{},
	"UpdateUserRequest":         apiserver.UpdateUserRequest{},
	"PutGrantsRequest":          apiserver.PutGrantsRequest{},
	"MintAPIKeyRequest":         apiserver.MintAPIKeyRequest{},
	"LoginRequest":              apiserver.LoginRequest{},
}

type openapiDoc struct {
	Components struct {
		Schemas map[string]schemaNode `yaml:"schemas"`
	} `yaml:"components"`
}

type schemaNode struct {
	Type       string                `yaml:"type"`
	Properties map[string]schemaNode `yaml:"properties"`
	Required   []string              `yaml:"required"`
	AllOf      []schemaNode          `yaml:"allOf"`
	Ref        string                `yaml:"$ref"`
}

func loadSpec(t *testing.T) openapiDoc {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "api", "openapi.yaml"))
	require.NoError(t, err)

	var doc openapiDoc
	require.NoError(t, yaml.Unmarshal(raw, &doc))
	require.NotEmpty(t, doc.Components.Schemas, "the spec defines no schemas")
	return doc
}

func TestEverySchemaHasAGoType(t *testing.T) {
	t.Parallel()
	doc := loadSpec(t)

	var orphans []string
	for name := range doc.Components.Schemas {
		if _, ok := schemas[name]; !ok {
			orphans = append(orphans, name)
		}
	}
	sort.Strings(orphans)
	require.Empty(t, orphans,
		"schema(s) in api/openapi.yaml describe no Go type — a contract nobody implements")
}

func TestEveryGoTypeHasASchema(t *testing.T) {
	t.Parallel()
	doc := loadSpec(t)

	var missing []string
	for name := range schemas {
		if _, ok := doc.Components.Schemas[name]; !ok {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	require.Empty(t, missing,
		"Go type(s) have no schema — an undocumented response a generated client cannot read")
}

func TestSchemaFieldsMatchTheGoStructs(t *testing.T) {
	t.Parallel()
	doc := loadSpec(t)

	for name, value := range schemas {
		schema, ok := doc.Components.Schemas[name]
		if !ok {
			continue // reported by TestEveryGoTypeHasASchema
		}

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			specFields := propertyNames(schema, doc)
			goFields := jsonFieldNames(reflect.TypeOf(value))

			missing := difference(specFields, goFields)
			require.Empty(t, missing,
				"%s: the spec advertises field(s) the Go struct does not marshal — "+
					"a generated client would read undefined", name)

			undocumented := difference(goFields, specFields)
			require.Empty(t, undocumented,
				"%s: the Go struct marshals field(s) the spec does not document — "+
					"the console cannot use what it does not know exists", name)
		})
	}
}

func TestRequiredFieldsAreNotOmitempty(t *testing.T) {
	t.Parallel()
	doc := loadSpec(t)

	for name, value := range schemas {
		schema, ok := doc.Components.Schemas[name]
		if !ok || len(schema.Required) == 0 {
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			omitted := omitemptyFields(reflect.TypeOf(value))
			for _, field := range schema.Required {
				require.NotContains(t, omitted, field,
					"%s.%s is required by the spec but `omitempty` in Go, so it "+
						"vanishes at its zero value — and a client that trusted "+
						"`required` would crash on the one response where it mattered",
					name, field)
			}
		})
	}
}

// propertyNames collects a schema's properties, following allOf composition one
// level (AudienceList embeds GatewayStatus, which Go models by embedding too).
func propertyNames(node schemaNode, doc openapiDoc) []string {
	var out []string
	for name := range node.Properties {
		out = append(out, name)
	}
	for _, part := range node.AllOf {
		if ref := refName(part.Ref); ref != "" {
			if target, ok := doc.Components.Schemas[ref]; ok {
				out = append(out, propertyNames(target, doc)...)
				continue
			}
		}
		out = append(out, propertyNames(part, doc)...)
	}
	sort.Strings(out)
	return out
}

func refName(ref string) string {
	const prefix = "#/components/schemas/"
	if strings.HasPrefix(ref, prefix) {
		return strings.TrimPrefix(ref, prefix)
	}
	return ""
}

// jsonFieldNames returns the JSON names a struct marshals, following embedded
// structs the way encoding/json does.
func jsonFieldNames(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		name, _ := jsonTag(field)
		if name == "-" {
			continue
		}
		if field.Anonymous && name == "" {
			out = append(out, jsonFieldNames(field.Type)...)
			continue
		}
		if name == "" {
			name = field.Name
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func omitemptyFields(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		name, opts := jsonTag(field)
		if field.Anonymous && name == "" {
			out = append(out, omitemptyFields(field.Type)...)
			continue
		}
		if name != "" && name != "-" && strings.Contains(opts, "omitempty") {
			out = append(out, name)
		}
	}
	return out
}

func jsonTag(field reflect.StructField) (name, opts string) {
	tag := field.Tag.Get("json")
	name, opts, _ = strings.Cut(tag, ",")
	return name, opts
}

func difference(a, b []string) []string {
	inB := make(map[string]bool, len(b))
	for _, s := range b {
		inB[s] = true
	}
	var out []string
	for _, s := range a {
		if !inB[s] {
			out = append(out, s)
		}
	}
	return out
}

func TestEveryRefResolvesAndEverySchemaIsUsed(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("..", "..", "api", "openapi.yaml"))
	require.NoError(t, err)
	text := string(raw)

	doc := loadSpec(t)
	defined := map[string]bool{}
	for name := range doc.Components.Schemas {
		defined[name] = true
	}

	referenced := map[string]bool{}
	for _, match := range schemaRef.FindAllStringSubmatch(text, -1) {
		name := match[1]
		// A dangling $ref is the trace a renamed schema leaves. YAML will not
		// complain, and a generated client emits a broken type rather than an
		// error, so nothing catches it before a request fails at runtime.
		require.True(t, defined[name],
			"$ref points at %q, which no schema defines — usually a rename that "+
				"missed a reference", name)
		referenced[name] = true
	}

	var orphans []string
	for name := range defined {
		if !referenced[name] {
			orphans = append(orphans, name)
		}
	}
	sort.Strings(orphans)
	// An unreferenced schema is the other half of the same rename: the new
	// definition landed but nothing points at it.
	require.Empty(t, orphans, "schema(s) defined but never referenced")
}

var schemaRef = regexp.MustCompile(`#/components/schemas/(\w+)`)
