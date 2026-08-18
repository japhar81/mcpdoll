// Copyright 2026 The MCPDoll Authors.

// Package config loads and validates MCPDoll's process configuration.
//
// Precedence, lowest to highest: built-in defaults, an optional YAML file, then
// environment variables prefixed `MCPDOLL_`. Nested keys map to underscores, so
// `dataplane.listen_addr` is `MCPDOLL_DATAPLANE_LISTEN_ADDR`.
//
// Validation is strict and happens at startup, not at first use. A gateway that
// boots with a bad configuration and only discovers it when the first request
// arrives has turned a deployment error into an outage.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the whole process configuration. Both binaries read the same file;
// each validates only the sections it uses.
type Config struct {
	Env          string          `yaml:"env"`
	Log          Log             `yaml:"log"`
	Telemetry    Telemetry       `yaml:"telemetry"`
	Database     Database        `yaml:"database"`
	Redis        Redis           `yaml:"redis"`
	DataPlane    DataPlane       `yaml:"dataplane"`
	ControlPlane ControlPlane    `yaml:"controlplane"`
	Snapshot     SnapshotConfig  `yaml:"snapshot"`
	Pipeline     PipelineConfig  `yaml:"pipeline"`
	Guard        GuardConfig     `yaml:"guard"`
	Admission    AdmissionConfig `yaml:"admission"`
	Health       HealthConfig    `yaml:"health"`
}

type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

type Telemetry struct {
	// OTLPEndpoint is the collector's HTTP endpoint, e.g. http://localhost:4318.
	// Empty disables export; spans are still created so trace ids exist for log
	// correlation, they simply go nowhere.
	OTLPEndpoint string  `yaml:"otlp_endpoint"`
	ServiceName  string  `yaml:"service_name"`
	SampleRatio  float64 `yaml:"sample_ratio"`
}

type Database struct {
	URL      string `yaml:"url"`
	MaxConns int32  `yaml:"max_conns"`
	MinConns int32  `yaml:"min_conns"`
}

type Redis struct {
	URL string `yaml:"url"`
}

type DataPlane struct {
	ListenAddr string `yaml:"listen_addr"`
	AdminAddr  string `yaml:"admin_addr"`
	// SnapshotSource is "grpc" (stream from the control plane) or "file"
	// (load from disk — used by tests and by air-gapped deployments).
	SnapshotSource string `yaml:"snapshot_source"`
	SnapshotPath   string `yaml:"snapshot_path"`
	ControlPlaneCP string `yaml:"control_plane_addr"`
	// TrustedSigningKeys are the base64 Ed25519 public keys a snapshot may be
	// signed with. More than one so a key rotation does not need lockstep
	// restarts.
	TrustedSigningKeys []string `yaml:"trusted_signing_keys"`
	// SnapshotHistory is how many previous snapshots to retain for local
	// rollback without contacting the control plane.
	SnapshotHistory int `yaml:"snapshot_history"`
}

type ControlPlane struct {
	ListenAddr     string `yaml:"listen_addr"`
	SnapshotAddr   string `yaml:"snapshot_addr"`
	SigningKeyPath string `yaml:"signing_key_path"`
	// SigningKeyID is recorded in every snapshot this control plane signs, and
	// is how a verifier selects the right public key during a rotation.
	SigningKeyID string `yaml:"signing_key_id"`
	// KeyDir is where generateSigningKey writes new keypairs. Empty means the
	// control plane will not mint keys, which is a reasonable posture for
	// anything but a development stack.
	KeyDir string `yaml:"key_dir"`
	// RegistryPath is the document the API serves.
	RegistryPath string `yaml:"registry_path"`
	// GatewayURL is the data plane the gateway inspection operations reach.
	GatewayURL string `yaml:"gateway_url"`
	// APIToken is the bearer credential the API requires. Empty is a startup
	// error unless --allow-anonymous is passed: an API that can mint signing
	// keys must not become reachable by leaving a line out of a config file.
	APIToken string `yaml:"api_token"`
	// AllowedOrigins are the browser origins permitted to call the API. No
	// wildcard is accepted; the console's origin is named explicitly.
	AllowedOrigins []string `yaml:"allowed_origins"`
}

