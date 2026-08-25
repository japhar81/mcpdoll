// Copyright 2026 Henry Zektser.

package cli

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mcpdoll/mcpdoll/internal/api"
)

// Tenants, users, grants, and API keys, over the control-plane API.
//
// Every one of these resolves a slug or an email to a uuid by listing first,
// because a uuid is not something an operator has in their hand and a CLI that
// demanded one would be unusable. The extra request is the price of a command
// somebody can actually type.

// ------------------------------------------------------------------ tenants --

func newTenantsCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tenants",
		Short: "Tenants: the isolation boundary users and backends belong to",
		Long: "A tenant owns its users, and every scope string those users are granted at\n" +
			"names it. The same backend serves many tenants at different addresses, so a\n" +
			"tenant is a routing fact as well as an ownership one.",
	}
	cmd.AddCommand(
		newTenantsListCmd(env),
		newTenantsCreateCmd(env),
		newTenantsDeleteCmd(env),
	)
	return cmd
}

func newTenantsListCmd(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:         "list",
		Short:       "Every tenant, with what the registry and the snapshot say about it",
		Annotations: map[string]string{annotationOperation: "listTenants"},
		Args:        cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := apiContext(cmd.Context())
			defer cancel()

			var out api.TenantList
			if err := apiCall(ctx, env, "GET", "/api/v1/tenants", nil, &out); err != nil {
				return err
			}
			return env.Emit(tenantListReport(out))
		},
	}
}

type tenantListReport api.TenantList

func (r tenantListReport) Table() Table {
	rows := make([][]string, 0, len(r.Registered))
	for _, t := range r.Registered {
		rows = append(rows, []string{
			t.Slug, t.Name, t.Status,
			strconv.Itoa(t.Users), strconv.Itoa(t.Backends), strconv.Itoa(t.Tools),
		})
	}

	notes := []string{fmt.Sprintf(
		"the gateway is serving snapshot %d across %d tenant(s), %d admitted tool(s)",
		r.SnapshotVersion, r.Tenants, r.Tools)}
	if !r.Ready {
		notes = append(notes,
			"the data plane did not answer, so the serving counts above are zero;",
			"the table is still what the control plane knows")
	}
	for _, t := range r.Registered {
		switch {
		case t.Status == "unregistered":
			notes = append(notes, fmt.Sprintf(
				"%s is bound by the registry but no tenant record exists — nothing can "+
					"authenticate into it", t.Slug))
		case t.Backends == 0:
			notes = append(notes, fmt.Sprintf(
				"%s has no backend bindings, so no tool can reach it whatever its users "+
					"are granted", t.Slug))
		}
	}
	return Table{
		Columns: []string{"SLUG", "NAME", "STATUS", "USERS", "BACKENDS", "TOOLS"},
		Rows:    rows,
		Notes:   notes,
	}
}

func newTenantsCreateCmd(env *Env) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "create <slug>",
		Short: "Create a tenant",
		Long: "The slug appears verbatim in every scope string this tenant's grants use, so\n" +
			"it cannot be changed afterwards: renaming would orphan every grant naming it.",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperation: "createTenant"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := apiContext(cmd.Context())
			defer cancel()

			if name == "" {
				name = args[0]
			}
			var out api.Tenant
			body := map[string]string{"slug": args[0], "name": name}
			if err := apiCall(ctx, env, "POST", "/api/v1/tenants", body, &out); err != nil {
				return err
			}
			env.Printf("tenant %s created; it has no users and no backend bindings yet\n", out.Slug)
			return env.Emit(tenantReport(out))
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "display name (defaults to the slug)")
	return cmd
}

type tenantReport api.Tenant

func (r tenantReport) Table() Table {
	return Table{
		Columns: []string{"SLUG", "NAME", "STATUS", "ID"},
		Rows:    [][]string{{r.Slug, r.Name, r.Status, r.ID}},
	}
}

