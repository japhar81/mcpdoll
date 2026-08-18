// Copyright 2026 The MCPDoll Authors.

// Command fixture-backend serves one of MCPDoll's fixture MCP backends over
// HTTP, for `make dev` and for manual exploration.
//
// These are the same backends the test suite drives in-process, so what you can
// reproduce by hand is exactly what CI asserts.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mcpdoll/mcpdoll/fixtures"
)

func main() {
	var (
		kind = flag.String("kind", "modern",
			"which fixture to serve: modern | legacy | misbehaving | hostile | confirming")
		addr = flag.String("addr", ":9101", "listen address")
		// The misbehaving backend's controls are exposed as flags so `make dev`
		// can stand up a permanently-slow or permanently-flapping backend without
		// needing a control API.
		latency   = flag.Duration("latency", 0, "misbehaving: delay added to every call")
		failEvery = flag.Int("fail-every", 0, "misbehaving: fail every Nth call (0 disables)")
		drifted   = flag.String("drift", "",
			"misbehaving: start drifted — \"cosmetic\" (description only) or "+
				"\"semantic\" (input schema)")
	)
	flag.Parse()

	backend, control, err := build(*kind)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	if control != nil {
		if *latency > 0 {
			control.SetLatency(*latency)
		}
		if *failEvery > 0 {
			control.FailEvery(*failEvery)
		}
		switch *drifted {
		case "":
		case "cosmetic":
			control.DriftAs(fixtures.DriftCosmetic)
		case "semantic":
			control.DriftAs(fixtures.DriftSemantic)
		default:
			fmt.Fprintf(os.Stderr,
				"unknown -drift %q: use cosmetic or semantic\n", *drifted)
			os.Exit(2)
		}
	}

	mux := http.NewServeMux()
	mux.Handle("/", backend.Handler())
	// A liveness endpoint so compose can order startup. Deliberately not on the
	// MCP path: a GET there is a 405 in stateless mode, which is correct
	// behaviour but useless as a health check.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","fixture":%q}`, *kind)
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("fixture %q listening on %s (MCP at /, health at /healthz)", *kind, *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("fixture-backend: %v", err)
	}
}

// build returns the backend and, for the misbehaving one, its control surface.
func build(kind string) (*fixtures.Backend, *fixtures.MisbehavingBackend, error) {
	switch strings.ToLower(kind) {
	case "modern":
		return fixtures.NewModern(), nil, nil
	case "legacy":
		return fixtures.NewLegacy(), nil, nil
	case "misbehaving":
		m := fixtures.NewMisbehaving()
		return m.Backend, m, nil
	case "hostile":
		return fixtures.NewHostile(), nil, nil
	case "confirming":
		return fixtures.NewConfirming(), nil, nil
	default:
		return nil, nil, fmt.Errorf(
			"fixture-backend: unknown kind %q; want modern, legacy, misbehaving, hostile or confirming",
			kind)
	}
}
