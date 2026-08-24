// Copyright 2026 Henry Zektser.

package plugins_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mcpdoll/mcpdoll/internal/dataplane/pipeline"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/plugins"
	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// These tests compile the real first-party plugin to WebAssembly and run it
// through the real wazero host. Nothing is stubbed.
//
// That is deliberate and it is not cheap — the build takes a few seconds — but
// the ABI is exactly the kind of thing a stub would agree with and reality would
// not. Buffer pinning is the case in point: a mocked host would never have caught
// the collector reclaiming a payload mid-call, because a mock has no collector.

// buildPlugin compiles a plugin to WASM once per test binary and returns its
// path and digest.
func buildPlugin(t *testing.T, pkg string) (path, digest string) {
	t.Helper()
	pluginBuildOnce(t)

	path = filepath.Join(pluginBuildDir, filepath.Base(pkg)+".wasm")
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "plugin %s was not built", pkg)
	sum := sha256.Sum256(raw)
	return path, "sha256:" + hex.EncodeToString(sum[:])
}

var (
	pluginBuildDir  string
	pluginBuildErr  error
	pluginBuildSync sync.Once
)

func pluginBuildOnce(t *testing.T) {
	t.Helper()
	pluginBuildSync.Do(func() {
		dir, err := os.MkdirTemp("", "mcpdoll-plugins-*")
		if err != nil {
			pluginBuildErr = err
			return
		}
		pluginBuildDir = dir

		root, err := repoRoot()
		if err != nil {
			pluginBuildErr = err
			return
		}
		for _, pkg := range []string{"redact", "entitlements"} {
			out := filepath.Join(dir, pkg+".wasm")
			cmd := exec.Command("go", "build",
				"-buildmode=c-shared", "-o", out, "./plugins/"+pkg)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
			if combined, err := cmd.CombinedOutput(); err != nil {
				pluginBuildErr = fmt.Errorf("building %s: %w\n%s", pkg, err, combined)
				return
			}
		}
	})
	require.NoError(t, pluginBuildErr)
}

func repoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	// internal/dataplane/plugins -> repo root
	return filepath.Abs(filepath.Join(wd, "..", "..", ".."))
}

func newHost(t *testing.T) *plugins.WASMHost {
	t.Helper()
	host, err := plugins.NewWASMHost(context.Background(), plugins.WASMOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, host.Close()) })
	return host
}

func redactManifest(t *testing.T, config map[string]any) *snapshotpb.PluginManifest {
	t.Helper()
	path, digest := buildPlugin(t, "redact")
	manifest := &snapshotpb.PluginManifest{
		Id:      "plg_redact",
		Name:    "redact",
		Version: "1.0.0",
		Runtime: snapshotpb.PluginRuntime_PLUGIN_RUNTIME_WASM,
		Hooks:   []snapshotpb.Hook{snapshotpb.Hook_HOOK_ON_TOOL_RESULT},
		Reads:   []string{"result"},
		Writes:  []string{"result.content"},
		Rollout: snapshotpb.RolloutState_ROLLOUT_STATE_ENFORCE,

		ArtifactRef:    "file://" + path,
		ArtifactDigest: digest,
	}
	if config != nil {
		raw, err := json.Marshal(config)
		require.NoError(t, err)
		manifest.ConfigJson = string(raw)
	}
	return manifest
}

func invoke(t *testing.T, host *plugins.WASMHost, manifest *snapshotpb.PluginManifest, payload any) *pipeline.Verdict {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	verdict, err := host.Invoke(ctx, &pipeline.Invocation{
		Manifest: manifest,
		Hook:     snapshotpb.Hook_HOOK_ON_TOOL_RESULT,
		Context:  raw,
	})
	require.NoError(t, err)
	return verdict
}