func newTenantsDeleteCmd(env *Env) *cobra.Command {
	var confirm bool
	cmd := &cobra.Command{
		Use:   "delete <slug>",
		Short: "Delete a tenant and everything it owns",
		Long: "Cascades to the tenant's users, their grants, and their API keys. There is no\n" +
			"undo, and --yes is required because a slug is easy to mistype.",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperation: "deleteTenant"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := apiContext(cmd.Context())
			defer cancel()

			tenant, err := resolveTenant(ctx, env, args[0])
			if err != nil {
				return err
			}
			if !confirm {
				return usageError(fmt.Errorf(
					"this deletes tenant %s along with its %d user(s), their grants, and "+
						"their API keys; pass --yes to confirm", tenant.Slug, tenant.Users))
			}
			if err := apiCall(ctx, env, "DELETE", "/api/v1/tenants/"+tenant.ID, nil, nil); err != nil {
				return err
			}
			env.Printf("tenant %s deleted\n", tenant.Slug)
			return nil
		},
	}
	cmd.Flags().BoolVar(&confirm, "yes", false, "confirm the deletion")
	return cmd
}

// -------------------------------------------------------------------- users --

func newUsersCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "users",
		Short: "Users, their grants, and their API keys",
		Long: "A user is the unit access is granted to. What an agent sees is decided by the\n" +
			"grants of whoever owns the key it presents, so this is where a catalog is\n" +
			"actually shaped.",
	}
	cmd.AddCommand(
		newUsersListCmd(env),
		newUsersCreateCmd(env),
		newUsersAllCmd(env),
		newUsersShowCmd(env),
		newUsersUpdateCmd(env),
		newUsersDeleteCmd(env),
		newUsersGrantsCmd(env),
		newUsersKeysCmd(env),
	)
	return cmd
}

func newUsersListCmd(env *Env) *cobra.Command {
	var tenantSlug string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Users, optionally narrowed to those granted into one tenant",
		Long: "Without --tenant, everybody. With it, the users whose grants reach that\n" +
			"tenant — which is the only sense in which a user belongs to one.",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationOperation: "listUsers"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := apiContext(cmd.Context())
			defer cancel()

			path := "/api/v1/users"
			if tenantSlug != "" {
				tenant, err := resolveTenant(ctx, env, tenantSlug)
				if err != nil {
					return err
				}
				path = "/api/v1/tenants/" + tenant.ID + "/users"
			}
			var out api.UserList
			if err := apiCall(ctx, env, "GET", path, nil, &out); err != nil {
				return err
			}
			return env.Emit(userListReport(out))
		},
	}
	cmd.Flags().StringVar(&tenantSlug, "tenant", "",
		"narrow to users granted into this tenant")
	return cmd
}

type userListReport api.UserList

func (r userListReport) Table() Table {
	rows := make([][]string, 0, len(r.Users))
	for _, u := range r.Users {
		password := "no"
		if u.HasPassword {
			password = "yes"
		}
		rows = append(rows, []string{u.Email, u.DisplayName, u.Status, password, u.ID})
	}
	return Table{
		Columns: []string{"EMAIL", "NAME", "STATUS", "PASSWORD", "ID"},
		Rows:    rows,
		Notes:   []string{tenantNote(r.Tenant)},
	}
}

