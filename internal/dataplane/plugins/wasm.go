// Copyright 2026 The MCPDoll Authors.

// Package plugins hosts MCPDoll's two plugin runtimes.
//
// WASM ([WASMHost]) is the default and should stay that way. A WASM module
// instantiated with no host imports beyond WASI cannot open a socket, read a
// file, or see a clock — not because policy forbids it but because the functions
// do not exist in its import namespace. That is a structural guarantee, and it is
// worth a great deal on a component that runs third-party code inside the request
// path of every tool call in the organization.
//
// gRPC ([GRPCHost], see grpc.go) exists for the plugins that genuinely cannot be
// pure: the LLM guard has to reach a model. Those give up the structural
// guarantee and are correspondingly harder to trust, which is exactly why the
// distinction is a runtime choice recorded in the manifest rather than a detail.
package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"

	"github.com/mcpdoll/mcpdoll/internal/dataplane/pipeline"
	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// The ABI a WASM plugin must implement. Three exports, documented in
// docs/architecture/plugin-authoring.md and implemented by the SDK in
// plugins/sdk so an author never writes them by hand.
const (
	// ExportAlloc allocates and pins `size` bytes, returning a pointer.
	//
	// Pinning is not optional. A Go-compiled guest's collector will reclaim an
	// unreferenced buffer between the host writing to it and the guest reading
	// it, which corrupts the payload silently rather than failing — the worst
	// possible failure mode for a security control.
	ExportAlloc = "mcpdoll_alloc"

	// ExportFree unpins a pointer previously returned by alloc.
	ExportFree = "mcpdoll_free"

	// ExportInvoke runs the plugin over the JSON at (ptr, len) and returns
	// (resultPtr << 32) | resultLen. The result buffer is pinned; the host frees
	// it.
	ExportInvoke = "mcpdoll_invoke"
)

// MaxPayloadBytes bounds what crosses the ABI in either direction.
//
// A plugin that returns a gigabyte of "verdict" would exhaust the host's memory
// on a path that is supposed to be bounded, so the limit applies to the guest's
// output as strictly as to the host's input.
const MaxPayloadBytes = 8 << 20 // 8 MiB

// WASMOptions configures a [WASMHost].
type WASMOptions struct {
	Logger *slog.Logger

	// MemoryLimitPages caps a guest's linear memory, in 64 KiB WebAssembly pages.
	//
	// This is a real bound, unlike the instruction budget below: a guest that
	// tries to grow past it gets a failed memory.grow rather than taking the host
	// down with it. Defaults to 256 pages (16 MiB), which is comfortable for a
	// Go-compiled guest and far below what a runaway allocation would want.
	MemoryLimitPages uint32

	// CacheDir persists compiled modules between restarts. Compilation dominates
	// first-call latency for a Go-compiled module, so without it every restart
	// pays it again.
	CacheDir string
}

// A note on fuel, because the manifest has a `fuel_limit` field and this host
// does not enforce it.
//
// An instruction budget would be the right control: it is reproducible, so a
// plugin that passes on an idle machine cannot trip on a busy one. wazero does
// not implement one — it offers `WithCloseOnContextDone`, which inserts periodic
// checks and terminates a guest when the call's context is cancelled or its
// deadline passes.
//
// So a runaway plugin *is* stopped, on wall-clock rather than instruction count,
// by the engine's per-plugin deadline. The consequence is that the limit is
// load-dependent: the same plugin may complete on a quiet host and be cut off on
// a busy one. [WASMHost.Load] warns when a manifest sets `fuel_limit`, rather
// than letting an operator believe a limit is in force that is not. Recorded in
// docs/deferred.md.

// WASMHost compiles and runs WASM plugins.
type WASMHost struct {
	opts    WASMOptions
	runtime wazero.Runtime
	cache   wazero.CompilationCache

	mu       sync.Mutex
	compiled map[string]*compiledPlugin
	closed   bool
}

