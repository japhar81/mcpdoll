// Copyright 2026 Henry Zektser.

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// Env is the resolved invocation context: streams, output format, and whichever
// profile the flags and config file settled on.
type Env struct {
	Out io.Writer
	Err io.Writer

	Output     string
	ConfigFlag string
	Profile    string
	APIURL     string
	Project    string
	Quiet      bool

	configPath string
	config     *Config

	// Resolved during load and read by commands.
	gatewayURL   string
	adminURL     string
	tokenRef     string
	snapshotPath string
}

// Config is `~/.mcpdoll/config.yaml`.
type Config struct {
	// CurrentProfile is used when --profile is absent.
	CurrentProfile string             `yaml:"current_profile"`
	Profiles       map[string]Profile `yaml:"profiles"`
}

// Profile is one named target.
type Profile struct {
	APIURL string `yaml:"api_url"`
	// GatewayURL is the data plane, for the inspector commands.
	GatewayURL string `yaml:"gateway_url"`
	// AdminURL is the data plane's admin listener, which serves backend health
	// on a separate port because that report is an inventory of what is behind
	// the gateway.
	AdminURL string `yaml:"admin_url"`
	// SnapshotPath is the file the local data plane serves, so that
	// `mcpdoll snapshot current` has somewhere to look.
	SnapshotPath string `yaml:"snapshot_path"`
	Project      string `yaml:"project"`
	// TokenRef references a credential rather than holding one. A config file is
	// world-readable often enough that storing a bearer token in it is a bad
	// default; the value is read from the named environment variable.
	TokenRef string `yaml:"token_ref"`
}

// DefaultConfigPath is where the config lives when nothing overrides it.
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".mcpdoll/config.yaml"
	}
	return filepath.Join(home, ".mcpdoll", "config.yaml")
}

// load resolves flags, environment, and config into the Env.
//
// Precedence, highest first: flag, environment variable, profile, built-in
// default. That ordering is what makes a CI job able to override a developer's
// checked-in profile without editing it.
func (e *Env) load(cmd *cobra.Command) error {
	if err := e.validateOutput(); err != nil {
		return err
	}

	path := e.ConfigFlag
	if path == "" {
		path = e.configPath
	}
	if path == "" {
		path = os.Getenv("MCPDOLL_CLI_CONFIG")
	}
	if path == "" {
		path = DefaultConfigPath()
	}

	cfg, err := loadConfig(path)
	if err != nil {
		return err
	}
	e.config = cfg

	profileName := e.Profile
	if profileName == "" {
		profileName = os.Getenv("MCPDOLL_PROFILE")
	}
	if profileName == "" {
		profileName = cfg.CurrentProfile
	}

	var profile Profile
	if profileName != "" {
		p, ok := cfg.Profiles[profileName]
		if !ok {
			// Naming the available profiles turns a typo into a one-step fix.
			return usageError(fmt.Errorf("no profile %q in %s (available: %s)",
				profileName, path, profileNames(cfg)))
		}
		profile = p
	}

	if e.APIURL == "" {
		e.APIURL = firstNonEmpty(os.Getenv("MCPDOLL_API_URL"), profile.APIURL, "http://localhost:3001")
	}
	if e.Project == "" {
		e.Project = firstNonEmpty(os.Getenv("MCPDOLL_PROJECT"), profile.Project)
	}
	e.gatewayURL = firstNonEmpty(os.Getenv("MCPDOLL_GATEWAY_URL"), profile.GatewayURL, "http://localhost:8080")
	e.adminURL = firstNonEmpty(os.Getenv("MCPDOLL_ADMIN_URL"), profile.AdminURL, "http://localhost:8081")
	e.snapshotPath = firstNonEmpty(os.Getenv("MCPDOLL_SNAPSHOT_PATH"), profile.SnapshotPath, "snapshot.pb")
	e.tokenRef = profile.TokenRef

	_ = cmd
	return nil
}

func (e *Env) validateOutput() error {
	switch strings.ToLower(e.Output) {
	case "table", "json", "yaml":
		e.Output = strings.ToLower(e.Output)
		return nil
	default:
		return usageError(fmt.Errorf("--output %q is not one of table, json, yaml", e.Output))
	}
}