func newUsersCreateCmd(env *Env) *cobra.Command {
	var displayName, password string
	cmd := &cobra.Command{
		Use:   "create <email>",
		Short: "Add a user",
		Long: "A user is a person, not a person in a tenant. Which tenants they reach is\n" +
			"what their grants say, so a new account reaches nothing until you grant it\n" +
			"something.\n\n" +
			"--password is optional. A user who signs in through an identity provider has\n" +
			"none, and a service identity that only holds API keys does not need one.",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperation: "createUser"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := apiContext(cmd.Context())
			defer cancel()

			body := map[string]string{"email": args[0]}
			if displayName != "" {
				body["display_name"] = displayName
			}
			if password != "" {
				body["password"] = password
			}
			var out api.User
			if err := apiCall(ctx, env, "POST", "/api/v1/users", body, &out); err != nil {
				return err
			}
			env.Printf("user %s created; they reach nothing until granted — "+
				"`mcpdoll users grants set`\n", out.Email)
			return env.Emit(userReport(out))
		},
	}
	cmd.Flags().StringVar(&displayName, "name", "", "display name")
	cmd.Flags().StringVar(&password, "password", "", "local password; omit for SSO or key-only identities")
	return cmd
}

type userReport api.User

func (r userReport) Table() Table {
	password := "no"
	if r.HasPassword {
		password = "yes"
	}
	return Table{
		Columns: []string{"EMAIL", "NAME", "STATUS", "PASSWORD", "ID"},
		Rows:    [][]string{{r.Email, r.DisplayName, r.Status, password, r.ID}},
	}
}

func newUsersShowCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:         "show <email>",
		Short:       "One user",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperation: "getUser"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := apiContext(cmd.Context())
			defer cancel()

			user, err := resolveUser(ctx, env, args[0])
			if err != nil {
				return err
			}
			var out api.User
			if err := apiCall(ctx, env, "GET", "/api/v1/users/"+user.ID, nil, &out); err != nil {
				return err
			}
			return env.Emit(userReport(out))
		},
	}
	return cmd
}

func newUsersUpdateCmd(env *Env) *cobra.Command {
	var displayName, status string
	cmd := &cobra.Command{
		Use:   "update <email>",
		Short: "Change a user's display name or status",
		Long: "Setting --status disabled stops the user's API keys too: a key's effective\n" +
			"grants are recomputed from its owner at every resolution, which is what makes\n" +
			"offboarding one operation rather than a hunt through credentials.",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperation: "updateUser"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := apiContext(cmd.Context())
			defer cancel()

			user, err := resolveUser(ctx, env, args[0])
			if err != nil {
				return err
			}
			// Unset flags keep what is already there. A PATCH that silently
			// blanked the display name because the operator only wanted to
			// disable an account would be a surprising way to lose data.
			if status == "" {
				status = user.Status
			}
			if !cmd.Flags().Changed("name") {
				displayName = user.DisplayName
			}
			body := map[string]string{"status": status}
			if displayName != "" {
				body["display_name"] = displayName
			}
			var out api.User
			if err := apiCall(ctx, env, "PATCH", "/api/v1/users/"+user.ID, body, &out); err != nil {
				return err
			}
			if out.Status == "disabled" {
				env.Printf("%s is disabled; every API key they hold stops resolving\n", out.Email)
			}
			return env.Emit(userReport(out))
		},
	}
	cmd.Flags().StringVar(&displayName, "name", "", "display name")
	cmd.Flags().StringVar(&status, "status", "", "active or disabled")
	_ = cmd.MarkFlagRequired("tenant")
	return cmd
}

func newUsersDeleteCmd(env *Env) *cobra.Command {
	var confirm bool
	cmd := &cobra.Command{
		Use:   "delete <email>",
		Short: "Delete a user and everything they hold",
		Long: "Distinct from `users update --status disabled`, and both are useful.\n\n" +
			"Disable is the offboarding path: the row stays, so an audit can still answer\n" +
			"who did what. Delete is for a row that should never have existed — a typo, an\n" +
			"account created in the wrong tenant — where keeping it is not history, it is a\n" +
			"decoy the next person has to work out is inert.",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperation: "deleteUser"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := apiContext(cmd.Context())
			defer cancel()

			user, err := resolveUser(ctx, env, args[0])
			if err != nil {
				return err
			}
			if !confirm {
				return usageError(fmt.Errorf(
					"this deletes %s along with every grant and API key they hold; "+
						"pass --yes to confirm, or use `users update --status disabled` "+
						"to keep the record", user.Email))
			}
			if err := apiCall(ctx, env, "DELETE", "/api/v1/users/"+user.ID, nil, nil); err != nil {
				return err
			}
			env.Printf("%s deleted; their credentials stop resolving within seconds\n", user.Email)
			return nil
		},
	}
	cmd.Flags().BoolVar(&confirm, "yes", false, "confirm the deletion")
	_ = cmd.MarkFlagRequired("tenant")
	return cmd
}

