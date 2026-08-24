// Copyright 2026 Henry Zektser.

// Command gents generates the console's wire types from api/openapi.yaml.
//
// `web/src/lib/types.ts` was hand-written, which made it a second definition of
// every response shape. The Go side has been held against the spec by
// internal/api/schema_test.go since early on, and the console's *paths* since
// internal/api/console_test.go — but a field renamed in Go and in the spec and
// not here compiled fine and read as `undefined` at runtime. Nothing catches a
// property that does not exist.
//
// So it is generated, and `make verify-generated` fails when it is stale.
//
// # It refuses rather than guesses
//
// Every construct this understands is listed in [emitType]. Anything else is a
// hard error naming the schema and the property, because the alternative — a
// generator that emits `unknown` for what it cannot parse — produces a file
// that compiles, type-checks, and is wrong. A build failure is recoverable in
// a minute; a silently weakened type is not recoverable at all.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

func main() {
	var (
		specPath = flag.String("spec", "api/openapi.yaml", "the OpenAPI document to read")
		outPath  = flag.String("out", "web/src/lib/types.ts", "the TypeScript file to write")
		check    = flag.Bool("check", false, "exit non-zero if the file is stale, writing nothing")
	)
	flag.Parse()

	raw, err := os.ReadFile(*specPath)
	if err != nil {
		fail("reading %s: %v", *specPath, err)
	}

	var doc document
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		fail("parsing %s: %v", *specPath, err)
	}
	if len(doc.Components.Schemas.keys) == 0 {
		fail("%s declares no schemas; that is a parse problem rather than an empty API", *specPath)
	}

	out, err := generate(&doc)
	if err != nil {
		fail("%v", err)
	}

	if *check {
		existing, err := os.ReadFile(*outPath)
		if err != nil {
			fail("reading %s: %v", *outPath, err)
		}
		if !bytes.Equal(existing, out) {
			fail("%s is stale — run `make generate`", *outPath)
		}
		return
	}
	if err := os.WriteFile(*outPath, out, 0o644); err != nil {
		fail("writing %s: %v", *outPath, err)
	}
	fmt.Fprintf(os.Stderr, "gents: wrote %d types to %s\n", len(doc.Components.Schemas.keys), *outPath)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "gents: "+format+"\n", args...)
	os.Exit(1)
}

// ------------------------------------------------------------------ input --

type document struct {
	Components struct {
		Schemas ordered `yaml:"schemas"`
	} `yaml:"components"`
}

// ordered preserves the document's key order.
//
// Go maps are unordered and the spec's field order is meaningful — it groups
// related fields and puts identity first. Alphabetising would produce a file
// that is correct and harder to read than the source it came from.
type ordered struct {
	keys []string
	vals map[string]*schema
}

func (o *ordered) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("expected a mapping, got %v", n.Kind)
	}
	o.vals = make(map[string]*schema, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		key := n.Content[i].Value
		var s schema
		if err := n.Content[i+1].Decode(&s); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		o.keys = append(o.keys, key)
		o.vals[key] = &s
	}
	return nil
}

type schema struct {
	Type        string   `yaml:"type"`
	Format      string   `yaml:"format"`
	Description string   `yaml:"description"`
	Required    []string `yaml:"required"`
	Enum        []string `yaml:"enum"`
	Ref         string   `yaml:"$ref"`
	Items       *schema  `yaml:"items"`
	AllOf       []schema `yaml:"allOf"`
	Properties  ordered  `yaml:"properties"`
	Additional  *additional
}

// UnmarshalYAML is hand-written only to reach `additionalProperties`, which is
// either a schema or the literal `true` and so cannot be a plain field.
func (s *schema) UnmarshalYAML(n *yaml.Node) error {
	type plain schema // avoid recursing into this method
	var p plain
	if err := n.Decode(&p); err != nil {
		return err
	}
	*s = schema(p)

	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value != "additionalProperties" {
			continue
		}
		var a additional
		if err := a.decode(n.Content[i+1]); err != nil {
			return err
		}
		s.Additional = &a
	}
	return nil
}