type SnapshotConfig struct {
	// CatalogTTL is the ceiling on the `ttlMs` the edge advertises for list
	// results. Per-bundle and per-policy values may lower it, never raise it.
	CatalogTTL time.Duration `yaml:"catalog_ttl"`
	// DegradedCatalogTTL is the shortened TTL used when the catalog is being
	// served from last-known-good because a backend is unreachable, so clients
	// re-ask sooner.
	DegradedCatalogTTL time.Duration `yaml:"degraded_catalog_ttl"`
}

type PipelineConfig struct {
	// TotalBudget caps the wall-clock time all plugins together may consume
	// for one request. Exceeding it is a budget exhaustion, recorded in the
	// audit trail, not a request failure.
	TotalBudget time.Duration `yaml:"total_budget"`
	// HookBudget is the default per-hook deadline when a manifest omits one.
	HookBudget time.Duration `yaml:"hook_budget"`
	// CircuitFailureThreshold is consecutive plugin failures before its
	// breaker opens.
	CircuitFailureThreshold int           `yaml:"circuit_failure_threshold"`
	CircuitCooldown         time.Duration `yaml:"circuit_cooldown"`
}

type GuardConfig struct {
	Enabled bool `yaml:"enabled"`
	// Endpoint of the gRPC guard plugin.
	Endpoint string `yaml:"endpoint"`
	// FastModel and EscalationModel must be pinned model ids. A floating alias
	// is a startup error — see Validate.
	FastModel       string `yaml:"fast_model"`
	EscalationModel string `yaml:"escalation_model"`
	PromptVersion   string `yaml:"prompt_version"`
	PolicyVersion   string `yaml:"policy_version"`
	// Confidence band within which the fast model's verdict is not trusted and
	// the larger model is consulted.
	EscalateBelow float64 `yaml:"escalate_below"`
	EscalateAbove float64 `yaml:"escalate_above"`
}

type AdmissionConfig struct {
	MaxToolsPerServer int `yaml:"max_tools_per_server"`
	// MaxToolNameLength is the total budget for `<prefix>.<tool>`, which the
	// MCP spec bounds and which admission enforces so a collision or overflow
	// is caught before publish rather than at serve time.
	MaxToolNameLength int `yaml:"max_tool_name_length"`
	// MaxTokensPerDefinition budgets the *serialized* definition — input
	// schema included, not just the description, because the schema is usually
	// the larger half of what the model has to read.
	MaxTokensPerDefinition int `yaml:"max_tokens_per_definition"`
	MaxTokensPerBundle     int `yaml:"max_tokens_per_bundle"`
	// RequireSeparateApprover enforces publisher != approver.
	RequireSeparateApprover bool `yaml:"require_separate_approver"`
}

type HealthConfig struct {
	ProbeInterval time.Duration `yaml:"probe_interval"`
	ProbeTimeout  time.Duration `yaml:"probe_timeout"`
	// GraceWindow is how long a tool from an unreachable backend stays listed
	// (failing its calls fast with a legible error) before removal. Dropping a
	// tool from the catalog invalidates every client's prompt cache, so the
	// grace window trades a little staleness for a lot of cache stability.
	GraceWindow time.Duration `yaml:"grace_window"`
	// EjectAfterFailures is consecutive invocation failures before ejection.
	EjectAfterFailures int           `yaml:"eject_after_failures"`
	EWMAAlpha          float64       `yaml:"ewma_alpha"`
	DriftScanInterval  time.Duration `yaml:"drift_scan_interval"`
}

