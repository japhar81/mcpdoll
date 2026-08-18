// Copyright 2026 The MCPDoll Authors.

// Command mcpdoll-cp is MCPDoll's control plane: the API the console and the
// CLI talk to.
//
// It is not in the request path. A control-plane outage stops publishing and
// stops the console; it does not stop a single agent's tool call, because the
// data plane serves from a snapshot it already holds. See
// docs/adr/0002-control-data-plane-split.md.
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

	"github.com/mcpdoll/mcpdoll/internal/controlplane/apiserver"
	"github.com/mcpdoll/mcpdoll/internal/observability"
	"github.com/mcpdoll/mcpdoll/internal/platform/config"
	"github.com/mcpdoll/mcpdoll/internal/platform/logging"
)

// Version is stamped at build time with -ldflags.
var Version = "dev"

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
		configPath     = flag.String("config", os.Getenv("MCPDOLL_CONFIG"), "path to a YAML config file")
		registryPath   = flag.String("registry", "", "registry document to serve (overrides config)")
		allowAnonymous = flag.Bool("allow-anonymous", false,
			"serve without requiring a bearer token — local development only")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("mcpdoll-cp", Version)
		return exitOK
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcpdoll-cp: %v\n", err)
		return exitConfigError
	}

	level, _ := config.ParseLevel(cfg.Log.Level) // already validated by Load
	log := logging.New(logging.Options{
		Level:   level,
		Format:  cfg.Log.Format,
		Service: "mcpdoll-cp",
	}).With(slog.String("version", Version))

	telemetry, err := observability.Setup(context.Background(), observability.Options{
		ServiceName:    "mcpdoll-cp",
		ServiceVersion: Version,
		OTLPEndpoint:   cfg.Telemetry.OTLPEndpoint,
		SampleRatio:    cfg.Telemetry.SampleRatio,
	})
	if err != nil {
		log.Error("telemetry setup failed", slog.String("error", err.Error()))
		return exitStartupFail
	}
	defer func() {
		// A fresh context: by the time this runs the request context is
		// cancelled, and a cancelled context aborts the very flush that is the
		// point of shutting down cleanly.
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := telemetry.Shutdown(flushCtx); err != nil {
			log.Warn("telemetry shutdown failed", slog.String("error", err.Error()))
		}
	}()

	registry := cfg.ControlPlane.RegistryPath
	if *registryPath != "" {
		registry = *registryPath
	}

	// The token comes from the environment rather than the config file, so it
	// never ends up in a repository. An operator who wants it in config can put
	// it there; the environment simply wins.
	token := os.Getenv("MCPDOLL_CP_TOKEN")
	if token == "" {
		token = cfg.ControlPlane.APIToken
	}

	server, err := apiserver.New(apiserver.Config{
		RegistryPath:   registry,
		SnapshotPath:   cfg.DataPlane.SnapshotPath,
		GatewayURL:     cfg.ControlPlane.GatewayURL,
		SigningKeyPath: cfg.ControlPlane.SigningKeyPath,
		SigningKeyID:   cfg.ControlPlane.SigningKeyID,
		KeyDir:         cfg.ControlPlane.KeyDir,
		Token:          token,
		AllowAnonymous: *allowAnonymous,
		AllowedOrigins: cfg.ControlPlane.AllowedOrigins,
		Version:        Version,
		Logger:         log,
	})
	if err != nil {
		// A refusal here is a configuration problem, and the message says which
		// one. Exiting 2 tells a supervisor not to restart: it will fail the
		// same way until somebody edits something.
		log.Error("refusing to start", slog.String("error", err.Error()))
		return exitConfigError
	}

	addr := cfg.ControlPlane.ListenAddr
	httpServer := &http.Server{
		Addr:    addr,
		Handler: server,
		// A slow-loris client should not be able to hold a connection open
		// indefinitely against an API that has no per-connection cost limit.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errs := make(chan error, 1)
	go func() {
		log.Info("control plane listening",
			slog.String("addr", addr),
			slog.String("registry", registry),
			slog.Bool("authenticated", !*allowAnonymous))
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errs:
		log.Error("control plane failed", slog.String("error", err.Error()))
		return exitRuntimeFail
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Warn("graceful shutdown timed out", slog.String("error", err.Error()))
	}
	return exitOK
}
