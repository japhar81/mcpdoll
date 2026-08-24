// Copyright 2026 Henry Zektser.

// Command paritycheck enforces MCPDoll's tri-surface rule: every API operation
// must be reachable from the CLI and from the console.
//
// It is a build gate rather than a report because the failure it prevents is
// gravitational. Features land in the API because that is where the work is, the
// CLI follows for the ones someone scripted, and the UI accumulates whatever was
// demoed. Six months later nobody can say what is reachable from where, and good
// intentions do not survive that — only something mechanical does.
//
// The check reads three sources, each of which is the *real* artifact rather
// than a description of it:
//
//   - `api/openapi.yaml` for the operations and their declared surfaces
//   - `mcpdoll __commands --json`, run against the built binary, for the CLI
//   - `web/src/routes.gen.ts`, generated from the router, for the console
//
// Reading the built binary and the generated manifest matters: a hand-maintained
// list of "commands we have" would drift from the commands users actually get,
// which is exactly the drift this tool exists to catch.
//
// See docs/adr/0004-api-first-tri-surface.md.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Exit codes, so CI can tell "the rule was broken" from "the tool could not
// run". A tool that cannot read its inputs must not report success.
const (
	exitOK        = 0
	exitViolation = 1
	exitToolError = 2
)

func main() {
	var (
		openapiPath = flag.String("openapi", "api/openapi.yaml", "the OpenAPI document")
		cliBin      = flag.String("cli-bin", "bin/mcpdoll", "the built CLI binary")
		routesPath  = flag.String("routes", "web/src/routes.gen.ts", "the generated route manifest")
		verbose     = flag.Bool("v", false, "list every operation and its surfaces")
	)
	flag.Parse()

	report, err := run(*openapiPath, *cliBin, *routesPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "paritycheck: %v\n", err)
		os.Exit(exitToolError)
	}

	report.print(os.Stdout, *verbose)
	if report.ok() {
		os.Exit(exitOK)
	}
	os.Exit(exitViolation)
}

// operation is one API operation and what covers it.
type operation struct {
	ID      string
	Method  string
	Path    string
	Summary string
	// DeclaredCLI and DeclaredUI come from `x-mcpdoll-surfaces` — what the spec
	// *claims*.
	DeclaredCLI string
	DeclaredUI  string
	// FoundCLI and FoundUI come from the binary and the route manifest — what
	// actually exists.
	FoundCLI string
	FoundUI  string
}

type report struct {
	Operations []operation

	// MissingCLI and MissingUI are operations with no covering surface.
	MissingCLI []operation
	MissingUI  []operation

	// OrphanCLI and OrphanUI are bindings naming an operation that does not
	// exist. This catches the renamed operationId whose CLI command still points
	// at the old id — otherwise a runtime 404 nobody notices until a user hits it.
	OrphanCLI map[string]string
	OrphanUI  map[string]string

	// UndeclaredSurfaces are operations whose spec does not declare where they
	// should be reachable from. Not a violation in itself, but it means the spec
	// stopped being the place to look.
	UndeclaredSurfaces []operation
}

func (r *report) ok() bool {
	return len(r.MissingCLI) == 0 && len(r.MissingUI) == 0 &&
		len(r.OrphanCLI) == 0 && len(r.OrphanUI) == 0 &&
		len(r.UndeclaredSurfaces) == 0
}

func run(openapiPath, cliBin, routesPath string) (*report, error) {
	ops, err := loadOperations(openapiPath)
	if err != nil {
		return nil, err
	}
	if len(ops) == 0 {
		return nil, fmt.Errorf("%s declares no operations; that is almost certainly a parse problem rather than an empty API", openapiPath)
	}

	cliOps, err := loadCLIOperations(cliBin)
	if err != nil {
		return nil, err
	}
	uiOps, err := loadUIOperations(routesPath)
	if err != nil {
		return nil, err
	}

	rep := &report{
		Operations: ops,
		OrphanCLI:  map[string]string{},
		OrphanUI:   map[string]string{},
	}

	known := make(map[string]bool, len(ops))
	for _, op := range ops {
		known[op.ID] = true
	}

	for i, op := range ops {
		if cmd, ok := cliOps[op.ID]; ok {
			ops[i].FoundCLI = cmd
		} else {
			rep.MissingCLI = append(rep.MissingCLI, op)
		}
		if route, ok := uiOps[op.ID]; ok {
			ops[i].FoundUI = route
		} else {
			rep.MissingUI = append(rep.MissingUI, op)
		}
		if op.DeclaredCLI == "" || op.DeclaredUI == "" {
			rep.UndeclaredSurfaces = append(rep.UndeclaredSurfaces, op)
		}
	}
	rep.Operations = ops

	for id, cmd := range cliOps {
		if !known[id] {
			rep.OrphanCLI[id] = cmd
		}
	}
	for id, route := range uiOps {
		if !known[id] {
			rep.OrphanUI[id] = route
		}
	}

	return rep, nil
}

