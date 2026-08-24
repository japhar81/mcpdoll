// Copyright 2026 Henry Zektser.

package cli

import (
	"encoding/base64"
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/mcpdoll/mcpdoll/internal/controlplane/registry"
	"github.com/mcpdoll/mcpdoll/internal/controlplane/snapshotter"
	"github.com/mcpdoll/mcpdoll/internal/controlplane/store"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/snapshot"
	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

func newSnapshotCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Build, inspect, and verify serving snapshots",
		Long: "A snapshot is the signed serving configuration the data plane runs from.\n" +
			"It is the only channel from the control plane to the data plane, so it is\n" +
			"both the unit of deployment and the unit of rollback.",
	}
	cmd.AddCommand(
		newSnapshotBuildCmd(env),
		newSnapshotInspectCmd(env),
		newSnapshotCurrentCmd(env),
		newSnapshotVerifyCmd(env),
	)
	return cmd
}

// ------------------------------------------------------------------ build ----

func newSnapshotBuildCmd(env *Env) *cobra.Command {
	var (
		registryPath     string
		keyPath          string
		keyID            string
		outPath          string
		discoverTimeout  time.Duration
		concurrency      int
		allowUnreachable bool
		dryRun           bool
		databaseURL      string
		version          int64
	)

	cmd := &cobra.Command{
		Use:   "build",
		Short: "Resolve a registry document into a signed snapshot",
		Long: "Discovers every backend the registry names, canonicalizes what they publish,\n" +
			"resolves toolsets and per-tenant bindings, validates the result, and signs\n" +
			"it.\n\n" +
			"Every problem is a build failure. A snapshot that some data-plane instances\n" +
			"would refuse is worse than no snapshot at all.",
		Example: "  mcpdoll snapshot build -r deploy/local/registry.yaml \\\n" +
			"      --key deploy/local/dev.key --key-id dev --out deploy/local/snapshot.pb",
		Annotations: map[string]string{
			// Read by the parity check: this is the operation this command
			// satisfies. See docs/adr/0004-api-first-tri-surface.md.
			annotationOperation: "buildSnapshot",
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			spec, err := registry.Load(registryPath)
			if err != nil {
				return configError(err)
			}

			priv, err := snapshot.LoadPrivateKey(keyPath)
			if err != nil {
				return configError(err)
			}
			signer, err := snapshot.NewSigner(keyID, priv)
			if err != nil {
				return configError(err)
			}

			opts := snapshotter.Options{
				Spec:             spec,
				Signer:           signer,
				DiscoverTimeout:  discoverTimeout,
				Concurrency:      concurrency,
				AllowUnreachable: allowUnreachable,
				Version:          version,
			}

			// Tenancy and RBAC live in the database, and a registry that binds
			// any tenant cannot be built without them. Reading it here rather
			// than requiring the API means a snapshot can be built where the
			// signing key lives, which is the whole reason this command exists
			// alongside the operation.
			dsn := firstNonEmpty(databaseURL, os.Getenv("MCPDOLL_DATABASE_URL"))
			if dsn != "" {
				db, err := store.Open(cmd.Context(), dsn)
				if err != nil {
					return configError(err)
				}
				defer db.Close()

				state, err := db.SnapshotState(cmd.Context())
				if err != nil {
					return configError(err)
				}
				opts.Tenants = state.Tenants
				env.Printf("carrying %d tenant(s) from the database\n", len(state.Tenants))
			} else if bindsAnyTenant(spec) {
				return configError(fmt.Errorf(
					"this registry binds backends to tenants, so the build needs the " +
						"database those tenants live in: pass --database-url or set " +
						"MCPDOLL_DATABASE_URL"))
			}

			env.Printf("discovering %d backend(s)…\n", len(spec.Servers))
			result, err := snapshotter.Build(cmd.Context(), opts)
			if err != nil {
				return validationError(err)
			}

			if !dryRun {
				if outPath == "" {
					return usageError(fmt.Errorf("--out is required unless --dry-run is set"))
				}
				if err := snapshot.WriteSignedSnapshot(outPath, result.Signed); err != nil {
					return err
				}
				env.Printf("wrote %s\n", outPath)
			}

			return env.Emit(buildReport{
				Version:        result.Snapshot.Version,
				SnapshotID:     result.Snapshot.Id,
				RegistryDigest: result.Snapshot.RegistryDigest,
				KeyID:          signer.KeyID(),
				PublicKey:      snapshot.TrustedKeyEntry(signer.KeyID(), signer.PublicKey()),
				Namespaces:     len(result.Snapshot.Namespaces),
				Servers:        len(result.Snapshot.Servers),
				Tools:          len(result.Snapshot.Tools),
				Toolsets:       len(result.Snapshot.Toolsets),
				Plugins:        len(result.Snapshot.Plugins),
				Backends:       result.Discovered,
				Warnings:       result.Warnings,
				Output:         outPath,
				DryRun:         dryRun,
			})
		},
	}

	cmd.Flags().StringVarP(&registryPath, "registry", "r", "registry.yaml",
		"registry document to resolve")
	cmd.Flags().StringVar(&keyPath, "key", "", "Ed25519 private key file (base64)")
	cmd.Flags().StringVar(&keyID, "key-id", "dev",
		"key id recorded in the snapshot, so verifiers can select the right public key")
	cmd.Flags().StringVar(&outPath, "out", "", "where to write the signed snapshot")
	cmd.Flags().DurationVar(&discoverTimeout, "discover-timeout", 15*time.Second,
		"per-backend discovery timeout")
	cmd.Flags().IntVar(&concurrency, "concurrency", 8,
		"how many backends to discover at once")
	cmd.Flags().BoolVar(&allowUnreachable, "allow-unreachable", false,
		"build even if a backend cannot be reached, omitting its tools")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"validate and report without writing a file")
	cmd.Flags().StringVar(&databaseURL, "database-url", "",
		"where tenants, users, and grants live; defaults to MCPDOLL_DATABASE_URL")
	cmd.Flags().Int64Var(&version, "version", 0,
		"snapshot version; 0 uses a Unix timestamp, which is monotonic without coordination")
	_ = cmd.MarkFlagRequired("key")

	return cmd
}