// GatewayURL is the data-plane base URL for inspector commands.
func (e *Env) GatewayURL() string { return e.gatewayURL }

// Token returns the bearer token from the profile's referenced environment
// variable, or "".
func (e *Env) Token() string {
	if e.tokenRef == "" {
		return os.Getenv("MCPDOLL_TOKEN")
	}
	return os.Getenv(e.tokenRef)
}

// Printf writes progress to stderr, so it never contaminates piped output.
//
// Progress on stdout is the classic reason `mcpdoll ... | jq` fails: the human
// sees something helpful and the pipeline sees a parse error.
func (e *Env) Printf(format string, args ...any) {
	if e.Quiet {
		return
	}
	fmt.Fprintf(e.Err, format, args...)
}

// Emit renders a result in the selected format.
func (e *Env) Emit(v any) error {
	switch e.Output {
	case "json":
		enc := json.NewEncoder(e.Out)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	case "yaml":
		enc := yaml.NewEncoder(e.Out)
		enc.SetIndent(2)
		if err := enc.Encode(v); err != nil {
			return err
		}
		return enc.Close()
	default:
		return e.emitTable(v)
	}
}

// emitTable renders a value for a terminal.
//
// Anything implementing [Tabular] chooses its own columns; everything else falls
// back to indented JSON, which is honest about the fact that no table was
// designed for it.
func (e *Env) emitTable(v any) error {
	if t, ok := v.(Tabular); ok {
		return writeTable(e.Out, t.Table())
	}
	enc := json.NewEncoder(e.Out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// Tabular is implemented by results with a designed table form.
type Tabular interface {
	Table() Table
}

// Table is a rendered table: a header row plus body rows.
type Table struct {
	// Title is printed above the table when non-empty.
	Title   string
	Columns []string
	Rows    [][]string
	// Notes are printed below, for the things a table cannot carry — warnings,
	// totals, next steps.
	Notes []string
}

func writeTable(w io.Writer, t Table) error {
	if t.Title != "" {
		fmt.Fprintf(w, "%s\n\n", t.Title)
	}
	if len(t.Rows) == 0 {
		fmt.Fprintln(w, "(no rows)")
	} else {
		widths := make([]int, len(t.Columns))
		for i, c := range t.Columns {
			widths[i] = len(c)
		}
		for _, row := range t.Rows {
			for i, cell := range row {
				if i < len(widths) && len(cell) > widths[i] {
					widths[i] = len(cell)
				}
			}
		}
		writeRow(w, t.Columns, widths)
		dividers := make([]string, len(t.Columns))
		for i := range dividers {
			dividers[i] = strings.Repeat("-", widths[i])
		}
		writeRow(w, dividers, widths)
		for _, row := range t.Rows {
			writeRow(w, row, widths)
		}
	}
	for _, note := range t.Notes {
		fmt.Fprintf(w, "\n%s", note)
	}
	if len(t.Notes) > 0 {
		fmt.Fprintln(w)
	}
	return nil
}

func writeRow(w io.Writer, cells []string, widths []int) {
	parts := make([]string, 0, len(cells))
	for i, cell := range cells {
		if i < len(widths) {
			parts = append(parts, fmt.Sprintf("%-*s", widths[i], cell))
		} else {
			parts = append(parts, cell)
		}
	}
	fmt.Fprintln(w, strings.TrimRight(strings.Join(parts, "  "), " "))
}

func loadConfig(path string) (*Config, error) {
	cfg := &Config{Profiles: map[string]Profile{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No config file is the normal first-run state, not an error.
			return cfg, nil
		}
		return nil, configError(fmt.Errorf("reading %s: %w", path, err))
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		// io.EOF means the file is empty, which is the same thing as absent and
		// is a state a `touch` produces. Only a *parse* failure is an error.
		return nil, configError(fmt.Errorf("parsing %s: %w", path, err))
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]Profile{}
	}
	return cfg, nil
}

func profileNames(cfg *Config) string {
	if len(cfg.Profiles) == 0 {
		return "none defined"
	}
	out := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		out = append(out, name)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
