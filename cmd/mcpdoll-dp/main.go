// Copyright 2026 The MCPDoll Authors.

// Command mcpdoll-dp is MCPDoll's data plane: the MCP endpoints agents connect
// to.
//
// It serves from one signed snapshot held in memory and has no dependency on the
// control plane at request time. A control-plane outage is invisible to clients;
// see docs/adr/0002-control-data-plane-split.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mcpdoll/mcpdoll/internal/dataplane/backends"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/edge"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/pipeline"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/plugins"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/snapshot"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/wiring"
	"github.com/mcpdoll/mcpdoll/internal/observability"
	"github.com/mcpdoll/mcpdoll/internal/platform/config"
	"github.com/mcpdoll/mcpdoll/internal/platform/logging"
)

// Version is stamped at build time with -ldflags.
var Version = "dev"

// Exit codes. Distinct so a supervisor can distinguish "misconfigured" (do not
// restart, it will fail identically) from "failed" (a restart may help).
const (
	exitOK          = 0
	exitConfigError = 2
	exitStartupFail = 3
	exitRuntimeFail = 4
)

func main() {
	os.Exit(run())
}

func run() int {
	var (
		configPath  = flag.String("config", os.Getenv("MCPDOLL_CONFIG"), "path to a YAML config file")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("mcpdoll-dp", Version)
		return exitOK
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		// Before the logger exists, so stderr directly.
		fmt.Fprintln(os.Stderr, err)
		return exitConfigError
	}

	level, _ := config.ParseLevel(cfg.Log.Level) // already validated by Load
	log := logging.New(logging.Options{
		Level:   level,
		Format:  cfg.Log.Format,
		Service: "mcpdoll-dp",
	})

	// Cancel on SIGINT/SIGTERM so shutdown is orderly: stop accepting, drain,
	// flush telemetry.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := serve(ctx, cfg, log); err != nil {
		log.ErrorContext(ctx, "data plane exited with an error", "err", err)
		if errors.Is(err, errStartup) {
			return exitStartupFail
		}
		return exitRuntimeFail
	}
	return exitOK
}

// errStartup marks a failure that happened before the server was listening, so
// main can pick the right exit code.
var errStartup = errors.New("startup failed")

