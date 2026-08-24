// Copyright 2026 Henry Zektser.

package wiring

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/mcpdoll/mcpdoll/internal/dataplane/pipeline"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/plugins"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/snapshot"
	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// HostRegistry resolves a plugin manifest to a running host, and keeps the set
// of loaded plugins in step with the snapshot.
//
// Loading happens at snapshot activation, not on first use. A plugin whose
// artifact is missing, whose digest does not match, or which does not implement
// the ABI should fail a *deploy* — visibly, once — rather than a user's tool
// call, repeatedly.
//
// A load failure does not refuse the snapshot, though. That would let one broken
// plugin block a configuration change that has nothing to do with it. Instead the
// plugin is recorded as unloadable and the engine skips it, applying the
// manifest's failure policy per effect class — which is the mechanism that
// already exists for deciding whether a missing check should block traffic.
type HostRegistry struct {
	wasm *plugins.WASMHost
	log  *slog.Logger

	mu sync.RWMutex
	// loaded maps plugin id to the host that can run it.
	loaded map[string]pipeline.Host
	// failures records why a plugin could not be loaded, for the console and for
	// a legible error when the engine asks for it.
	failures map[string]error
}

// NewHostRegistry builds a registry over the given runtimes.
//
// A nil WASM host is legitimate: a deployment with no plugins does not need a
// WASM runtime, and instantiating one would be several megabytes of machinery for
// nothing.
func NewHostRegistry(wasm *plugins.WASMHost, log *slog.Logger) *HostRegistry {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &HostRegistry{
		wasm:     wasm,
		log:      log,
		loaded:   map[string]pipeline.Host{},
		failures: map[string]error{},
	}
}

var _ pipeline.HostResolver = (*HostRegistry)(nil)

// Host implements pipeline.HostResolver.
func (r *HostRegistry) Host(manifest *snapshotpb.PluginManifest) (pipeline.Host, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if host, ok := r.loaded[manifest.Id]; ok {
		return host, nil
	}
	if err, ok := r.failures[manifest.Id]; ok {
		// Return the load-time reason rather than a bare "not loaded": the
		// engine records it in the outcome, so it reaches the audit trail and
		// the console where someone can act on it.
		return nil, err
	}
	return nil, fmt.Errorf("wiring: plugin %q is not loaded", manifest.Name)
}

// Sync loads the snapshot's plugins and unloads the ones that are gone.
//
// Registered as a snapshot observer rather than a preparer: see the type comment
// for why a plugin that fails to load does not refuse the snapshot.
func (r *HostRegistry) Sync(ctx context.Context, view *snapshot.View) {
	// Every plugin the snapshot declares, not the ones some audience selected.
	// Plugin scoping is now per toolset and resolved per principal at connect
	// time (ADR 0016), so the host cannot know in advance which principals will
	// need which plugin — and loading a WASM module lazily on a request would
	// put compilation on the serving path.
	wanted := map[string]*snapshotpb.PluginManifest{}
	for _, manifest := range view.Proto().Plugins {
		wanted[manifest.Id] = manifest
	}

	loaded := map[string]pipeline.Host{}
	failures := map[string]error{}

	for id, manifest := range wanted {
		host, err := r.load(ctx, manifest)
		if err != nil {
			failures[id] = err
			r.log.ErrorContext(ctx, "plugin could not be loaded",
				"plugin", manifest.Name,
				"runtime", manifest.Runtime.String(),
				"err", err,
				"effect", "the plugin will be skipped; its manifest failure policy decides whether that blocks traffic")
			continue
		}
		loaded[id] = host
	}

	r.mu.Lock()
	r.loaded = loaded
	r.failures = failures
	r.mu.Unlock()

	r.log.InfoContext(ctx, "plugin hosts synchronized",
		"loaded", len(loaded), "failed", len(failures))
}

func (r *HostRegistry) load(ctx context.Context, manifest *snapshotpb.PluginManifest) (pipeline.Host, error) {
	switch manifest.Runtime {
	case snapshotpb.PluginRuntime_PLUGIN_RUNTIME_WASM:
		if r.wasm == nil {
			return nil, errors.New(
				"wiring: this data plane has no WASM runtime configured, so a wasm plugin cannot run")
		}
		if err := r.wasm.Load(ctx, manifest); err != nil {
			return nil, err
		}
		return r.wasm, nil

	case snapshotpb.PluginRuntime_PLUGIN_RUNTIME_GRPC:
		// Not built. Failing with a pointer to the gap is honest; silently
		// treating a gRPC plugin as absent would make a configured security
		// control look present.
		return nil, fmt.Errorf(
			"wiring: plugin %q needs the gRPC runtime, which is not implemented in this build "+
				"(see docs/deferred.md)", manifest.Name)

	default:
		return nil, fmt.Errorf("wiring: plugin %q has no runtime configured", manifest.Name)
	}
}

// Failures reports the plugins that could not be loaded, for the admin surface.
func (r *HostRegistry) Failures() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]string, len(r.failures))
	for id, err := range r.failures {
		out[id] = err.Error()
	}
	return out
}

// Loaded lists the plugin ids currently runnable.
func (r *HostRegistry) Loaded() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.loaded))
	for id := range r.loaded {
		out = append(out, id)
	}
	return out
}

// Close releases every runtime.
func (r *HostRegistry) Close() error {
	if r.wasm == nil {
		return nil
	}
	return r.wasm.Close()
}

// allHooks is the closed set, used to enumerate a snapshot's plugins.
//
// Ranging over the seven rather than over a plugin list because the view indexes
// plugins by hook — and because a hook added without updating this list would
// then silently fail to load its plugins, which the ADR-gated closed set is meant
// to prevent.
var allHooks = []snapshotpb.Hook{
	snapshotpb.Hook_HOOK_ON_REQUEST,
	snapshotpb.Hook_HOOK_ON_IDENTITY,
	snapshotpb.Hook_HOOK_ON_CATALOG,
	snapshotpb.Hook_HOOK_ON_TOOL_CALL,
	snapshotpb.Hook_HOOK_ON_TOOL_RESULT,
	snapshotpb.Hook_HOOK_ON_RESPONSE,
	snapshotpb.Hook_HOOK_ON_AUDIT,
}