// buildReport is `snapshot build`'s result.
type buildReport struct {
	Version        int64                       `json:"version" yaml:"version"`
	SnapshotID     string                      `json:"snapshot_id" yaml:"snapshot_id"`
	Org            string                      `json:"org" yaml:"org"`
	RegistryDigest string                      `json:"registry_digest" yaml:"registry_digest"`
	KeyID          string                      `json:"key_id" yaml:"key_id"`
	PublicKey      string                      `json:"public_key" yaml:"public_key"`
	Namespaces     int                         `json:"namespaces" yaml:"namespaces"`
	Servers        int                         `json:"servers" yaml:"servers"`
	Tools          int                         `json:"tools" yaml:"tools"`
	Toolsets       int                         `json:"toolsets" yaml:"toolsets"`
	Plugins        int                         `json:"plugins" yaml:"plugins"`
	Backends       []snapshotter.BackendReport `json:"backends" yaml:"backends"`
	Warnings       []string                    `json:"warnings,omitempty" yaml:"warnings,omitempty"`
	Output         string                      `json:"output,omitempty" yaml:"output,omitempty"`
	DryRun         bool                        `json:"dry_run" yaml:"dry_run"`
}

func (r buildReport) Table() Table {
	rows := make([][]string, 0, len(r.Backends))
	for _, b := range r.Backends {
		version := b.NegotiatedVersion
		if version == "" {
			version = "-"
		}
		rows = append(rows, []string{
			b.ServerName,
			version,
			strconv.Itoa(len(b.Admitted)),
			strconv.Itoa(len(b.Excluded)),
			b.Endpoint,
		})
	}

	notes := []string{
		fmt.Sprintf("snapshot %d (%s): %d namespaces, %d servers, %d tools, %d toolsets, %d plugins",
			r.Version, r.SnapshotID, r.Namespaces, r.Servers, r.Tools, r.Toolsets, r.Plugins),
		fmt.Sprintf("signed with key %q", r.KeyID),
		"trusted-key entry for the data plane: " + r.PublicKey,
	}
	if r.DryRun {
		notes = append(notes, "dry run: nothing was written")
	} else if r.Output != "" {
		notes = append(notes, "wrote "+r.Output)
	}
	for _, w := range r.Warnings {
		notes = append(notes, "warning: "+w)
	}

	return Table{
		Columns: []string{"BACKEND", "PROTOCOL", "ADMITTED", "EXCLUDED", "ENDPOINT"},
		Rows:    rows,
		Notes:   notes,
	}
}