func serve(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	telemetry, err := observability.Setup(ctx, observability.Options{
		ServiceName:    "mcpdoll-dp",
		ServiceVersion: Version,
		OTLPEndpoint:   cfg.Telemetry.OTLPEndpoint,
		SampleRatio:    cfg.Telemetry.SampleRatio,
	})
	if err != nil {
		return fmt.Errorf("%w: telemetry: %w", errStartup, err)
	}
	defer func() {
		// A fresh context: the parent is already cancelled by the time this
		// runs, and a cancelled context would abort the flush that is the whole
		// point of shutting down cleanly.
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := telemetry.Shutdown(flushCtx); err != nil {
			log.WarnContext(flushCtx, "telemetry shutdown", "err", err)
		}
	}()

	metrics, err := observability.NewMetrics(telemetry.Meter)
	if err != nil {
		return fmt.Errorf("%w: metrics: %w", errStartup, err)
	}

	verifier, err := snapshot.NewVerifier(cfg.DataPlane.TrustedSigningKeys)
	if err != nil {
		return fmt.Errorf("%w: %w", errStartup, err)
	}
	log.InfoContext(ctx, "trusted snapshot signing keys",
		"key_ids", verifier.TrustedKeyIDs())

	store := snapshot.NewStore(cfg.DataPlane.SnapshotHistory)

	pool := backends.New(backends.Options{
		Logger:           log,
		Telemetry:        telemetry,
		Metrics:          metrics,
		FailureThreshold: cfg.Pipeline.CircuitFailureThreshold,
		Cooldown:         cfg.Pipeline.CircuitCooldown,
		// TokenSource is nil: no exchange is configured, so backends receive no
		// credential at all. That is the safe default — it is emphatically not
		// "forward the caller's token".
	})
	defer pool.Close()

	identity, err := buildIdentity(cfg)
	if err != nil {
		return fmt.Errorf("%w: %w", errStartup, err)
	}

	stateSigner, err := buildStateSigner(ctx, log)
	if err != nil {
		return fmt.Errorf("%w: %w", errStartup, err)
	}

	// Plugin runtimes. A deployment with no plugins still gets a registry — it
	// costs nothing and means the edge does not branch on whether plugins exist.
	wasmHost, err := plugins.NewWASMHost(ctx, plugins.WASMOptions{
		Logger:   log,
		CacheDir: os.Getenv("MCPDOLL_WASM_CACHE_DIR"),
	})
	if err != nil {
		return fmt.Errorf("%w: %w", errStartup, err)
	}
	hosts := wiring.NewHostRegistry(wasmHost, log)
	defer func() {
		if err := hosts.Close(); err != nil {
			log.Warn("closing plugin hosts", "err", err)
		}
	}()

	engine, err := pipeline.New(pipeline.Options{
		Logger:                  log,
		Telemetry:               telemetry,
		Metrics:                 metrics,
		Hosts:                   hosts,
		TotalBudget:             cfg.Pipeline.TotalBudget,
		HookBudget:              cfg.Pipeline.HookBudget,
		CircuitFailureThreshold: cfg.Pipeline.CircuitFailureThreshold,
		CircuitCooldown:         cfg.Pipeline.CircuitCooldown,
		TraceSink:               traceSink(log),
	})
	if err != nil {
		return fmt.Errorf("%w: %w", errStartup, err)
	}

	// Plugins load on activation, before the edge rebuilds, so a request never
	// arrives at a hook whose plugins are still being compiled.
	store.Observe(func(view *snapshot.View) { hosts.Sync(ctx, view) })

	dp, err := edge.New(edge.Options{
		Store:       store,
		Pool:        pool,
		Identity:    identity,
		Logger:      log,
		Telemetry:   telemetry,
		Metrics:     metrics,
		Pipeline:    wiring.NewEdgePipeline(engine, log),
		GraceWindow: cfg.Health.GraceWindow,
		StateSigner: stateSigner,
	})
	if err != nil {
		return fmt.Errorf("%w: %w", errStartup, err)
	}

	// Load the first snapshot before listening, so the process is either ready
	// or has failed for a legible reason — rather than accepting traffic it can
	// only 503.
	source, err := buildSnapshotSource(cfg, store, verifier, log)
	if err != nil {
		return fmt.Errorf("%w: %w", errStartup, err)
	}
	if _, err := source.LoadOnce(ctx); err != nil {
		return fmt.Errorf("%w: loading the initial snapshot: %w", errStartup, err)
	}

	srv := &http.Server{
		Addr:              cfg.DataPlane.ListenAddr,
		Handler:           dp.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: a tool call's duration is the backend's business, and
		// a blanket write deadline would sever a legitimately slow call. Per-call
		// bounding is the pipeline budget's and the caller's context's job.
	}

	errs := make(chan error, 2)

	go func() {
		log.InfoContext(ctx, "data plane listening",
			"addr", cfg.DataPlane.ListenAddr,
			logging.FieldSnapshot, store.Version(),
			"audiences", dp.AudienceSlugs())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- fmt.Errorf("http server: %w", err)
		}
	}()

	go func() {
		if err := source.Run(ctx); err != nil {
			errs <- fmt.Errorf("snapshot source: %w", err)
		}
	}()

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-errs:
		// Still drain: an in-flight tool call should finish rather than be cut
		// off because a different subsystem failed.
		shutdown(srv, log)
		return err
	}

	shutdown(srv, log)
	return nil
}

func shutdown(srv *http.Server, log *slog.Logger) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("graceful shutdown did not complete", "err", err)
	}
}