func newUsersAllCmd(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "all",
		Short: "Every user, across every tenant",
		Long: "A user is a person and belongs to no tenant, so this is the whole list.\n" +
			"`users list --tenant X` narrows it to those granted into one.",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationOperation: "listAllUsers"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := apiContext(cmd.Context())
			defer cancel()

			var out api.UserList
			if err := apiCall(ctx, env, "GET", "/api/v1/users", nil, &out); err != nil {
				return err
			}
			return env.Emit(userListReport(out))
		},
	}
}

// ------------------------------------------------------------------- grants --

func newUsersGrantsCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "grants <email>",
		Short: "What a user holds directly",
		Long: "Directly: an API key's effective grants are the intersection of what the key\n" +
			"declares with this set, recomputed at every resolution — so a key can narrow\n" +
			"what its owner holds but never widen it.",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperation: "listGrants"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := apiContext(cmd.Context())
			defer cancel()

			user, err := resolveUser(ctx, env, args[0])
			if err != nil {
				return err
			}
			var out api.GrantList
			if err := apiCall(ctx, env, "GET",
				"/api/v1/users/"+user.ID+"/grants", nil, &out); err != nil {
				return err
			}
			return env.Emit(grantListReport{GrantList: out, Email: user.Email})
		},
	}
	cmd.AddCommand(newUsersGrantsSetCmd(env))
	return cmd
}

type grantListReport struct {
	api.GrantList
	Email string `json:"email" yaml:"email"`
}

func (r grantListReport) Table() Table {
	rows := make([][]string, 0, len(r.Grants))
	for _, g := range r.Grants {
		rows = append(rows, []string{g.Role, g.Scope})
	}
	notes := []string{r.Email}
	if len(rows) == 0 {
		notes = append(notes,
			"no grants: this principal's catalog is empty, which is the correct",
			"state for somebody nobody has granted anything yet")
	}
	return Table{Columns: []string{"ROLE", "SCOPE"}, Rows: rows, Notes: notes}
}

func newUsersGrantsSetCmd(env *Env) *cobra.Command {
	var grants []string
	cmd := &cobra.Command{
		Use:   "set <email>",
		Short: "Set a user's grants to exactly this set",
		Long: "Declarative, not additive: anything not passed is revoked. The question being\n" +
			"answered is \"what should this person hold\", and expressing that as a sequence\n" +
			"of deltas is exactly how a revocation gets forgotten.\n\n" +
			"Each --grant is role@scope. Scopes nest:\n" +
			"  *                                  everything\n" +
			"  t/acme                             one tenant\n" +
			"  t/acme/ts/support                  one toolset in it\n" +
			"  t/acme/ts/support/lookup_customer  one tool\n\n" +
			"A tool scope names the backend's own tool, not the qualified name a\n" +
			"client sees: `lookup_customer`, not `crm.lookup_customer`. The prefix is\n" +
			"a namespace property, so a grant written with it would survive a\n" +
			"namespace rename by authorizing nothing. The server refuses that form\n" +
			"rather than storing it.\n\n" +
			"Passing no --grant revokes everything, which is how you strip an account\n" +
			"without deleting it.",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperation: "putGrants"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := apiContext(cmd.Context())
			defer cancel()

			user, err := resolveUser(ctx, env, args[0])
			if err != nil {
				return err
			}
			parsed, err := parseGrants(grants)
			if err != nil {
				return err
			}
			var out api.GrantList
			if err := apiCall(ctx, env, "PUT", "/api/v1/users/"+user.ID+"/grants",
				map[string]any{"grants": parsed}, &out); err != nil {
				return err
			}
			env.Printf("%s now holds %d grant(s); it takes effect for the data plane at "+
				"the next snapshot\n", user.Email, len(out.Grants))
			return env.Emit(grantListReport{GrantList: out, Email: user.Email})
		},
	}
	cmd.Flags().StringArrayVar(&grants, "grant", nil, "role@scope; repeatable")
	_ = cmd.MarkFlagRequired("tenant")
	return cmd
}