// TestWASMHostRunsRealPlugin is the end-to-end proof that the ABI works.
func TestWASMHostRunsRealPlugin(t *testing.T) {
	host := newHost(t)
	manifest := redactManifest(t, nil)
	require.NoError(t, host.Load(context.Background(), manifest))
	require.Equal(t, []string{"plg_redact"}, host.LoadedPlugins())

	verdict := invoke(t, host, manifest, map[string]any{
		"hook": "on_tool_result",
		"result": map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "customer paid with 4111 1111 1111 1111 last Tuesday"},
			},
		},
	})

	require.Equal(t, pipeline.Decision("mutate"), verdict.Decision)
	require.Contains(t, verdict.Reason, "card_number")
	require.NotEmpty(t, verdict.Patch)

	// The patch replaces the text and nothing else.
	var ops []map[string]any
	require.NoError(t, json.Unmarshal(verdict.Patch, &ops))
	require.Len(t, ops, 1)
	require.Equal(t, "replace", ops[0]["op"])
	require.Equal(t, "/result/content/0/text", ops[0]["path"])
	require.Contains(t, ops[0]["value"], "[REDACTED]")
	require.NotContains(t, ops[0]["value"], "4111")
	require.Contains(t, ops[0]["value"], "last Tuesday",
		"redaction must remove the secret and nothing else")
}

// TestWASMPluginRedactsEachBuiltinShape exercises every built-in pattern through
// the real guest.
func TestWASMPluginRedactsEachBuiltinShape(t *testing.T) {
	host := newHost(t)
	manifest := redactManifest(t, nil)
	require.NoError(t, host.Load(context.Background(), manifest))

	tests := []struct{ name, text, secret string }{
		{"card number", "card 4111 1111 1111 1111 on file", "4111 1111 1111 1111"},
		{"ssn", "ssn 123-45-6789 recorded", "123-45-6789"},
		{"jwt", "token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTYifQ.abcdefghijklmnop here",
			"eyJhbGciOiJIUzI1NiJ9"},
		{"bearer", "Authorization: Bearer abcdefghijklmnopqrstuvwxyz012345",
			"abcdefghijklmnopqrstuvwxyz012345"},
		{"api key", "key sk-abcdefghijklmnopqrstuvwx set", "sk-abcdefghijklmnopqrstuvwx"},
		{"aws key", "AKIAIOSFODNN7EXAMPLE is the id", "AKIAIOSFODNN7EXAMPLE"},
		{"private key", "-----BEGIN RSA PRIVATE KEY----- MIIE", "-----BEGIN RSA PRIVATE KEY-----"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			verdict := invoke(t, host, manifest, map[string]any{
				"hook": "on_tool_result",
				"result": map[string]any{
					"content": []map[string]any{{"type": "text", "text": tc.text}},
				},
			})
			require.Equal(t, pipeline.Decision("mutate"), verdict.Decision,
				"%s should have been redacted", tc.name)

			var ops []map[string]any
			require.NoError(t, json.Unmarshal(verdict.Patch, &ops))
			require.NotContains(t, ops[0]["value"], tc.secret)
		})
	}
}

// TestWASMPluginLeavesOrdinaryTextAlone is the false-positive half. A redaction
// plugin that mangles legitimate content is one an operator disables, at which
// point it protects nothing.
func TestWASMPluginLeavesOrdinaryTextAlone(t *testing.T) {
	host := newHost(t)
	manifest := redactManifest(t, nil)
	require.NoError(t, host.Load(context.Background(), manifest))

	unchanged := []string{
		"customer cus_42: Acme Corp, tier gold, opened 2024-03-11",
		"order 1234567890123456 shipped",
		"2 open tickets: #4471 (shipping delay), #4489 (invoice query)",
		"the invoice total is 1111.11 and the reference is 2024-03-11",
		"employee 123456789 started on 2021-06-01",
		"sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"see https://example.com/docs/bearer-tokens for details",
	}
	for _, text := range unchanged {
		t.Run(text[:min(30, len(text))], func(t *testing.T) {
			verdict := invoke(t, host, manifest, map[string]any{
				"hook": "on_tool_result",
				"result": map[string]any{
					"content": []map[string]any{{"type": "text", "text": text}},
				},
			})
			require.Equal(t, pipeline.Decision("allow"), verdict.Decision,
				"ordinary text was redacted: %q", text)
		})
	}
}

