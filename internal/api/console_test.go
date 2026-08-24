// Copyright 2026 Henry Zektser.

package api_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// The third side of the contract.
//
// `make parity` proves every operation has a CLI command and a console route,
// and `schema_test.go` proves every response shape matches the Go struct that
// produces it. Neither looks at the URL the console actually fetches.
//
// So a route can move on the server, the operation keeps its id, parity stays
// green, the types still compile — and the console 404s at runtime. That is
// exactly what happened when `listTenants` moved from
// `/api/v1/gateway/tenants` to `/api/v1/tenants`: three checks passed and the
// screen was broken until somebody clicked it.
//
// This closes it. It is a Go test reading a TypeScript file, which is the same
// shape as tools/paritycheck reading the generated route manifest: the check
// belongs where `make test` runs it, not where it would need a browser.

var (
	// Every string literal in api.ts that looks like a request path.
	consolePath = regexp.MustCompile("[\"`](/(?:api/v1|healthz)[^\"`]*)[\"`]")
	// `{param}` in the spec.
	specParam = regexp.MustCompile(`\{[^}]*\}`)
)

// stripInterpolation replaces each `${...}` in a template literal.
//
// A hand-written regex will not do it: `${query({ tools })}` nests braces, and
// RE2 has no recursion, so a non-greedy match stops at the inner `}` and leaves
// a fragment that matches nothing in the spec. Scanning for the balanced close
// is a few lines and is actually correct.
//
// An interpolation that calls the query helper is a query string, which the
// spec declares as parameters rather than as part of the path, so it becomes
// nothing. Anything else is a path parameter and becomes a placeholder.
func stripInterpolation(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); {
		if !strings.HasPrefix(p[i:], "${") {
			b.WriteByte(p[i])
			i++
			continue
		}
		depth, j := 0, i+1
		for ; j < len(p); j++ {
			switch p[j] {
			case '{':
				depth++
			case '}':
				depth--
			}
			if depth == 0 {
				break
			}
		}
		if j >= len(p) {
			// Unbalanced: leave it alone rather than silently truncating, so a
			// malformed literal fails loudly instead of matching by accident.
			b.WriteString(p[i:])
			break
		}
		if !strings.Contains(p[i:j], "query(") {
			b.WriteString("{}")
		}
		i = j + 1
	}
	return b.String()
}

// normalisePath reduces a path to something comparable across the two
// languages: every parameter becomes a single placeholder, and any query string
// is dropped because the spec declares those separately.
func normalisePath(p string) string {
	p = stripInterpolation(p)
	p = specParam.ReplaceAllString(p, "{}")
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	return strings.TrimRight(p, "/")
}

func specPaths(t *testing.T) map[string]bool {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("..", "..", "api", "openapi.yaml"))
	require.NoError(t, err)

	var doc struct {
		Paths map[string]yaml.Node `yaml:"paths"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &doc))
	require.NotEmpty(t, doc.Paths, "the spec declares no paths")

	out := map[string]bool{}
	for p := range doc.Paths {
		out[normalisePath(p)] = true
	}
	return out
}

func consolePaths(t *testing.T) []string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("..", "..", "web", "src", "lib", "api.ts"))
	require.NoError(t, err)

	seen := map[string]bool{}
	for _, m := range consolePath.FindAllStringSubmatch(string(raw), -1) {
		seen[m[1]] = true
	}
	require.NotEmpty(t, seen,
		"no request paths found in web/src/lib/api.ts — the file's shape changed "+
			"and this check no longer understands it. Fix the pattern rather than "+
			"letting it pass vacuously.")

	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func TestEveryConsolePathIsDeclaredBySpec(t *testing.T) {
	t.Parallel()

	declared := specPaths(t)
	var missing []string
	for _, p := range consolePaths(t) {
		if !declared[normalisePath(p)] {
			missing = append(missing, p)
		}
	}
	require.Empty(t, missing,
		"the console fetches path(s) the spec does not declare. An operation "+
			"that moved server-side keeps its id, so `make parity` stays green "+
			"and the screen 404s at runtime")
}

func TestEverySpecPathIsFetchedByTheConsole(t *testing.T) {
	t.Parallel()

	fetched := map[string]bool{}
	for _, p := range consolePaths(t) {
		fetched[normalisePath(p)] = true
	}

	var orphans []string
	for p := range specPaths(t) {
		if !fetched[p] {
			orphans = append(orphans, p)
		}
	}
	sort.Strings(orphans)

	// The other half of the same rename: the client was updated and the server
	// was not, or an operation exists that no console screen reaches. The
	// tri-surface law says the second cannot happen, so either way this is a
	// real gap rather than a style preference.
	require.Empty(t, orphans,
		"path(s) the spec declares that the console never fetches")
}