// buildIdentity wires the identity resolver.
//
// Only the header-based dev resolver exists today, and it refuses to be
// constructed in production — a resolver that trusts client-supplied headers
// reaching production would be a complete authorization bypass. A real OIDC
// resolver is recorded in docs/deferred.md.
func buildIdentity(cfg config.Config) (edge.IdentityResolver, error) {
	resolver, err := edge.NewHeaderIdentityResolver(cfg.Env, "dev-user", []string{"platform"})
	if err != nil {
		return nil, fmt.Errorf(
			"%w\n\nMCPDoll does not yet ship a production identity provider; "+
				"see docs/deferred.md. Run with env=development or staging.", err)
	}
	return resolver, nil
}

// buildStateSigner obtains the MRTR requestState signing key.
//
// The key must be shared across data-plane instances, because a retry may land
// on a different instance than the one that issued the state. An absent key
// generates an ephemeral one and logs loudly: a single-instance dev gateway
// works, and a multi-instance deployment that forgot to configure the secret
// gets told rather than silently failing every second approval.
func buildStateSigner(ctx context.Context, log *slog.Logger) (*edge.StateSigner, error) {
	if raw := os.Getenv("MCPDOLL_REQUEST_STATE_KEY"); raw != "" {
		signer, err := edge.NewStateSigner([]byte(raw))
		if err != nil {
			return nil, err
		}
		return signer, nil
	}

	key, err := edge.GenerateStateKey()
	if err != nil {
		return nil, err
	}
	log.WarnContext(ctx, "generated an ephemeral requestState signing key",
		"why", "MCPDOLL_REQUEST_STATE_KEY is unset",
		"impact", "interactive (input_required) flows will fail across a restart "+
			"and across instances; set a shared secret for any multi-instance deployment")
	signer, err := edge.NewStateSigner(key)
	if err != nil {
		return nil, err
	}
	return signer, nil
}

// snapshotSource is what the data plane loads configuration from.
type snapshotSource interface {
	LoadOnce(context.Context) (*snapshot.View, error)
	Run(context.Context) error
}

func buildSnapshotSource(
	cfg config.Config,
	store *snapshot.Store,
	verifier *snapshot.Verifier,
	log *slog.Logger,
) (snapshotSource, error) {
	switch cfg.DataPlane.SnapshotSource {
	case "file":
		return snapshot.NewFileSource(snapshot.FileSourceOptions{
			Path:     cfg.DataPlane.SnapshotPath,
			Store:    store,
			Verifier: verifier,
			Logger:   log,
		})
	case "grpc":
		// Not built. Failing at startup with a pointer to the alternative is the
		// honest behaviour; silently falling back to a file would hide a
		// misconfiguration that matters.
		return nil, fmt.Errorf(
			"dataplane.snapshot_source %q is not implemented in this build "+
				"(see docs/deferred.md); use \"file\" with dataplane.snapshot_path",
			cfg.DataPlane.SnapshotSource)
	default:
		return nil, fmt.Errorf("dataplane.snapshot_source %q is unknown", cfg.DataPlane.SnapshotSource)
	}
}

// traceSink is where a completed pipeline trace goes.
//
// It logs a one-line summary today. The durable, queryable audit store the
// console's waterfall reads is not built (see docs/deferred.md), and a sink that
// silently discarded traces would make that gap invisible — so this at least puts
// the record somewhere a human can find it.
func traceSink(log *slog.Logger) func(*pipeline.Trace) {
	return func(t *pipeline.Trace) {
		if len(t.Hooks) == 0 {
			return
		}
		var ran bool
		for _, h := range t.Hooks {
			if len(h.Outcomes) > 0 {
				ran = true
			}
		}
		if !ran {
			// No plugin was configured for this hook. Logging it would be one
			// line per request per hook for a system with no plugins at all.
			return
		}
		log.Debug("pipeline trace",
			logging.FieldRequestID, t.RequestID,
			logging.FieldAudience, t.Audience,
			logging.FieldPrincipal, t.Principal,
			logging.FieldToolName, t.Tool,
			"summary", t.Summary())
	}
}