// parseGrants turns role@scope strings into grants.
//
// Split on the first `@`, not the last. A role name never contains one — the
// catalog's names are identifiers — but a tool name can, and a scope naming
// that tool would be silently cut in half by a split from the right, producing
// a grant that validates and authorizes something else.
func parseGrants(raw []string) ([]api.Grant, error) {
	out := make([]api.Grant, 0, len(raw))
	for _, entry := range raw {
		role, scope, found := strings.Cut(entry, "@")
		if !found || role == "" || scope == "" {
			return nil, usageError(fmt.Errorf(
				"--grant %q is not role@scope, for example tool_user@t/acme/ts/support", entry))
		}
		out = append(out, api.Grant{Role: role, Scope: scope})
	}
	return out, nil
}

// ----------------------------------------------------------------- api keys --

func newUsersKeysCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keys <email>",
		Short: "Every API key a user holds, revoked ones included",
		Long: "Revoked keys stay listed. A credential that was in use and is not any more is\n" +
			"the thing an incident review needs to see.\n\n" +
			"Distinct from `mcpdoll keys generate`, which mints a snapshot *signing* key.",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperation: "listAPIKeys"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := apiContext(cmd.Context())
			defer cancel()

			user, err := resolveUser(ctx, env, args[0])
			if err != nil {
				return err
			}
			var out api.APIKeyList
			if err := apiCall(ctx, env, "GET",
				"/api/v1/users/"+user.ID+"/keys", nil, &out); err != nil {
				return err
			}
			return env.Emit(apiKeyListReport{APIKeyList: out, Email: user.Email})
		},
	}
	cmd.AddCommand(newUsersKeysMintCmd(env), newUsersKeysRevokeCmd(env))
	return cmd
}

type apiKeyListReport struct {
	api.APIKeyList
	Email string `json:"email" yaml:"email"`
}

func (r apiKeyListReport) Table() Table {
	rows := make([][]string, 0, len(r.Keys))
	for _, k := range r.Keys {
		state := "active"
		switch {
		case k.RevokedAt != "":
			state = "revoked"
		case !k.Active:
			state = "expired"
		}
		used := k.LastUsedAt
		if used == "" {
			used = "never"
		}
		rows = append(rows, []string{
			k.Name, k.Prefix, state, strconv.Itoa(len(k.Declared)), used,
		})
	}
	return Table{
		Columns: []string{"NAME", "PREFIX", "STATE", "DECLARED", "LAST USED"},
		Rows:    rows,
		Notes: []string{
			r.Email,
			"DECLARED is what each key asks for; effective grants are that",
			"intersected with the owner's, recomputed at every resolution",
		},
	}
}