type compiledPlugin struct {
	manifest *snapshotpb.PluginManifest
	module   wazero.CompiledModule
	// digest of the artifact actually loaded, checked against the manifest.
	digest string

	// instances is a pool. A module instance holds mutable linear memory, so two
	// concurrent invocations cannot share one — but instantiating per call costs
	// far more than the call itself for a Go-compiled guest.
	instances chan api.Module
}

// NewWASMHost builds a host.
func NewWASMHost(ctx context.Context, opts WASMOptions) (*WASMHost, error) {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.MemoryLimitPages == 0 {
		opts.MemoryLimitPages = 256 // 16 MiB
	}

	config := wazero.NewRuntimeConfig().
		// Without this a guest stuck in a loop ignores the engine's deadline
		// entirely, and the hook budget becomes advisory. It costs a periodic
		// check per guest; a plugin host that cannot stop a runaway plugin is
		// not worth having.
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(opts.MemoryLimitPages)

	var cache wazero.CompilationCache
	if opts.CacheDir != "" {
		var err error
		cache, err = wazero.NewCompilationCacheWithDir(opts.CacheDir)
		if err != nil {
			return nil, fmt.Errorf("plugins: compilation cache at %s: %w", opts.CacheDir, err)
		}
		config = config.WithCompilationCache(cache)
	}

	rt := wazero.NewRuntimeWithConfig(ctx, config)

	// WASI is instantiated because a Go-compiled guest's runtime needs it to
	// start at all. Note what it does *not* provide: wazero's
	// wasi_snapshot_preview1 has no sockets, and the module config below grants
	// no filesystem, no environment, and no stdio. The guest's import namespace
	// simply has no way to reach anything.
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("plugins: instantiating WASI: %w", err)
	}

	return &WASMHost{
		opts:     opts,
		runtime:  rt,
		cache:    cache,
		compiled: map[string]*compiledPlugin{},
	}, nil
}