// ------------------------------------------------------------------ openapi --

// specDoc is the subset of OpenAPI the check reads. Parsing only what is needed
// keeps the tool independent of the rest of the document's shape.
type specDoc struct {
	Paths map[string]map[string]specOperation `yaml:"paths"`
}

type specOperation struct {
	OperationID string       `yaml:"operationId"`
	Summary     string       `yaml:"summary"`
	Surfaces    specSurfaces `yaml:"x-mcpdoll-surfaces"`
}

type specSurfaces struct {
	CLI string `yaml:"cli"`
	UI  string `yaml:"ui"`
}

// httpMethods are the keys under a path item that are operations. Anything else
// there — `parameters`, `summary` — is not.
var httpMethods = map[string]bool{
	"get": true, "put": true, "post": true, "delete": true,
	"options": true, "head": true, "patch": true, "trace": true,
}

func loadOperations(path string) ([]operation, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var doc specDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	var ops []operation
	seen := map[string]string{}
	for p, item := range doc.Paths {
		for method, op := range item {
			if !httpMethods[strings.ToLower(method)] {
				continue
			}
			if op.OperationID == "" {
				return nil, fmt.Errorf(
					"%s %s has no operationId; the parity check keys on it, and so does every generator",
					strings.ToUpper(method), p)
			}
			if prior, dup := seen[op.OperationID]; dup {
				return nil, fmt.Errorf(
					"operationId %q is used by both %s and %s %s; ids must be unique",
					op.OperationID, prior, strings.ToUpper(method), p)
			}
			seen[op.OperationID] = strings.ToUpper(method) + " " + p

			ops = append(ops, operation{
				ID:          op.OperationID,
				Method:      strings.ToUpper(method),
				Path:        p,
				Summary:     op.Summary,
				DeclaredCLI: op.Surfaces.CLI,
				DeclaredUI:  op.Surfaces.UI,
			})
		}
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].ID < ops[j].ID })
	return ops, nil
}

// ---------------------------------------------------------------------- cli --

// commandTree mirrors the CLI's `__commands --json` output.
type commandTree struct {
	Commands []struct {
		Path      string `json:"path"`
		Operation string `json:"operation"`
		Hidden    bool   `json:"hidden"`
	} `json:"commands"`
}

func loadCLIOperations(bin string) (map[string]string, error) {
	if _, err := os.Stat(bin); err != nil {
		return nil, fmt.Errorf(
			"cannot find the CLI binary at %s: %w\n\nBuild it first: go build -o %s ./cmd/mcpdoll",
			bin, err, bin)
	}

	// Run the real binary rather than reflecting over source: the point is to
	// check the commands users actually get.
	cmd := exec.Command(bin, "__commands", "--json")
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		var exitErr *exec.ExitError
		if ok := asExitError(err, &exitErr); ok {
			stderr = string(exitErr.Stderr)
		}
		return nil, fmt.Errorf("running %s __commands: %w\n%s", bin, err, stderr)
	}

	var tree commandTree
	if err := json.Unmarshal(out, &tree); err != nil {
		return nil, fmt.Errorf("parsing the command tree from %s: %w", bin, err)
	}

	ops := map[string]string{}
	for _, c := range tree.Commands {
		if c.Operation == "" {
			continue
		}
		if prior, dup := ops[c.Operation]; dup {
			return nil, fmt.Errorf(
				"operation %q is claimed by two commands (%s and %s); one command per operation, "+
					"or the coverage report becomes ambiguous",
				c.Operation, prior, c.Path)
		}
		ops[c.Operation] = c.Path
	}
	return ops, nil
}