func newUsersKeysMintCmd(env *Env) *cobra.Command {
	var tenantSlug, keyTenant, name, expires string
	var grants []string
	var allTenants bool
	cmd := &cobra.Command{
		Use:   "mint <email>",
		Short: "Mint an API key",
		Long: "Prints the secret exactly once. Only its SHA-256 digest is stored, so a\n" +
			"caller who does not capture it has to mint another key — which is what makes\n" +
			"a leaked log harmless.\n\n" +
			"--grant narrows the key below its owner. Declaring more than the owner holds\n" +
			"is not an error and has no effect: the intersection happens at every\n" +
			"resolution, so a key can never widen access. Passing none means the key\n" +
			"carries whatever its owner holds.\n\n" +
			"--all-tenants mints a key whose catalog covers every tenant its grants\n" +
			"reach, with tool names qualified as <tenant>.<prefix>.<tool> — for asking\n" +
			"one model to compare the same tool across, say, test and live. Narrow it\n" +
			"with --grant to choose which tenants: the grants decide, so there is no\n" +
			"second list that could disagree with them.\n\n" +
			"Without it a key acts in exactly one tenant and cannot address another at\n" +
			"all. That isolation is structural rather than filtered, which is why it is\n" +
			"the default and why spanning takes key:manage at the global scope.",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperation: "mintAPIKey"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := apiContext(cmd.Context())
			defer cancel()

			user, err := resolveUser(ctx, env, args[0])
			if err != nil {
				return err
			}
			parsed, err := parseGrants(grants)
			if err != nil {
				return err
			}
			body := map[string]any{"name": name, "grants": parsed}
			switch {
			case allTenants:
				if keyTenant != "" {
					return fmt.Errorf(
						"--all-tenants and --acts-in contradict each other: a spanning " +
							"key names no tenant, its grants decide which it reaches")
				}
				body["spans_tenants"] = true
			default:
				if keyTenant == "" {
					keyTenant = tenantSlug
				}
				if keyTenant == "" {
					return fmt.Errorf(
						"--tenant is required: a key acts in exactly one tenant. " +
							"Pass --all-tenants for a key covering every tenant its grants reach")
				}
				body["tenant"] = keyTenant
			}
			if expires != "" {
				body["expires_at"] = expires
			}
			var out api.MintedAPIKey
			if err := apiCall(ctx, env, "POST",
				"/api/v1/users/"+user.ID+"/keys", body, &out); err != nil {
				return err
			}
			env.Printf("this secret is shown once and is not recoverable\n")
			return env.Emit(mintedKeyReport(out))
		},
	}
	cmd.Flags().StringVar(&tenantSlug, "tenant", "", "tenant to resolve the user in (required)")
	cmd.Flags().StringVar(&keyTenant, "acts-in", "",
		"tenant this key acts in; defaults to --tenant. An MCP session resolves to exactly one")
	cmd.Flags().BoolVar(&allTenants, "all-tenants", false,
		"span every tenant the grants reach, with tenant-qualified tool names")
	cmd.Flags().StringVar(&name, "name", "", "what this key is for (required)")
	cmd.Flags().StringArrayVar(&grants, "grant", nil, "role@scope to narrow the key to; repeatable")
	cmd.Flags().StringVar(&expires, "expires", "", "RFC 3339 expiry; omit for a key that does not expire")
	// --tenant is required unless the key spans, which is checked in RunE
	// rather than declared here: cobra's MarkFlagRequired cannot express
	// "required unless another flag is set", and marking it required outright
	// would make --all-tenants unusable.
	cmd.MarkFlagsMutuallyExclusive("tenant", "all-tenants")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

type mintedKeyReport api.MintedAPIKey

func (r mintedKeyReport) Table() Table {
	return Table{
		Columns: []string{"NAME", "PREFIX", "SECRET"},
		Rows:    [][]string{{r.Key.Name, r.Key.Prefix, r.Secret}},
		Notes: []string{
			"give the SECRET column to the agent — it is the whole credential",
			"and the prefix alone will not authenticate",
		},
	}
}

