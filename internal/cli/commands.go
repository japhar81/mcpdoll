// Copyright 2026 The MCPDoll Authors.

package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/mcpdoll/mcpdoll/internal/controlplane/registry"
)

// annotationOperation is the cobra annotation naming the API operation a command
// satisfies.
//
// This is the CLI half of the tri-surface check (ADR 0004). The parity tool reads
// it out of the command tree, so a command that forgets the annotation is
// reported as covering nothing — which is the correct outcome, since a command
// nobody can trace back to an operation is not evidence the operation is
// reachable.
const annotationOperation = "mcpdoll.operation"

// newCommandsCmd is the hidden `__commands` command.
//
// It exists so `tools/paritycheck` can enumerate the real command tree — the one
// users get, including annotations — rather than a hand-maintained list that
// drifts. Hidden because it is a build-tooling interface, not a user feature.
func newCommandsCmd(env *Env) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:    "__commands",
		Short:  "Dump the command tree (build tooling)",
		Hidden: true,
		Long: "Emits every command, its flags, and the API operation it satisfies.\n" +
			"Consumed by tools/paritycheck to enforce the tri-surface rule.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			tree := walkCommands(cmd.Root())
			if asJSON || env.Output == "json" {
				enc := json.NewEncoder(env.Out)
				enc.SetIndent("", "  ")
				return enc.Encode(tree)
			}
			return env.Emit(tree)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON regardless of --output")
	return cmd
}

// CommandTree is the machine-readable command surface.
type CommandTree struct {
	Version  string          `json:"version"`
	Commands []CommandRecord `json:"commands"`
}

// CommandRecord is one leaf command.
type CommandRecord struct {
	// Path is the full invocation, e.g. "mcpdoll snapshot build".
	Path string `json:"path"`
	// Operation is the API operationId this command satisfies, or "" for a
	// command that is not an API surface (completion, the command dump itself).
	Operation string   `json:"operation,omitempty"`
	Short     string   `json:"short"`
	Hidden    bool     `json:"hidden"`
	Flags     []string `json:"flags,omitempty"`
	// Local reports whether the command works without a control plane, which is
	// how the parity tool knows not to demand an operation for it.
	Local bool `json:"local"`
}

// walkCommands flattens the tree to its leaves.
//
// Only leaves are reported: a group like `mcpdoll snapshot` is navigation, not an
// operation, and demanding an API operation for it would make the parity check
// meaningless noise.
func walkCommands(root *cobra.Command) CommandTree {
	tree := CommandTree{Version: Version}
	var visit func(cmd *cobra.Command, path []string)
	visit = func(cmd *cobra.Command, path []string) {
		current := append(append([]string(nil), path...), cmd.Name())
		children := cmd.Commands()

		// Runnability is the whole test. A navigation group has no RunE, so it
		// is excluded automatically — and a command that is *both* runnable and
		// a parent (`registry servers`, which lists, and also holds `show`) is
		// still reported. Suppressing those cost a real operation its CLI
		// surface, and the parity check reported it as missing.
		if cmd.Runnable() {
			record := CommandRecord{
				Path:      strings.Join(current, " "),
				Operation: cmd.Annotations[annotationOperation],
				Short:     cmd.Short,
				Hidden:    cmd.Hidden,
				Local:     cmd.Annotations[annotationOperation] == "",
			}
			cmd.LocalFlags().VisitAll(func(f *pflag.Flag) {
				record.Flags = append(record.Flags, "--"+f.Name)
			})
			sort.Strings(record.Flags)
			tree.Commands = append(tree.Commands, record)
		}

		for _, child := range children {
			visit(child, current)
		}
	}
	visit(root, nil)
	sort.Slice(tree.Commands, func(i, j int) bool {
		return tree.Commands[i].Path < tree.Commands[j].Path
	})
	return tree
}

// Table renders the command surface for a human.
func (t CommandTree) Table() Table {
	rows := make([][]string, 0, len(t.Commands))
	var withOperation int
	for _, c := range t.Commands {
		if c.Hidden {
			continue
		}
		op := c.Operation
		if op == "" {
			op = "-"
		} else {
			withOperation++
		}
		rows = append(rows, []string{c.Path, op, c.Short})
	}
	return Table{
		Columns: []string{"COMMAND", "OPERATION", "DESCRIPTION"},
		Rows:    rows,
		Notes: []string{
			fmt.Sprintf("%d command(s), %d bound to an API operation", len(rows), withOperation),
		},
	}
}

// ------------------------------------------------------------- completion ----

func newCompletionCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "completion <bash|zsh|fish|powershell>",
		Short: "Generate a shell completion script",
		Long: "Generate a completion script for your shell.\n\n" +
			"  bash:  source <(mcpdoll completion bash)\n" +
			"  zsh:   mcpdoll completion zsh > \"${fpath[1]}/_mcpdoll\"\n" +
			"  fish:  mcpdoll completion fish | source\n",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "bash":
				return cmd.Root().GenBashCompletionV2(env.Out, true)
			case "zsh":
				return cmd.Root().GenZshCompletion(env.Out)
			case "fish":
				return cmd.Root().GenFishCompletion(env.Out, true)
			case "powershell":
				return cmd.Root().GenPowerShellCompletionWithDesc(env.Out)
			default:
				return usageError(fmt.Errorf("unsupported shell %q", args[0]))
			}
		},
	}
	return cmd
}

// --------------------------------------------------------------- registry ----

func newRegistryCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "registry",
		Short: "Work with registry documents",
		Long: "The registry document declares which backends exist, how their tools are\n" +
			"classified, and which audiences see what. It is the reviewable source a\n" +
			"snapshot is built from.",
	}
	cmd.AddCommand(
		newRegistryValidateCmd(env),
		newRegistryHooksCmd(env),
		newRegistryShowCmd(env),
		newRegistryServersCmd(env),
	)
	return cmd
}

func newRegistryValidateCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "validate <file>",
		Short: "Check a registry document without contacting any backend",
		Long: "Validates structure and internal consistency: unknown keys, dangling\n" +
			"references, duplicate prefixes, TTLs that try to widen rather than narrow.\n\n" +
			"This runs offline, so it belongs in a pre-commit hook or a pull-request check.\n" +
			"`snapshot build --dry-run` does the same plus live discovery.",
		Args: cobra.ExactArgs(1),
		Annotations: map[string]string{
			annotationOperation: "validateRegistry",
		},
		RunE: func(_ *cobra.Command, args []string) error {
			spec, err := registry.Load(args[0])
			if err != nil {
				return configError(err)
			}
			return env.Emit(registryReport{
				File:       args[0],
				Valid:      true,
				Org:        spec.Org,
				Version:    spec.Version,
				Namespaces: len(spec.Namespaces),
				Servers:    len(spec.Servers),
				Toolsets:   len(spec.Toolsets),
				Policies:   len(spec.Policies),
				Plugins:    len(spec.Plugins),
			})
		},
	}
	return cmd
}

type registryReport struct {
	File       string `json:"file" yaml:"file"`
	Valid      bool   `json:"valid" yaml:"valid"`
	Org        string `json:"org" yaml:"org"`
	Version    int64  `json:"version" yaml:"version"`
	Namespaces int    `json:"namespaces" yaml:"namespaces"`
	Servers    int    `json:"servers" yaml:"servers"`
	Toolsets   int    `json:"toolsets" yaml:"toolsets"`
	Policies   int    `json:"policies" yaml:"policies"`
	Plugins    int    `json:"plugins" yaml:"plugins"`
}

func (r registryReport) Table() Table {
	return Table{
		Columns: []string{"FILE", "ORG", "VERSION", "NS", "SERVERS", "BUNDLES", "AUDIENCES", "POLICIES", "PLUGINS"},
		Rows: [][]string{{
			r.File, r.Org, fmt.Sprint(r.Version),
			fmt.Sprint(r.Namespaces), fmt.Sprint(r.Servers), fmt.Sprint(r.Toolsets),
			fmt.Sprint(r.Policies), fmt.Sprint(r.Plugins),
		}},
		Notes: []string{"document is valid"},
	}
}

func newRegistryHooksCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hooks",
		Short: "List the seven pipeline hooks in execution order",
		Long: "MCPDoll has exactly seven hooks. The set is closed: adding an eighth requires\n" +
			"an ADR, because every hook is a place plugin authors must reason about and a\n" +
			"place the request budget has to be divided.",
		Annotations: map[string]string{
			annotationOperation: "listHooks",
		},
		RunE: func(_ *cobra.Command, _ []string) error {
			return env.Emit(hooksReport{Hooks: registry.HookNames()})
		},
	}
	return cmd
}

type hooksReport struct {
	Hooks []string `json:"hooks" yaml:"hooks"`
}

func (r hooksReport) Table() Table {
	rows := make([][]string, 0, len(r.Hooks))
	for i, h := range r.Hooks {
		rows = append(rows, []string{fmt.Sprint(i + 1), h})
	}
	return Table{
		Columns: []string{"#", "HOOK"},
		Rows:    rows,
		Notes:   []string{"execution order; the set is closed at seven (see docs/adr/0007-seven-hooks.md)"},
	}
}