// Default returns the built-in configuration. Every field must be set here:
// a zero value that only fails at first use is exactly the failure mode
// startup validation exists to prevent.
func Default() Config {
	return Config{
		Env: "development",
		Log: Log{Level: "info", Format: "json"},
		Telemetry: Telemetry{
			OTLPEndpoint: "http://localhost:4318",
			ServiceName:  "mcpdoll",
			SampleRatio:  1.0,
		},
		Database: Database{
			URL:      "postgres://mcpdoll:mcpdoll@localhost:5432/mcpdoll?sslmode=disable",
			MaxConns: 16,
			MinConns: 2,
		},
		Redis: Redis{URL: "redis://localhost:6379/0"},
		DataPlane: DataPlane{
			ListenAddr:      ":8080",
			AdminAddr:       ":8081",
			SnapshotSource:  "grpc",
			ControlPlaneCP:  "localhost:9090",
			SnapshotHistory: 5,
		},
		ControlPlane: ControlPlane{
			ListenAddr:   ":3001",
			SnapshotAddr: ":9090",
		},
		Snapshot: SnapshotConfig{
			CatalogTTL:         5 * time.Minute,
			DegradedCatalogTTL: 30 * time.Second,
		},
		Pipeline: PipelineConfig{
			TotalBudget:             250 * time.Millisecond,
			HookBudget:              50 * time.Millisecond,
			CircuitFailureThreshold: 5,
			CircuitCooldown:         30 * time.Second,
		},
		Guard: GuardConfig{
			Enabled:       false,
			PromptVersion: "v1",
			PolicyVersion: "v1",
			EscalateBelow: 0.85,
			EscalateAbove: 0.15,
		},
		Admission: AdmissionConfig{
			MaxToolsPerServer:       64,
			MaxToolNameLength:       64,
			MaxTokensPerDefinition:  1500,
			MaxTokensPerBundle:      40000,
			RequireSeparateApprover: true,
		},
		Health: HealthConfig{
			ProbeInterval:      30 * time.Second,
			ProbeTimeout:       5 * time.Second,
			GraceWindow:        10 * time.Minute,
			EjectAfterFailures: 5,
			EWMAAlpha:          0.2,
			DriftScanInterval:  5 * time.Minute,
		},
	}
}

// Load reads defaults, then the file at path if non-empty, then the
// environment, and validates the result.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("config: reading %s: %w", path, err)
		}
		// KnownFields makes a typo in the config file a startup error instead
		// of a silently ignored setting.
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil {
			return Config{}, fmt.Errorf("config: parsing %s: %w", path, err)
		}
	}
	if err := applyEnv(&cfg); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// floatingModelAliases are model identifiers that do not pin a version.
//
// Pointing the guard at one of these is a startup error, not a warning: the
// guard's verdict cache is keyed on the model version, and every audit record
// claims which model produced the verdict. A floating alias silently
// invalidates both — the cache would serve verdicts from a model that no longer
// exists, and the audit trail would be a record of something untrue.
var floatingModelAliases = []string{
	"latest", "stable", "current", "preview", "default",
}