func newUsersKeysRevokeCmd(env *Env) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "revoke <email>",
		Short: "Revoke an API key",
		Long: "Marks the key revoked rather than deleting the row, so the credential stops\n" +
			"working and the record of it having existed does not.",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperation: "revokeAPIKey"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := apiContext(cmd.Context())
			defer cancel()

			user, err := resolveUser(ctx, env, args[0])
			if err != nil {
				return err
			}
			var keys api.APIKeyList
			if err := apiCall(ctx, env, "GET",
				"/api/v1/users/"+user.ID+"/keys", nil, &keys); err != nil {
				return err
			}
			// Matched by name or by prefix. Both are things an operator has:
			// the name is what they called it, and the prefix is what an audit
			// log records.
			var found *api.APIKey
			for i, k := range keys.Keys {
				if k.Name == name || k.Prefix == name {
					found = &keys.Keys[i]
					break
				}
			}
			if found == nil {
				return notFoundError(fmt.Errorf(
					"%s holds no key named or prefixed %q", user.Email, name))
			}
			if err := apiCall(ctx, env, "DELETE", "/api/v1/keys/"+found.ID, nil, nil); err != nil {
				return err
			}
			env.Printf("key %s (%s) revoked\n", found.Name, found.Prefix)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "key", "", "key name or prefix (required)")
	_ = cmd.MarkFlagRequired("tenant")
	_ = cmd.MarkFlagRequired("key")
	return cmd
}

// -------------------------------------------------------------------- roles --

func newRolesCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "roles",
		Short: "Compose roles out of the closed permission set",
		Long: "Permissions are a closed set: adding one is a schema and code change,\n" +
			"friction that exists because a permission set which grows casually stops\n" +
			"being reviewable.\n\n" +
			"Composing roles out of that set is ordinary administration, and these\n" +
			"commands do it. Two rules hold the line: you cannot put a permission in a\n" +
			"role that you do not hold, and you cannot grant a role that confers a\n" +
			"permission you do not hold *at that scope*. The second is the one that\n" +
			"matters — without it, defining a role would be a way to hand yourself\n" +
			"anything.",
	}
	cmd.AddCommand(
		newRolesListCmd(env),
		newRolesSetCmd(env),
		newRolesDeleteCmd(env),
	)
	return cmd
}

func newRolesListCmd(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:         "list",
		Short:       "The role catalog and every permission that exists",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationOperation: "listRoles"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := apiContext(cmd.Context())
			defer cancel()

			var out api.RoleCatalog
			if err := apiCall(ctx, env, "GET", "/api/v1/roles", nil, &out); err != nil {
				return err
			}
			return env.Emit(roleCatalogReport(out))
		},
	}
}

func newRolesSetCmd(env *Env) *cobra.Command {
	var permissions []string
	var description string

	cmd := &cobra.Command{
		Use:   "set <role>",
		Short: "Create a role, or redefine what it permits",
		Long: "Declarative, not additive: the permissions passed are what the role holds\n" +
			"afterwards, so removing one means sending the set without it. Expressing a\n" +
			"role as a sequence of deltas is exactly how a permission gets left behind on\n" +
			"one somebody thought they had narrowed.\n\n" +
			"Passing no --permission leaves a role that grants nothing, which is a\n" +
			"legitimate way to neutralize one without deleting it and breaking the\n" +
			"grants that name it.\n\n" +
			"`mcpdoll roles list` prints the permissions that exist. A built-in role can\n" +
			"be redefined but not deleted.",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperation: "putRole"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := apiContext(cmd.Context())
			defer cancel()

			body := map[string]any{"permissions": permissions}
			if permissions == nil {
				body["permissions"] = []string{}
			}
			if description != "" {
				body["description"] = description
			}
			var out api.Role
			if err := apiCall(ctx, env, "PUT",
				"/api/v1/roles/"+url.PathEscape(args[0]), body, &out); err != nil {
				return err
			}
			return env.Emit(roleReport(out))
		},
	}
	cmd.Flags().StringArrayVar(&permissions, "permission", nil,
		"a permission the role confers; repeatable, and the complete set")
	cmd.Flags().StringVar(&description, "description", "", "what the role is for")
	return cmd
}