type additional struct {
	Any    bool
	Schema *schema
}

func (a *additional) decode(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		var b bool
		if err := n.Decode(&b); err != nil {
			return fmt.Errorf("additionalProperties: %w", err)
		}
		a.Any = b
		return nil
	}
	var s schema
	if err := n.Decode(&s); err != nil {
		return fmt.Errorf("additionalProperties: %w", err)
	}
	a.Schema = &s
	return nil
}

// ----------------------------------------------------------------- naming --

// renames maps a schema name to the TypeScript name it must have.
//
// `Error` is the only entry and it is not cosmetic: TypeScript has a global
// `Error`, and exporting an interface with that name from a module every screen
// imports would shadow it wherever both are in scope. The console has always
// called this `ApiErrorBody`.
var renames = map[string]string{"Error": "ApiErrorBody"}

func tsName(schemaName string) string {
	if n, ok := renames[schemaName]; ok {
		return n
	}
	return schemaName
}

var refPattern = regexp.MustCompile(`^#/components/schemas/(\w+)$`)

func refTarget(ref string) (string, bool) {
	m := refPattern.FindStringSubmatch(ref)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// ----------------------------------------------------------------- output --

func generate(doc *document) ([]byte, error) {
	var b strings.Builder
	b.WriteString(`// Code generated by tools/gents. DO NOT EDIT.
//
// Every type here comes from a schema in api/openapi.yaml, which ADR 0004 makes
// the source of truth for all three surfaces. It was hand-written until a
// renamed field could compile on this side and read as ` + "`undefined`" + ` at runtime —
// the Go structs were held against the spec and the console's paths were, but
// nothing checked its field names.
//
// Run ` + "`make generate`" + ` after editing the spec. ` + "`make verify-generated`" + ` fails
// the build when this file is stale.

`)

	known := map[string]bool{}
	for _, name := range doc.Components.Schemas.keys {
		known[name] = true
	}

	for _, name := range doc.Components.Schemas.keys {
		s := doc.Components.Schemas.vals[name]
		block, err := emitInterface(name, s, known)
		if err != nil {
			return nil, err
		}
		b.WriteString(block)
		b.WriteString("\n")
	}
	return []byte(b.String()), nil
}

func emitInterface(name string, s *schema, known map[string]bool) (string, error) {
	var b strings.Builder
	b.WriteString(docComment(s.Description, ""))

	// allOf is composition: a `$ref` to the base plus one inline object. That is
	// the only form the spec uses, and it maps exactly onto `extends`.
	extends, body, err := resolveComposition(name, s, known)
	if err != nil {
		return "", err
	}

	fmt.Fprintf(&b, "export interface %s", tsName(name))
	if extends != "" {
		fmt.Fprintf(&b, " extends %s", extends)
	}
	b.WriteString(" {\n")

	required := map[string]bool{}
	for _, r := range body.Required {
		required[r] = true
	}

	for _, prop := range body.Properties.keys {
		p := body.Properties.vals[prop]
		ts, err := emitType(name, prop, p, known)
		if err != nil {
			return "", err
		}
		b.WriteString(docComment(p.Description, "  "))
		optional := ""
		if !required[prop] {
			optional = "?"
		}
		fmt.Fprintf(&b, "  %s%s: %s;\n", prop, optional, ts)
	}
	b.WriteString("}\n")
	return b.String(), nil
}

// resolveComposition flattens the one allOf shape the spec uses.
func resolveComposition(name string, s *schema, known map[string]bool) (string, *schema, error) {
	if len(s.AllOf) == 0 {
		return "", s, nil
	}

	var extends string
	merged := &schema{}
	for i := range s.AllOf {
		part := &s.AllOf[i]
		switch {
		case part.Ref != "":
			target, ok := refTarget(part.Ref)
			if !ok || !known[target] {
				return "", nil, fmt.Errorf(
					"schema %q: allOf references %q, which no schema defines", name, part.Ref)
			}
			if extends != "" {
				// TypeScript can extend several interfaces, but the spec does
				// not do it and supporting it untested would be guessing.
				return "", nil, fmt.Errorf(
					"schema %q: allOf composes more than one $ref, which gents does not handle", name)
			}
			extends = tsName(target)
		case len(part.Properties.keys) > 0:
			if len(merged.Properties.keys) > 0 {
				return "", nil, fmt.Errorf(
					"schema %q: allOf has more than one inline object, which gents does not handle", name)
			}
			merged.Properties = part.Properties
			merged.Required = part.Required
		default:
			return "", nil, fmt.Errorf(
				"schema %q: an allOf member is neither a $ref nor an object", name)
		}
	}
	return extends, merged, nil
}

// emitType renders one property's TypeScript type.
//
// The complete list of what this understands. Anything else is an error naming
// the schema and the property, because a generator that fell back to `unknown`
// would produce a file that compiles and lies.
func emitType(owner, prop string, s *schema, known map[string]bool) (string, error) {
	if s.Ref != "" {
		target, ok := refTarget(s.Ref)
		if !ok || !known[target] {
			return "", fmt.Errorf("%s.%s: $ref %q resolves to no schema", owner, prop, s.Ref)
		}
		return tsName(target), nil
	}

	switch s.Type {
	case "string":
		if len(s.Enum) > 0 {
			quoted := make([]string, 0, len(s.Enum))
			for _, e := range s.Enum {
				quoted = append(quoted, `"`+e+`"`)
			}
			return strings.Join(quoted, " | "), nil
		}
		return "string", nil

	case "integer", "number":
		return "number", nil

	case "boolean":
		return "boolean", nil

	case "array":
		if s.Items == nil {
			return "", fmt.Errorf("%s.%s: an array with no `items`", owner, prop)
		}
		inner, err := emitType(owner, prop+"[]", s.Items, known)
		if err != nil {
			return "", err
		}
		// Parenthesise a union so `"a" | "b"[]` cannot mean the wrong thing.
		if strings.Contains(inner, "|") {
			inner = "(" + inner + ")"
		}
		return inner + "[]", nil

	case "object":
		if s.Additional == nil {
			return "", fmt.Errorf(
				"%s.%s: an inline object with no `additionalProperties`. Give it its own "+
					"schema and $ref it — an anonymous nested type is not addressable "+
					"from anywhere else", owner, prop)
		}
		if s.Additional.Any {
			return "Record<string, unknown>", nil
		}
		inner, err := emitType(owner, prop+"{}", s.Additional.Schema, known)
		if err != nil {
			return "", err
		}
		return "Record<string, " + inner + ">", nil

	case "":
		return "", fmt.Errorf(
			"%s.%s: no `type` and no `$ref`. gents refuses rather than emitting "+
				"`unknown`, which would compile and weaken the contract silently", owner, prop)

	default:
		return "", fmt.Errorf("%s.%s: unhandled type %q", owner, prop, s.Type)
	}
}

// docComment renders a description as JSDoc, or nothing.
func docComment(desc, indent string) string {
	desc = strings.TrimSpace(desc)
	if desc == "" {
		return ""
	}
	lines := strings.Split(desc, "\n")
	// Trim trailing blanks the YAML block scalar leaves behind.
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 1 {
		return fmt.Sprintf("%s/** %s */\n", indent, lines[0])
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s/**\n", indent)
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			fmt.Fprintf(&b, "%s *\n", indent)
			continue
		}
		fmt.Fprintf(&b, "%s * %s\n", indent, l)
	}
	fmt.Fprintf(&b, "%s */\n", indent)
	return b.String()
}