// ---------------------------------------------------------------- inspect ----

func newSnapshotInspectCmd(env *Env) *cobra.Command {
	var showTools bool

	cmd := &cobra.Command{
		Use:   "inspect <file>",
		Short: "Show what a snapshot file contains",
		Long: "Reads a signed snapshot without verifying it, so a snapshot signed by a key\n" +
			"you do not hold can still be inspected. Use `snapshot verify` to check the\n" +
			"signature.",
		Args: cobra.ExactArgs(1),
		Annotations: map[string]string{
			annotationOperation: "inspectSnapshot",
		},
		RunE: func(_ *cobra.Command, args []string) error {
			signed, err := snapshot.ReadSignedSnapshot(args[0])
			if err != nil {
				return notFoundError(err)
			}
			// Inspect deliberately parses without verifying: an operator
			// diagnosing "why won't this activate" needs to see the contents,
			// and the contents are not trusted for anything here.
			snap, err := snapshot.ParseUnverified(signed)
			if err != nil {
				return err
			}
			view, err := snapshot.Build(snap)
			if err != nil {
				// Report it but still show what we can: an unservable snapshot is
				// exactly when inspection is most useful.
				env.Printf("warning: this snapshot would not activate: %v\n", err)
			}
			return env.Emit(newInspectReport(args[0], signed, snap, view, showTools))
		},
	}
	cmd.Flags().BoolVar(&showTools, "tools", false, "list every tool rather than summarising")
	return cmd
}

type inspectReport struct {
	File           string `json:"file" yaml:"file"`
	Version        int64  `json:"version" yaml:"version"`
	SnapshotID     string `json:"snapshot_id" yaml:"snapshot_id"`
	Org            string `json:"org" yaml:"org"`
	BuiltAt        string `json:"built_at" yaml:"built_at"`
	Age            string `json:"age" yaml:"age"`
	KeyID          string `json:"key_id" yaml:"key_id"`
	Algorithm      string `json:"algorithm" yaml:"algorithm"`
	RegistryDigest string `json:"registry_digest" yaml:"registry_digest"`
	Servable       bool   `json:"servable" yaml:"servable"`

	Tenants []tenantSummary `json:"tenants" yaml:"tenants"`
	Tools   []toolSummary   `json:"tools,omitempty" yaml:"tools,omitempty"`

	showTools bool
}

// tenantSummary is one tenant's slice of a snapshot.
//
// Not an audience: a snapshot admits tools per tenant, and what any principal
// in that tenant sees is a subset decided by their grants (ADR 0016).
type tenantSummary struct {
	Slug          string `json:"slug" yaml:"slug"`
	Name          string `json:"name" yaml:"name"`
	Tools         int    `json:"tools" yaml:"tools"`
	TokenEstimate int    `json:"token_estimate" yaml:"token_estimate"`
}

type toolSummary struct {
	QualifiedName string `json:"qualified_name" yaml:"qualified_name"`
	Backend       string `json:"backend" yaml:"backend"`
	EffectClass   string `json:"effect_class" yaml:"effect_class"`
	Tokens        int    `json:"token_estimate" yaml:"token_estimate"`
	Digest        string `json:"digest" yaml:"digest"`
}