func TestWASMPluginCustomPatterns(t *testing.T) {
	host := newHost(t)
	manifest := redactManifest(t, map[string]any{
		"builtins":    false,
		"patterns":    []string{`EMP-[0-9]{6}`},
		"placeholder": "***",
	})
	require.NoError(t, host.Load(context.Background(), manifest))

	verdict := invoke(t, host, manifest, map[string]any{
		"hook": "on_tool_result",
		"result": map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "employee EMP-123456 and card 4111 1111 1111 1111"},
			},
		},
	})
	require.Equal(t, pipeline.Decision("mutate"), verdict.Decision)

	var ops []map[string]any
	require.NoError(t, json.Unmarshal(verdict.Patch, &ops))
	value := ops[0]["value"].(string)
	require.Contains(t, value, "***", "the custom pattern should have matched")
	require.NotContains(t, value, "EMP-123456")
	require.Contains(t, value, "4111 1111 1111 1111",
		"with builtins disabled, the card number should survive")
}

// TestWASMPluginBadConfigFailsOpen: a misconfigured plugin must not block
// traffic on its own initiative. Whether a broken check should deny is the
// engine's per-effect-class decision, not the plugin's.
func TestWASMPluginBadConfigFailsOpen(t *testing.T) {
	host := newHost(t)
	manifest := redactManifest(t, map[string]any{
		"patterns": []string{"(unclosed group"},
	})
	require.NoError(t, host.Load(context.Background(), manifest))

	verdict := invoke(t, host, manifest, map[string]any{
		"hook": "on_tool_result",
		"result": map[string]any{
			"content": []map[string]any{{"type": "text", "text": "anything"}},
		},
	})
	require.Equal(t, pipeline.Decision("allow"), verdict.Decision)
	require.Contains(t, verdict.Reason, "regular expression")
	require.Contains(t, verdict.Annotations, "config_error",
		"the misconfiguration should be visible in the audit trail")
}

// TestWASMHostRejectsDigestMismatch is the control that makes a swapped artifact
// fail closed. Without it, anyone who can write to the artifact directory can
// replace a redaction plugin with one that redacts nothing.
func TestWASMHostRejectsDigestMismatch(t *testing.T) {
	host := newHost(t)
	manifest := redactManifest(t, nil)
	manifest.ArtifactDigest = "sha256:" + strings.Repeat("0", 64)

	err := host.Load(context.Background(), manifest)
	require.Error(t, err)
	require.ErrorContains(t, err, "digest mismatch")
	require.Empty(t, host.LoadedPlugins(), "a mismatched artifact must not load")
}

func TestWASMHostRequiresADigest(t *testing.T) {
	host := newHost(t)
	manifest := redactManifest(t, nil)
	manifest.ArtifactDigest = ""

	err := host.Load(context.Background(), manifest)
	require.ErrorContains(t, err, "declares no artifact digest")
	require.ErrorContains(t, err, "hashes to sha256:",
		"the error should give the operator the digest they need")
}

func TestWASMHostRejectsMissingArtifact(t *testing.T) {
	host := newHost(t)
	manifest := redactManifest(t, nil)
	manifest.ArtifactRef = "file:///nonexistent/plugin.wasm"
	manifest.ArtifactDigest = "sha256:" + strings.Repeat("0", 64)

	err := host.Load(context.Background(), manifest)
	require.Error(t, err)
}

// TestWASMHostRejectsRemoteArtifacts: fetching a plugin at runtime would make the
// gateway's behaviour depend on a remote server's availability and content.
func TestWASMHostRejectsRemoteArtifacts(t *testing.T) {
	host := newHost(t)
	manifest := redactManifest(t, nil)
	manifest.ArtifactRef = "https://example.com/plugin.wasm"

	err := host.Load(context.Background(), manifest)
	require.ErrorContains(t, err, "not supported")
	require.ErrorContains(t, err, "file://")
}