// Validate checks cross-field invariants.
func (c *Config) Validate() error {
	var errs []string

	switch c.Log.Format {
	case "json", "text":
	default:
		errs = append(errs, fmt.Sprintf("log.format %q must be \"json\" or \"text\"", c.Log.Format))
	}
	if _, err := ParseLevel(c.Log.Level); err != nil {
		errs = append(errs, err.Error())
	}

	if c.Telemetry.SampleRatio < 0 || c.Telemetry.SampleRatio > 1 {
		errs = append(errs, fmt.Sprintf("telemetry.sample_ratio %v must be in [0,1]", c.Telemetry.SampleRatio))
	}

	switch c.DataPlane.SnapshotSource {
	case "grpc":
		if c.DataPlane.ControlPlaneCP == "" {
			errs = append(errs, "dataplane.control_plane_addr is required when snapshot_source is \"grpc\"")
		}
	case "file":
		if c.DataPlane.SnapshotPath == "" {
			errs = append(errs, "dataplane.snapshot_path is required when snapshot_source is \"file\"")
		}
	default:
		errs = append(errs, fmt.Sprintf("dataplane.snapshot_source %q must be \"grpc\" or \"file\"", c.DataPlane.SnapshotSource))
	}
	if c.DataPlane.SnapshotHistory < 1 {
		errs = append(errs, "dataplane.snapshot_history must be at least 1")
	}

	if c.Pipeline.TotalBudget <= 0 {
		errs = append(errs, "pipeline.total_budget must be positive")
	}
	if c.Pipeline.HookBudget <= 0 {
		errs = append(errs, "pipeline.hook_budget must be positive")
	}
	if c.Pipeline.HookBudget > c.Pipeline.TotalBudget {
		errs = append(errs, fmt.Sprintf(
			"pipeline.hook_budget (%s) exceeds pipeline.total_budget (%s): a single hook could consume the whole request budget",
			c.Pipeline.HookBudget, c.Pipeline.TotalBudget))
	}
	if c.Pipeline.CircuitFailureThreshold < 1 {
		errs = append(errs, "pipeline.circuit_failure_threshold must be at least 1")
	}

	if c.Guard.Enabled {
		if c.Guard.Endpoint == "" {
			errs = append(errs, "guard.endpoint is required when the guard is enabled")
		}
		for field, model := range map[string]string{
			"guard.fast_model":       c.Guard.FastModel,
			"guard.escalation_model": c.Guard.EscalationModel,
		} {
			if model == "" {
				errs = append(errs, field+" is required when the guard is enabled")
				continue
			}
			if alias := floatingAlias(model); alias != "" {
				errs = append(errs, fmt.Sprintf(
					"%s is %q, which contains the floating alias %q; pin an exact model version so the verdict cache key and the audit record stay truthful",
					field, model, alias))
			}
		}
		if c.Guard.PromptVersion == "" {
			errs = append(errs, "guard.prompt_version is required when the guard is enabled")
		}
		if c.Guard.PolicyVersion == "" {
			errs = append(errs, "guard.policy_version is required when the guard is enabled")
		}
		if c.Guard.EscalateAbove >= c.Guard.EscalateBelow {
			errs = append(errs, fmt.Sprintf(
				"guard.escalate_above (%v) must be below guard.escalate_below (%v): the band between them is what escalates",
				c.Guard.EscalateAbove, c.Guard.EscalateBelow))
		}
	}

	if c.Admission.MaxToolNameLength < 1 || c.Admission.MaxToolNameLength > 128 {
		errs = append(errs, "admission.max_tool_name_length must be in [1,128]")
	}
	if c.Admission.MaxToolsPerServer < 1 {
		errs = append(errs, "admission.max_tools_per_server must be at least 1")
	}
	if c.Admission.MaxTokensPerDefinition < 1 {
		errs = append(errs, "admission.max_tokens_per_definition must be at least 1")
	}

	if c.Snapshot.DegradedCatalogTTL > c.Snapshot.CatalogTTL {
		errs = append(errs, fmt.Sprintf(
			"snapshot.degraded_catalog_ttl (%s) exceeds snapshot.catalog_ttl (%s): the degraded TTL must be shorter so clients re-ask sooner, not later",
			c.Snapshot.DegradedCatalogTTL, c.Snapshot.CatalogTTL))
	}

	if c.Health.EWMAAlpha <= 0 || c.Health.EWMAAlpha > 1 {
		errs = append(errs, fmt.Sprintf("health.ewma_alpha %v must be in (0,1]", c.Health.EWMAAlpha))
	}
	if c.Health.ProbeTimeout >= c.Health.ProbeInterval {
		errs = append(errs, fmt.Sprintf(
			"health.probe_timeout (%s) must be shorter than health.probe_interval (%s) or probes overlap",
			c.Health.ProbeTimeout, c.Health.ProbeInterval))
	}

	if len(errs) > 0 {
		return fmt.Errorf("config: %d problem(s):\n  - %s", len(errs), strings.Join(errs, "\n  - "))
	}
	return nil
}

// floatingAlias returns the offending alias if model looks unpinned.
func floatingAlias(model string) string {
	lower := strings.ToLower(model)
	for _, alias := range floatingModelAliases {
		// Match as a hyphen/colon-delimited segment so a legitimate id that
		// merely contains the letters (say a model named "…-currency-…") is not
		// rejected.
		for _, form := range []string{"-" + alias, ":" + alias, "/" + alias} {
			if strings.HasSuffix(lower, form) || strings.Contains(lower, form+"-") {
				return alias
			}
		}
		if lower == alias {
			return alias
		}
	}
	return ""
}