func asExitError(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

// ----------------------------------------------------------------------- ui --

// routeBinding matches an entry in the generated route manifest:
//
//	{ path: "/registry/servers", operation: "listServers" }
//
// The manifest is generated from the router, so it cannot describe a route that
// does not exist.
var routeBinding = regexp.MustCompile(
	`path:\s*"([^"]+)"[^}]*?operation:\s*"([^"]+)"`)

func loadUIOperations(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// A missing manifest is not a tool error — it is the honest state of
			// a console that has not been built. Report zero coverage so the
			// violation is about the UI rather than about the check.
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	ops := map[string]string{}
	for _, match := range routeBinding.FindAllStringSubmatch(string(raw), -1) {
		route, operation := match[1], match[2]
		if prior, dup := ops[operation]; dup {
			return nil, fmt.Errorf(
				"operation %q is claimed by two routes (%s and %s)", operation, prior, route)
		}
		ops[operation] = route
	}
	return ops, nil
}

// -------------------------------------------------------------------- report --

func (r *report) print(w *os.File, verbose bool) {
	if verbose || r.ok() {
		fmt.Fprintf(w, "%-28s %-34s %s\n", "OPERATION", "CLI", "UI")
		fmt.Fprintf(w, "%-28s %-34s %s\n", strings.Repeat("-", 28),
			strings.Repeat("-", 34), strings.Repeat("-", 34))
		for _, op := range r.Operations {
			fmt.Fprintf(w, "%-28s %-34s %s\n", op.ID, dash(op.FoundCLI), dash(op.FoundUI))
		}
		fmt.Fprintln(w)
	}

	total := len(r.Operations)
	withCLI := total - len(r.MissingCLI)
	withUI := total - len(r.MissingUI)
	fmt.Fprintf(w, "%d operation(s): %d with a CLI command, %d with a UI route\n",
		total, withCLI, withUI)

	if r.ok() {
		fmt.Fprintln(w, "\nparity: OK — every operation is reachable from all three surfaces")
		return
	}

	fmt.Fprintln(w, "\nparity: FAILED")

	if len(r.MissingCLI) > 0 {
		fmt.Fprintf(w, "\n%d operation(s) have no CLI command:\n", len(r.MissingCLI))
		for _, op := range r.MissingCLI {
			fmt.Fprintf(w, "  %-28s %s %s\n", op.ID, op.Method, op.Path)
			if op.DeclaredCLI != "" {
				fmt.Fprintf(w, "%s the spec says it should be %q\n", strings.Repeat(" ", 6), op.DeclaredCLI)
			}
		}
		fmt.Fprintln(w, "\n  Add the command, and annotate it:")
		fmt.Fprintln(w, `      Annotations: map[string]string{annotationOperation: "<operationId>"},`)
	}

	if len(r.MissingUI) > 0 {
		fmt.Fprintf(w, "\n%d operation(s) have no UI route:\n", len(r.MissingUI))
		for _, op := range r.MissingUI {
			fmt.Fprintf(w, "  %-28s %s %s\n", op.ID, op.Method, op.Path)
			if op.DeclaredUI != "" {
				fmt.Fprintf(w, "%s the spec says it should be %q\n", strings.Repeat(" ", 6), op.DeclaredUI)
			}
		}
		fmt.Fprintln(w, "\n  Add the route to the console's router; the manifest regenerates from it.")
	}

	if len(r.OrphanCLI) > 0 {
		fmt.Fprintf(w, "\n%d CLI command(s) name an operation that does not exist:\n", len(r.OrphanCLI))
		for _, id := range sortedKeys(r.OrphanCLI) {
			fmt.Fprintf(w, "  %-28s %s\n", id, r.OrphanCLI[id])
		}
		fmt.Fprintln(w, "\n  Usually a renamed operationId. Left alone this is a runtime 404")
		fmt.Fprintln(w, "  nobody notices until a user hits it.")
	}

	if len(r.OrphanUI) > 0 {
		fmt.Fprintf(w, "\n%d UI route(s) name an operation that does not exist:\n", len(r.OrphanUI))
		for _, id := range sortedKeys(r.OrphanUI) {
			fmt.Fprintf(w, "  %-28s %s\n", id, r.OrphanUI[id])
		}
	}

	if len(r.UndeclaredSurfaces) > 0 {
		fmt.Fprintf(w, "\n%d operation(s) do not declare their surfaces in the spec:\n",
			len(r.UndeclaredSurfaces))
		for _, op := range r.UndeclaredSurfaces {
			fmt.Fprintf(w, "  %-28s %s %s\n", op.ID, op.Method, op.Path)
		}
		fmt.Fprintln(w, "\n  Add x-mcpdoll-surfaces so the spec stays the place to look.")
	}
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