func TestWASMHostRejectsNonPluginWASM(t *testing.T) {
	// A module that is valid WASM but does not implement the ABI must fail at
	// load, not on a user's first tool call.
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.wasm")
	// The minimal valid module: magic + version, no exports.
	minimal := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	require.NoError(t, os.WriteFile(path, minimal, 0o600))
	sum := sha256.Sum256(minimal)

	host := newHost(t)
	err := host.Load(context.Background(), &snapshotpb.PluginManifest{
		Id: "plg_empty", Name: "empty",
		ArtifactRef:    "file://" + path,
		ArtifactDigest: "sha256:" + hex.EncodeToString(sum[:]),
	})
	require.ErrorContains(t, err, "does not export")
	require.ErrorContains(t, err, "plugin-authoring.md",
		"the error should point at the guide")
}

func TestWASMHostInvokeUnloadedPlugin(t *testing.T) {
	host := newHost(t)
	_, err := host.Invoke(context.Background(), &pipeline.Invocation{
		Manifest: &snapshotpb.PluginManifest{Id: "plg_missing", Name: "missing"},
		Context:  []byte(`{}`),
	})
	require.ErrorContains(t, err, "not loaded")
}

// TestWASMHostConcurrentInvocations exercises the instance pool. Each instance
// owns mutable linear memory, so two concurrent calls sharing one would corrupt
// each other's payloads — the same class of bug as the pinning issue, and just as
// silent.
func TestWASMHostConcurrentInvocations(t *testing.T) {
	host := newHost(t)
	manifest := redactManifest(t, nil)
	require.NoError(t, host.Load(context.Background(), manifest))

	const workers = 8
	const each = 6

	var wg sync.WaitGroup
	errs := make(chan error, workers*each)

	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range each {
				// A distinct secret per call, so a crossed payload is detectable
				// rather than coincidentally identical.
				unique := fmt.Sprintf("EMP-%06d", w*100+i)
				raw, _ := json.Marshal(map[string]any{
					"hook": "on_tool_result",
					"result": map[string]any{
						"content": []map[string]any{
							{"type": "text", "text": "card 4111 1111 1111 1111 for " + unique},
						},
					},
				})
				verdict, err := host.Invoke(context.Background(), &pipeline.Invocation{
					Manifest: manifest,
					Hook:     snapshotpb.Hook_HOOK_ON_TOOL_RESULT,
					Context:  raw,
				})
				if err != nil {
					errs <- err
					return
				}
				var ops []map[string]any
				if err := json.Unmarshal(verdict.Patch, &ops); err != nil {
					errs <- err
					return
				}
				value, _ := ops[0]["value"].(string)
				if !strings.Contains(value, unique) {
					errs <- fmt.Errorf("payload crossed between calls: wanted %s in %q", unique, value)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}

// TestWASMHostRespectsDeadline: wazero has no instruction metering, so the
// deadline is the control that stops a runaway guest. If it does not work, a
// plugin stuck in a loop blocks a request forever.
func TestWASMHostRespectsDeadline(t *testing.T) {
	host := newHost(t)
	manifest := redactManifest(t, nil)
	require.NoError(t, host.Load(context.Background(), manifest))

	// An already-expired context: the guest must not run to completion.
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)

	_, err := host.Invoke(ctx, &pipeline.Invocation{
		Manifest: manifest,
		Hook:     snapshotpb.Hook_HOOK_ON_TOOL_RESULT,
		Context:  []byte(`{"hook":"on_tool_result","result":{"content":[{"type":"text","text":"x"}]}}`),
	})
	require.Error(t, err, "an expired deadline must abort the guest")
}

func TestWASMHostRejectsOversizedPayload(t *testing.T) {
	host := newHost(t)
	manifest := redactManifest(t, nil)
	require.NoError(t, host.Load(context.Background(), manifest))

	_, err := host.Invoke(context.Background(), &pipeline.Invocation{
		Manifest: manifest,
		Context:  make([]byte, plugins.MaxPayloadBytes+1),
	})
	require.ErrorContains(t, err, "over the")
}

func TestWASMHostCloseIsIdempotent(t *testing.T) {
	host, err := plugins.NewWASMHost(context.Background(), plugins.WASMOptions{})
	require.NoError(t, err)
	require.NoError(t, host.Close())
	require.NoError(t, host.Close())

	manifest := redactManifest(t, nil)
	require.ErrorContains(t, host.Load(context.Background(), manifest), "closed")
}
