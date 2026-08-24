// Copyright 2026 The MCPDoll Authors.

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDefaultIsValid(t *testing.T) {
	cfg := Default()
	require.NoError(t, cfg.Validate(), "the built-in defaults must themselves be valid")
}

func TestLoadNoFileUsesDefaults(t *testing.T) {
	cfg, err := Load("")
	require.NoError(t, err)
	require.Equal(t, Default(), cfg)
}

func TestLoadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcpdoll.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
env: production
log:
  level: warn
  format: text
dataplane:
  listen_addr: ":9999"
  snapshot_source: file
  snapshot_path: /var/lib/mcpdoll/snapshot.pb
  revocations_path: /var/lib/mcpdoll/revocations.pb
  trusted_signing_keys:
    - AAAA
    - BBBB
pipeline:
  total_budget: 500ms
  hook_budget: 100ms
`), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, "production", cfg.Env)
	require.Equal(t, "warn", cfg.Log.Level)
	require.Equal(t, ":9999", cfg.DataPlane.ListenAddr)
	require.Equal(t, []string{"AAAA", "BBBB"}, cfg.DataPlane.TrustedSigningKeys)
	require.Equal(t, 500*time.Millisecond, cfg.Pipeline.TotalBudget)
	// Unmentioned fields keep their defaults.
	require.Equal(t, int32(16), cfg.Database.MaxConns)
}

// TestLoadFileRejectsUnknownKey: a typo in a config file must fail at startup.
// Silently ignoring it is how an operator ends up believing a setting is in
// effect when it never was.
func TestLoadFileRejectsUnknownKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	require.NoError(t, os.WriteFile(path, []byte("dataplane:\n  listen_adr: \":1\"\n"), 0o600))
	_, err := Load(path)
	require.ErrorContains(t, err, "listen_adr")
}

func TestLoadFileMissing(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	require.ErrorContains(t, err, "reading")
}

// TestEnvOverrides walks each supported field kind, since the mapping is
// derived by reflection and a broken case would be silent.
func TestEnvOverrides(t *testing.T) {
	t.Setenv("MCPDOLL_ENV", "staging")                                // string
	t.Setenv("MCPDOLL_LOG_LEVEL", "debug")                            // nested string
	t.Setenv("MCPDOLL_DATABASE_MAX_CONNS", "64")                      // int32
	t.Setenv("MCPDOLL_TELEMETRY_SAMPLE_RATIO", "0.25")                // float
	t.Setenv("MCPDOLL_ADMISSION_REQUIRE_SEPARATE_APPROVER", "false")  // bool
	t.Setenv("MCPDOLL_PIPELINE_TOTAL_BUDGET", "1s")                   // duration
	t.Setenv("MCPDOLL_DATAPLANE_TRUSTED_SIGNING_KEYS", "K1, K2 ,,K3") // string slice

	cfg, err := Load("")
	require.NoError(t, err)
	require.Equal(t, "staging", cfg.Env)
	require.Equal(t, "debug", cfg.Log.Level)
	require.Equal(t, int32(64), cfg.Database.MaxConns)
	require.InDelta(t, 0.25, cfg.Telemetry.SampleRatio, 1e-9)
	require.False(t, cfg.Admission.RequireSeparateApprover)
	require.Equal(t, time.Second, cfg.Pipeline.TotalBudget)
	require.Equal(t, []string{"K1", "K2", "K3"}, cfg.DataPlane.TrustedSigningKeys,
		"blank entries from a trailing comma must be dropped, not become an empty trusted key")
}

func TestEnvOverridesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	require.NoError(t, os.WriteFile(path, []byte("env: fromfile\n"), 0o600))
	t.Setenv("MCPDOLL_ENV", "fromenv")
	cfg, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, "fromenv", cfg.Env)
}

func TestEnvParseErrors(t *testing.T) {
	tests := []struct{ key, val, want string }{
		{"MCPDOLL_DATABASE_MAX_CONNS", "lots", "not an integer"},
		{"MCPDOLL_TELEMETRY_SAMPLE_RATIO", "half", "not a number"},
		{"MCPDOLL_GUARD_ENABLED", "maybe", "not a boolean"},
		{"MCPDOLL_PIPELINE_TOTAL_BUDGET", "soon", "not a duration"},
		{"MCPDOLL_DATABASE_MAX_CONNS", "99999999999", "overflows"},
	}
	for _, tc := range tests {
		t.Run(tc.key+"="+tc.val, func(t *testing.T) {
			t.Setenv(tc.key, tc.val)
			_, err := Load("")
			require.ErrorContains(t, err, tc.want)
		})
	}
}

// TestValidateRejectsFloatingModelAlias is the startup error the brief
// requires. A floating alias would make the guard's verdict cache key and its
// audit records both untrue.
func TestValidateRejectsFloatingModelAlias(t *testing.T) {
	unpinned := []string{
		"claude-sonnet-latest",
		"claude-opus-4-latest",
		"gpt-4o:latest",
		"models/gemini-stable",
		"some-model-preview",
		"latest",
		"my-model-current",
		"vendor/model-default",
	}
	for _, model := range unpinned {
		t.Run(model, func(t *testing.T) {
			cfg := Default()
			cfg.Guard = GuardConfig{
				Enabled:         true,
				Endpoint:        "localhost:50051",
				FastModel:       model,
				EscalationModel: "claude-opus-5-20260115",
				PromptVersion:   "v1",
				PolicyVersion:   "v1",
				EscalateBelow:   0.85,
				EscalateAbove:   0.15,
			}
			err := cfg.Validate()
			require.ErrorContains(t, err, "floating alias")
			require.ErrorContains(t, err, "guard.fast_model")
		})
	}
}

func TestValidateAcceptsPinnedModels(t *testing.T) {
	pinned := []string{
		"claude-haiku-4-5-20251001",
		"claude-opus-5-20260115",
		"gpt-4o-2024-11-20",
		// A pinned id that merely contains the letters of an alias must pass.
		"vendor-currency-classifier-v3-20260101",
	}
	for _, model := range pinned {
		t.Run(model, func(t *testing.T) {
			cfg := Default()
			cfg.Guard = GuardConfig{
				Enabled:         true,
				Endpoint:        "localhost:50051",
				FastModel:       model,
				EscalationModel: model,
				PromptVersion:   "v1",
				PolicyVersion:   "v1",
				EscalateBelow:   0.85,
				EscalateAbove:   0.15,
			}
			require.NoError(t, cfg.Validate(), "%q should be accepted as pinned", model)
		})
	}
}

// TestValidateGuardDisabledSkipsModelChecks: the guard ships disabled by
// default, and a default config must not be rejected for lacking a model.
func TestValidateGuardDisabledSkipsModelChecks(t *testing.T) {
	cfg := Default()
	require.False(t, cfg.Guard.Enabled)
	require.Empty(t, cfg.Guard.FastModel)
	require.NoError(t, cfg.Validate())
}

func TestValidateCrossFieldInvariants(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name:    "hook budget cannot exceed total budget",
			mutate:  func(c *Config) { c.Pipeline.HookBudget = 2 * c.Pipeline.TotalBudget },
			wantErr: "exceeds pipeline.total_budget",
		},
		{
			name:    "degraded TTL must be shorter than the normal TTL",
			mutate:  func(c *Config) { c.Snapshot.DegradedCatalogTTL = 2 * c.Snapshot.CatalogTTL },
			wantErr: "exceeds snapshot.catalog_ttl",
		},
		{
			name:    "probe timeout must be shorter than the probe interval",
			mutate:  func(c *Config) { c.Health.ProbeTimeout = c.Health.ProbeInterval },
			wantErr: "probes overlap",
		},
		{
			name:    "file snapshot source needs a path",
			mutate:  func(c *Config) { c.DataPlane.SnapshotSource = "file" },
			wantErr: "snapshot_path is required",
		},
		{
			name:    "grpc snapshot source needs an address",
			mutate:  func(c *Config) { c.DataPlane.ControlPlaneCP = "" },
			wantErr: "control_plane_addr is required",
		},
		{
			name:    "unknown snapshot source",
			mutate:  func(c *Config) { c.DataPlane.SnapshotSource = "carrier-pigeon" },
			wantErr: `must be "grpc" or "file"`,
		},
		{
			name:    "sample ratio out of range",
			mutate:  func(c *Config) { c.Telemetry.SampleRatio = 1.5 },
			wantErr: "must be in [0,1]",
		},
		{
			name:    "bad log level",
			mutate:  func(c *Config) { c.Log.Level = "chatty" },
			wantErr: "log.level",
		},
		{
			name:    "bad log format",
			mutate:  func(c *Config) { c.Log.Format = "xml" },
			wantErr: "log.format",
		},
		{
			name:    "ewma alpha out of range",
			mutate:  func(c *Config) { c.Health.EWMAAlpha = 0 },
			wantErr: "ewma_alpha",
		},
		{
			name:    "snapshot history must retain at least one",
			mutate:  func(c *Config) { c.DataPlane.SnapshotHistory = 0 },
			wantErr: "snapshot_history",
		},
		{
			name:    "tool name budget bounded",
			mutate:  func(c *Config) { c.Admission.MaxToolNameLength = 500 },
			wantErr: "max_tool_name_length",
		},
		{
			name: "escalation band must be non-empty",
			mutate: func(c *Config) {
				c.Guard = GuardConfig{
					Enabled: true, Endpoint: "x:1",
					FastModel: "m-20260101", EscalationModel: "m-20260101",
					PromptVersion: "v1", PolicyVersion: "v1",
					EscalateBelow: 0.2, EscalateAbove: 0.8,
				}
			},
			wantErr: "must be below guard.escalate_below",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			err := cfg.Validate()
			require.Error(t, err)
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

// TestValidateReportsEveryProblem: an operator fixing configuration one error
// per restart is a bad afternoon. Report them all at once.
func TestValidateReportsEveryProblem(t *testing.T) {
	cfg := Default()
	cfg.Log.Format = "xml"
	cfg.Log.Level = "chatty"
	cfg.Telemetry.SampleRatio = 9
	cfg.Health.EWMAAlpha = 0
	err := cfg.Validate()
	require.Error(t, err)
	require.ErrorContains(t, err, "4 problem(s)")
	for _, want := range []string{"log.format", "log.level", "sample_ratio", "ewma_alpha"} {
		require.ErrorContains(t, err, want)
	}
}

func TestParseLevel(t *testing.T) {
	for _, name := range []string{"debug", "INFO", "Warn", "warning", "error", ""} {
		_, err := ParseLevel(name)
		require.NoError(t, err, "level %q", name)
	}
	_, err := ParseLevel("verbose")
	require.ErrorContains(t, err, "must be one of")
}

// TestProductionRequiresARevocationPath: without one, revoking a credential
// takes effect at the next snapshot rather than immediately — which may be
// never, if nobody publishes.
//
// A startup error rather than a warning, for the same reason `--allow-anonymous`
// has to be typed: the unsafe state must not be reachable by omission. A
// deployment that genuinely wants snapshot-latency revocation can say so by
// running as staging, which is a thing somebody has to choose.
func TestProductionRequiresARevocationPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prod.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
env: production
dataplane:
  snapshot_source: file
  snapshot_path: /var/lib/mcpdoll/snapshot.pb
  trusted_signing_keys: [AAAA]
`), 0o600))

	_, err := Load(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "revocations_path")
	require.Contains(t, err.Error(), "keeps working until the next snapshot")
}

// TestDevelopmentDoesNotRequireOne: `make dev` must not be a configuration
// exercise, and a developer poking at a gateway is not the threat model.
func TestDevelopmentDoesNotRequireOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
env: development
dataplane:
  snapshot_source: file
  snapshot_path: /tmp/snapshot.pb
  trusted_signing_keys: [AAAA]
`), 0o600))

	_, err := Load(path)
	require.NoError(t, err)
}
