// Copyright 2026 Henry Zektser.

// Command mcpdoll is the command-line client for the MCPDoll gateway.
//
// The command tree lives in internal/cli so the parity check and the docs
// generator can walk it without running a binary's side effects. This file is a
// thin main over it.
package main

import (
	"os"

	"github.com/mcpdoll/mcpdoll/internal/cli"
)

// version is stamped at build time with -ldflags.
var version = "dev"

func main() {
	cli.Version = version
	os.Exit(cli.Execute(cli.Options{
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}))
}