func newRolesDeleteCmd(env *Env) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete <role>",
		Short: "Delete a role nobody holds",
		Long: "Refused while anybody holds it. A grant naming a deleted role authorizes\n" +
			"nothing, and the person who loses access is not the person running this —\n" +
			"they find out when their agent stops. Revoke the grants first.\n\n" +
			"A built-in cannot be deleted at all: the seed would recreate it on the next\n" +
			"boot. Narrow its permissions instead.",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperation: "deleteRole"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				return fmt.Errorf("refusing to delete %q without --yes", args[0])
			}
			ctx, cancel := apiContext(cmd.Context())
			defer cancel()

			if err := apiCall(ctx, env, "DELETE",
				"/api/v1/roles/"+url.PathEscape(args[0]), nil, nil); err != nil {
				return err
			}
			env.Printf("deleted role %s\n", args[0])
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm the deletion")
	return cmd
}

type roleReport api.Role

func (r roleReport) Table() Table {
	rows := [][]string{
		{"role", r.Name},
		{"builtin", map[bool]string{true: "yes — cannot be deleted", false: "no"}[r.Builtin]},
	}
	if r.Description != "" {
		rows = append(rows, []string{"description", r.Description})
	}
	for i, p := range r.Permissions {
		label := ""
		if i == 0 {
			label = "permissions"
		}
		rows = append(rows, []string{label, p})
	}
	if len(r.Permissions) == 0 {
		rows = append(rows, []string{"permissions", "(none — this role grants nothing)"})
	}
	return Table{Columns: []string{"FIELD", "VALUE"}, Rows: rows}
}

type roleCatalogReport api.RoleCatalog

func (r roleCatalogReport) Table() Table {
	rows := make([][]string, 0, len(r.Roles))
	for _, role := range r.Roles {
		rows = append(rows, []string{
			role.Name,
			strconv.Itoa(len(role.Permissions)),
			strings.Join(role.Permissions, " "),
		})
	}
	return Table{
		Columns: []string{"ROLE", "COUNT", "PERMISSIONS"},
		Rows:    rows,
		Notes: []string{fmt.Sprintf(
			"%d permission(s) exist in total: %s",
			len(r.Permissions), strings.Join(r.Permissions, " "))},
	}
}

// --------------------------------------------------------------- resolution --

// resolveTenant turns a slug into the tenant summary carrying its id.
func resolveTenant(ctx context.Context, env *Env, slug string) (api.TenantSummary, error) {
	var list api.TenantList
	if err := apiCall(ctx, env, "GET", "/api/v1/tenants", nil, &list); err != nil {
		return api.TenantSummary{}, err
	}
	for _, t := range list.Registered {
		if t.Slug != slug {
			continue
		}
		if t.ID == "" {
			// Bound by the registry, but no record exists. Saying "not found"
			// would be misleading when the slug is visibly in the config.
			return api.TenantSummary{}, notFoundError(fmt.Errorf(
				"the registry binds tenant %q but no tenant record exists; create it "+
					"with `mcpdoll tenants create %s`", slug, slug))
		}
		return t, nil
	}
	return api.TenantSummary{}, notFoundError(fmt.Errorf("no tenant with slug %q", slug))
}

// resolveUser turns a tenant slug and an email into the user carrying their id.
// resolveUser turns an email into the user carrying their id.
//
// No tenant, because a user has none: an email identifies one person across the
// install.
func resolveUser(ctx context.Context, env *Env, email string) (api.User, error) {
	var list api.UserList
	if err := apiCall(ctx, env, "GET", "/api/v1/users", nil, &list); err != nil {
		return api.User{}, err
	}
	for _, u := range list.Users {
		if u.Email == email {
			return u, nil
		}
	}
	return api.User{}, notFoundError(fmt.Errorf("no user %q", email))
}

// tenantNote labels a user listing with its scope.
func tenantNote(tenant string) string {
	if tenant == "" {
		return "every user; --tenant narrows to those granted into one"
	}
	return "granted into " + tenant
}