func newInspectReport(
	file string,
	signed *snapshotpb.SignedSnapshot,
	snap *snapshotpb.Snapshot,
	view *snapshot.View,
	showTools bool,
) inspectReport {
	r := inspectReport{
		File:           file,
		Version:        snap.Version,
		SnapshotID:     snap.Id,
		KeyID:          signed.KeyId,
		Algorithm:      signed.Algorithm,
		RegistryDigest: snap.RegistryDigest,
		Servable:       view != nil,
		showTools:      showTools,
	}
	if snap.BuiltAt != nil {
		built := snap.BuiltAt.AsTime()
		r.BuiltAt = built.Format(time.RFC3339)
		r.Age = time.Since(built).Round(time.Second).String()
	}

	if view != nil {
		for _, slug := range view.TenantSlugs() {
			tenant := view.Tenant(slug)
			tools := view.ToolsForTenant(tenant.Id)
			tokens := 0
			for _, t := range tools {
				tokens += int(t.Def.TokenEstimate)
			}
			r.Tenants = append(r.Tenants, tenantSummary{
				Slug: slug, Name: tenant.Name,
				Tools: len(tools), TokenEstimate: tokens,
			})
		}
	}

	if showTools {
		servers := map[string]string{}
		for _, s := range snap.Servers {
			servers[s.Id] = s.Name
		}
		for _, t := range snap.Tools {
			r.Tools = append(r.Tools, toolSummary{
				QualifiedName: t.QualifiedName,
				Backend:       servers[t.ServerId],
				EffectClass:   registry.EffectClassName(t.EffectClass),
				Tokens:        int(t.TokenEstimate),
				Digest:        t.Digest,
			})
		}
		sort.Slice(r.Tools, func(i, j int) bool {
			return r.Tools[i].QualifiedName < r.Tools[j].QualifiedName
		})
	}
	return r
}

func (r inspectReport) Table() Table {
	if r.showTools {
		rows := make([][]string, 0, len(r.Tools))
		for _, t := range r.Tools {
			rows = append(rows, []string{
				t.QualifiedName, t.Backend, t.EffectClass,
				strconv.Itoa(t.Tokens), shortDigest(t.Digest),
			})
		}
		return Table{
			Title:   fmt.Sprintf("snapshot %d — %d tools", r.Version, len(r.Tools)),
			Columns: []string{"TOOL", "BACKEND", "EFFECT", "TOKENS", "DIGEST"},
			Rows:    rows,
		}
	}

	rows := make([][]string, 0, len(r.Tenants))
	for _, t := range r.Tenants {
		rows = append(rows, []string{
			t.Slug, t.Name, strconv.Itoa(t.Tools), strconv.Itoa(t.TokenEstimate),
		})
	}

	notes := []string{
		fmt.Sprintf("version %d (%s), built %s (%s ago)", r.Version, r.SnapshotID, r.BuiltAt, r.Age),
		fmt.Sprintf("signed with key %q using %s", r.KeyID, r.Algorithm),
		"registry digest " + shortDigest(r.RegistryDigest),
	}
	if !r.Servable {
		notes = append(notes, "WOULD NOT ACTIVATE: see the warning above")
	}
	return Table{
		Columns: []string{"TENANT", "NAME", "ADMITTED TOOLS", "TOKENS"},
		Rows:    rows,
		Notes:   notes,
	}
}

// ----------------------------------------------------------------- verify ----

func newSnapshotVerifyCmd(env *Env) *cobra.Command {
	var trustedKeys []string

	cmd := &cobra.Command{
		Use:   "verify <file>",
		Short: "Check a snapshot's signature against a trusted key",
		Long: "Verifies the signature over exactly the bytes in the file, then confirms the\n" +
			"snapshot would activate. Both checks matter: a correctly signed snapshot with\n" +
			"a dangling reference is refused by every data-plane instance.",
		Args: cobra.ExactArgs(1),
		Annotations: map[string]string{
			annotationOperation: "verifySnapshot",
		},
		RunE: func(_ *cobra.Command, args []string) error {
			verifier, err := snapshot.NewVerifier(trustedKeys)
			if err != nil {
				return configError(err)
			}
			signed, err := snapshot.ReadSignedSnapshot(args[0])
			if err != nil {
				return notFoundError(err)
			}
			snap, err := verifier.Verify(signed)
			if err != nil {
				return validationError(err)
			}
			view, err := snapshot.Build(snap)
			if err != nil {
				return validationError(wrapf(err, "signature is valid but the snapshot would not activate"))
			}
			return env.Emit(verifyReport{
				File:    args[0],
				Valid:   true,
				Version: snap.Version,
				KeyID:   signed.KeyId,
				Tenants: view.TenantSlugs(),
				Tools:   len(snap.Tools),
			})
		},
	}
	cmd.Flags().StringArrayVar(&trustedKeys, "trusted-key", nil,
		"trusted signing key as keyID:base64PublicKey (repeatable)")
	_ = cmd.MarkFlagRequired("trusted-key")
	return cmd
}