// Close releases every compiled module and the runtime.
func (h *WASMHost) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	h.mu.Unlock()

	ctx := context.Background()
	var errs []error
	if err := h.runtime.Close(ctx); err != nil {
		errs = append(errs, err)
	}
	if h.cache != nil {
		if err := h.cache.Close(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Load compiles a plugin's artifact and verifies its digest.
//
// The digest check is what makes a swapped artifact fail closed. Without it,
// anyone who can write to the artifact directory can replace a redaction plugin
// with one that redacts nothing, and the manifest would still claim it was
// running.
func (h *WASMHost) Load(ctx context.Context, manifest *snapshotpb.PluginManifest) error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return errors.New("plugins: host is closed")
	}
	if existing, ok := h.compiled[manifest.Id]; ok && existing.digest == manifest.ArtifactDigest {
		h.mu.Unlock()
		return nil
	}
	h.mu.Unlock()

	wasmBytes, err := readArtifact(manifest.ArtifactRef)
	if err != nil {
		return fmt.Errorf("plugins: %s: %w", manifest.Name, err)
	}
	if len(wasmBytes) == 0 {
		return fmt.Errorf("plugins: %s: artifact %s is empty", manifest.Name, manifest.ArtifactRef)
	}

	sum := sha256.Sum256(wasmBytes)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if manifest.ArtifactDigest == "" {
		return fmt.Errorf(
			"plugins: %s declares no artifact digest; without one a swapped artifact "+
				"cannot be detected (the artifact on disk hashes to %s)",
			manifest.Name, digest)
	}
	if digest != manifest.ArtifactDigest {
		return fmt.Errorf(
			"plugins: %s artifact digest mismatch: the manifest expects %s but %s hashes to %s",
			manifest.Name, manifest.ArtifactDigest, manifest.ArtifactRef, digest)
	}

	module, err := h.runtime.CompileModule(ctx, wasmBytes)
	if err != nil {
		return fmt.Errorf("plugins: %s: compiling: %w", manifest.Name, err)
	}

	// Verify the ABI at load time rather than on the first request. A plugin that
	// cannot be invoked should fail a deploy, not a user's tool call.
	exports := module.ExportedFunctions()
	for _, name := range []string{ExportAlloc, ExportFree, ExportInvoke} {
		if _, ok := exports[name]; !ok {
			_ = module.Close(ctx)
			return fmt.Errorf(
				"plugins: %s does not export %q; see docs/architecture/plugin-authoring.md",
				manifest.Name, name)
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if old, ok := h.compiled[manifest.Id]; ok {
		old.closeInstances(ctx)
		_ = old.module.Close(ctx)
	}
	h.compiled[manifest.Id] = &compiledPlugin{
		manifest:  manifest,
		module:    module,
		digest:    digest,
		instances: make(chan api.Module, maxInstancesPerPlugin),
	}
	if manifest.FuelLimit > 0 {
		// Say so plainly rather than letting an operator believe a limit they
		// configured is in force.
		h.opts.Logger.WarnContext(ctx, "fuel_limit is not enforced by the wasm host",
			"plugin", manifest.Name,
			"fuel_limit", manifest.FuelLimit,
			"why", "wazero has no instruction metering",
			"instead", "the plugin's budget_ms deadline bounds it on wall-clock time")
	}
	h.opts.Logger.InfoContext(ctx, "loaded wasm plugin",
		"plugin", manifest.Name, "digest", digest[:19])
	return nil
}

// maxInstancesPerPlugin bounds the instance pool. Each instance holds its own
// linear memory — several megabytes for a Go-compiled guest — so an unbounded
// pool under a traffic spike is a memory incident.
const maxInstancesPerPlugin = 8

// Invoke implements pipeline.Host.
func (h *WASMHost) Invoke(ctx context.Context, inv *pipeline.Invocation) (*pipeline.Verdict, error) {
	h.mu.Lock()
	plugin, ok := h.compiled[inv.Manifest.Id]
	h.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("plugins: %s is not loaded", inv.Manifest.Name)
	}
	if len(inv.Context) > MaxPayloadBytes {
		return nil, fmt.Errorf("plugins: invocation payload is %d bytes, over the %d-byte limit",
			len(inv.Context), MaxPayloadBytes)
	}

	// The host adds the plugin's configuration, the hook, and the shadow flag;
	// see buildPayload for why that split belongs here rather than in the engine.
	payload, err := buildPayload(inv)
	if err != nil {
		return nil, err
	}
	if len(payload) > MaxPayloadBytes {
		return nil, fmt.Errorf("plugins: assembled payload is %d bytes, over the %d-byte limit",
			len(payload), MaxPayloadBytes)
	}

	instance, err := h.acquire(ctx, plugin)
	if err != nil {
		return nil, err
	}
	// A guest that trapped or ran out of fuel has undefined internal state, so
	// it is discarded rather than returned to the pool.
	healthy := false
	defer func() {
		if healthy {
			h.release(plugin, instance)
		} else {
			_ = instance.Close(context.Background())
		}
	}()

	out, err := callGuest(ctx, instance, payload)
	if err != nil {
		return nil, fmt.Errorf("plugins: %s: %w", inv.Manifest.Name, err)
	}
	healthy = true

	var verdict pipeline.Verdict
	if err := json.Unmarshal(out, &verdict); err != nil {
		return nil, fmt.Errorf("plugins: %s returned an unreadable verdict: %w",
			inv.Manifest.Name, err)
	}
	return &verdict, nil
}

// callGuest performs one alloc / invoke / free cycle.
func callGuest(ctx context.Context, mod api.Module, input []byte) ([]byte, error) {
	allocFn := mod.ExportedFunction(ExportAlloc)
	freeFn := mod.ExportedFunction(ExportFree)
	invokeFn := mod.ExportedFunction(ExportInvoke)

	res, err := allocFn.Call(ctx, uint64(len(input)))
	if err != nil {
		return nil, fmt.Errorf("alloc: %w", err)
	}
	inPtr := uint32(res[0])
	if inPtr == 0 {
		return nil, errors.New("alloc returned a null pointer")
	}
	defer func() { _, _ = freeFn.Call(context.Background(), uint64(inPtr)) }()

	if !mod.Memory().Write(inPtr, input) {
		return nil, fmt.Errorf("writing %d bytes at %d exceeds the guest's memory", len(input), inPtr)
	}

	res, err = invokeFn.Call(ctx, uint64(inPtr), uint64(len(input)))
	if err != nil {
		return nil, fmt.Errorf("invoke: %w", err)
	}
	packed := res[0]
	outPtr, outLen := uint32(packed>>32), uint32(packed)
	if outPtr == 0 || outLen == 0 {
		return nil, errors.New("invoke returned an empty result")
	}
	if outLen > MaxPayloadBytes {
		return nil, fmt.Errorf("plugin returned %d bytes, over the %d-byte limit",
			outLen, MaxPayloadBytes)
	}
	defer func() { _, _ = freeFn.Call(context.Background(), uint64(outPtr)) }()

	out, ok := mod.Memory().Read(outPtr, outLen)
	if !ok {
		return nil, fmt.Errorf("reading %d bytes at %d exceeds the guest's memory", outLen, outPtr)
	}
	// Copy: the returned slice aliases guest memory, which the next call will
	// reuse.
	return append([]byte(nil), out...), nil
}

func (h *WASMHost) acquire(ctx context.Context, plugin *compiledPlugin) (api.Module, error) {
	select {
	case mod := <-plugin.instances:
		return mod, nil
	default:
	}

	config := wazero.NewModuleConfig().
		// A unique name per instance: wazero refuses two modules with the same
		// name in one runtime, and the pool holds several.
		WithName("").
		// Nothing is granted. No filesystem, no environment, no arguments, no
		// stdio, no clock. A plugin that needs any of them is a gRPC plugin.
		WithStartFunctions("_initialize")

	mod, err := h.runtime.InstantiateModule(ctx, plugin.module, config)
	if err != nil {
		return nil, fmt.Errorf("plugins: %s: instantiating: %w", plugin.manifest.Name, err)
	}
	return mod, nil
}

func (h *WASMHost) release(plugin *compiledPlugin, mod api.Module) {
	select {
	case plugin.instances <- mod:
	default:
		// Pool full: close rather than leak. Under a spike it is better to pay
		// re-instantiation than to hold memory the steady state does not need.
		_ = mod.Close(context.Background())
	}
}

func (p *compiledPlugin) closeInstances(ctx context.Context) {
	for {
		select {
		case mod := <-p.instances:
			_ = mod.Close(ctx)
		default:
			return
		}
	}
}

// readArtifact loads a plugin artifact.
//
// Only `file://` and bare paths are supported. Fetching a plugin over HTTP at
// load time would make the gateway's behaviour depend on a remote server's
// availability and content at restart — a plugin should be a build artifact
// placed by a deploy, not something retrieved at runtime.
func readArtifact(ref string) ([]byte, error) {
	switch {
	case ref == "":
		return nil, errors.New("artifact_ref is empty")
	case strings.HasPrefix(ref, "file://"):
		return os.ReadFile(strings.TrimPrefix(ref, "file://"))
	case strings.Contains(ref, "://"):
		scheme, _, _ := strings.Cut(ref, "://")
		return nil, fmt.Errorf(
			"artifact scheme %q is not supported; place the artifact on disk and use file:// "+
				"(fetching a plugin at runtime would make the gateway depend on a remote server)",
			scheme)
	default:
		return os.ReadFile(ref)
	}
}

// LoadedPlugins lists the ids currently compiled, for diagnostics.
func (h *WASMHost) LoadedPlugins() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.compiled))
	for id := range h.compiled {
		out = append(out, id)
	}
	return out
}
