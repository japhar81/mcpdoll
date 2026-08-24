// Copyright 2026 The MCPDoll Authors.

// Package cli builds MCPDoll's command tree.
//
// The tree lives in a package rather than in `main` for two reasons: the parity
// check needs to walk it without running the binary's side effects, and the docs
// generator needs the same. `cmd/mcpdoll` is a thin `main` over this.
//
// Two conventions hold throughout:
//
//   - **Human output by default, JSON as the contract.** `--output json` is what
//     scripts depend on and what is tested; the table renderer is a convenience
//     that may change. Anything a person needs to see is also in the JSON.
//   - **Exit codes carry meaning.** A script must be able to distinguish "you
//     asked for something invalid" from "the thing you asked about is broken",
//     without parsing stderr.
package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

// Exit codes. Documented here and in docs/cli/, because a script that treats
// every non-zero the same cannot retry intelligently.
const (
	// ExitOK is success.
	ExitOK = 0
	// ExitUsage is a malformed invocation: unknown flag, missing argument.
	ExitUsage = 1
	// ExitConfig is a bad configuration or registry document. Retrying will not
	// help; something has to be edited.
	ExitConfig = 2
	// ExitNotFound is a resource that does not exist.
	ExitNotFound = 3
	// ExitUnavailable is a target that could not be reached. A retry may help.
	ExitUnavailable = 4
	// ExitValidation is input that parsed but failed a rule — a name collision,
	// an over-budget bundle.
	ExitValidation = 5
	// ExitFailed is any other failure.
	ExitFailed = 6
)

// Version is stamped at build time.
var Version = "dev"

// Options configures the command tree, so tests can drive it without touching
// the process's streams or the user's home directory.
type Options struct {
	Stdout io.Writer
	Stderr io.Writer
	// ConfigPath overrides ~/.mcpdoll/config.yaml.
	ConfigPath string
}

// New builds the root command.
func New(opts Options) *cobra.Command {
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}

	env := &Env{
		Out:        opts.Stdout,
		Err:        opts.Stderr,
		configPath: opts.ConfigPath,
	}

	root := &cobra.Command{
		Use:   "mcpdoll",
		Short: "Command-line client for the MCPDoll MCP gateway",
		Long: "mcpdoll is the command-line client for MCPDoll, an enterprise MCP gateway.\n\n" +
			"It talks to the control-plane API over HTTP and never touches the database\n" +
			"directly, so anything this CLI can do, your own tooling can do too.",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		// Usage on a usage error, not on a runtime failure: printing 40 lines of
		// help because a backend was unreachable buries the actual message.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return env.load(cmd)
		},
	}

	root.SetOut(opts.Stdout)
	root.SetErr(opts.Stderr)

	root.PersistentFlags().StringVarP(&env.Output, "output", "o", "table",
		"output format: table | json | yaml")
	root.PersistentFlags().StringVar(&env.ConfigFlag, "config", opts.ConfigPath,
		"config file (default ~/.mcpdoll/config.yaml)")
	root.PersistentFlags().StringVar(&env.Profile, "profile", "",
		"named profile from the config file")
	root.PersistentFlags().StringVar(&env.APIURL, "api-url", "",
		"control-plane API base URL")
	root.PersistentFlags().StringVar(&env.Project, "project", "",
		"project scope for this invocation")
	root.PersistentFlags().BoolVar(&env.Quiet, "quiet", false,
		"suppress progress output; results still print")

	root.AddCommand(
		newSnapshotCmd(env),
		newKeysCmd(env),
		newGatewayCmd(env),
		newRegistryCmd(env),
		newTenantsCmd(env),
		newUsersCmd(env),
		newRolesCmd(env),
		newPluginsCmd(env),
		newSystemCmd(env),
		newCompletionCmd(env),
		newCommandsCmd(env),
	)

	return root
}

// Execute runs the tree and returns a process exit code.
//
// Errors are rendered once, here, so no command has to decide how to print a
// failure — and so the exit code always matches what was printed.
func Execute(opts Options) int {
	root := New(opts)
	err := root.Execute()
	if err == nil {
		return ExitOK
	}

	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	fmt.Fprintf(stderr, "mcpdoll: %v\n", err)
	return codeFor(err)
}