type verifyReport struct {
	File    string   `json:"file" yaml:"file"`
	Valid   bool     `json:"valid" yaml:"valid"`
	Version int64    `json:"version" yaml:"version"`
	KeyID   string   `json:"key_id" yaml:"key_id"`
	Tenants []string `json:"tenants" yaml:"tenants"`
	Tools   int      `json:"tools" yaml:"tools"`
}

func (r verifyReport) Table() Table {
	return Table{
		Columns: []string{"FILE", "VALID", "VERSION", "KEY", "TOOLS", "AUDIENCES"},
		Rows: [][]string{{
			r.File, strconv.FormatBool(r.Valid), strconv.FormatInt(r.Version, 10),
			r.KeyID, strconv.Itoa(r.Tools), fmt.Sprint(r.Tenants),
		}},
	}
}

// -------------------------------------------------------------------- keys ---

func newKeysCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keys",
		Short: "Manage snapshot signing keys",
	}
	cmd.AddCommand(newKeysGenerateCmd(env))
	return cmd
}

func newKeysGenerateCmd(env *Env) *cobra.Command {
	var (
		dir   string
		keyID string
	)
	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Mint an Ed25519 snapshot signing keypair",
		Long: "Writes <key-id>.key (private, 0600) and <key-id>.pub into the target directory.\n\n" +
			"The private key is NOT encrypted. This command exists for local development;\n" +
			"production keys must come from your key management system, because whoever\n" +
			"holds this key can publish configuration to every data-plane instance.",
		Annotations: map[string]string{
			annotationOperation: "generateSigningKey",
		},
		RunE: func(_ *cobra.Command, _ []string) error {
			pub, priv, err := snapshot.GenerateKey()
			if err != nil {
				return err
			}
			if err := snapshot.WriteKeyPair(dir, keyID, pub, priv); err != nil {
				return err
			}
			return env.Emit(keyReport{
				KeyID:      keyID,
				Directory:  dir,
				PublicKey:  base64.StdEncoding.EncodeToString(pub),
				TrustEntry: snapshot.TrustedKeyEntry(keyID, pub),
			})
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "deploy/local", "directory to write the keypair into")
	cmd.Flags().StringVar(&keyID, "key-id", "dev", "key id")
	return cmd
}

type keyReport struct {
	KeyID      string `json:"key_id" yaml:"key_id"`
	Directory  string `json:"directory" yaml:"directory"`
	PublicKey  string `json:"public_key" yaml:"public_key"`
	TrustEntry string `json:"trust_entry" yaml:"trust_entry"`
}

func (r keyReport) Table() Table {
	return Table{
		Columns: []string{"KEY_ID", "DIRECTORY"},
		Rows:    [][]string{{r.KeyID, r.Directory}},
		Notes: []string{
			"add this to the data plane's dataplane.trusted_signing_keys:",
			"  " + r.TrustEntry,
			"the private key is unencrypted — do not use it in production",
		},
	}
}

func shortDigest(d string) string {
	const prefix = "sha256:"
	if len(d) > len(prefix)+12 {
		return d[len(prefix) : len(prefix)+12]
	}
	return d
}

// bindsAnyTenant reports whether the registry names a tenant anywhere.
//
// The distinction matters for the error message. A registry with no bindings
// builds fine without a database — that is what this system did before tenancy
// existed — so demanding one unconditionally would break a legitimate use. A
// registry that *does* bind is unbuildable without it, and saying so here is
// better than the snapshotter's "this build does not carry tenant X", which
// reads as a registry problem.
func bindsAnyTenant(spec *registry.Spec) bool {
	for _, srv := range spec.Servers {
		if len(srv.Bindings) > 0 {
			return true
		}
	}
	return false
}
