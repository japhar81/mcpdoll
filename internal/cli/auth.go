// Copyright 2026 Henry Zektser.

package cli

import (
	"fmt"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/mcpdoll/mcpdoll/internal/api"
)

// Signing in, and asking what a credential may do.
//
// The control plane resolves three credentials — a session, an API key, and the
// static configuration token — and runs every operation through the caller's
// grants (ADR 0022). These commands are how an operator gets one of the first
// two, so their laptop holds a credential scoped to them rather than the
// deployment's master token.

func newAuthCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Sign in, and see what your credential may do",
		Long: "MCPDoll's control plane enforces the same grants the gateway does. A session\n" +
			"is a person; an API key is an agent; the static configuration token is\n" +
			"break-glass and holds everything.",
	}
	cmd.AddCommand(newAuthLoginCmd(env), newAuthWhoamiCmd(env), newAuthLogoutCmd(env))
	return cmd
}

func newAuthLoginCmd(env *Env) *cobra.Command {
	var tenant, email, password string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in and print a session token",
		Long: "The tenant is part of the identity, not a lookup hint: the same email in two\n" +
			"tenants is two different people.\n\n" +
			"The token is printed once and never stored by this command. Export it as\n" +
			"MCPDOLL_TOKEN, or put its name in your profile's token_ref — a config file\n" +
			"is world-readable often enough that writing a credential into one is a bad\n" +
			"default.\n\n" +
			"With no --password, it is read from the terminal without echoing. Passing it\n" +
			"as a flag puts it in your shell history, which is why the prompt is the\n" +
			"default rather than the fallback.",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationOperation: "login"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := apiContext(cmd.Context())
			defer cancel()

			if password == "" {
				var err error
				if password, err = readPassword(env, email); err != nil {
					return err
				}
			}

			var out api.Session
			body := map[string]string{"tenant": tenant, "email": email, "password": password}
			if err := apiCall(ctx, env, "POST", "/api/v1/auth/login", body, &out); err != nil {
				return err
			}

			env.Printf("signed in as %s in %s; the token below is shown once\n",
				out.User.Email, out.User.Tenant)
			env.Printf("export MCPDOLL_TOKEN=<token>\n")
			return env.Emit(sessionReport(out))
		},
	}
	cmd.Flags().StringVar(&tenant, "tenant", "", "tenant slug (required)")
	cmd.Flags().StringVar(&email, "email", "", "your email (required)")
	cmd.Flags().StringVar(&password, "password", "",
		"password; omit to be prompted without echo")
	_ = cmd.MarkFlagRequired("tenant")
	_ = cmd.MarkFlagRequired("email")
	return cmd
}

// readPassword prompts without echoing.
//
// Refuses when stdin is not a terminal rather than reading a line: a password
// arriving on a pipe is one somebody put in a script, and silently accepting it
// would make that the path of least resistance.
func readPassword(env *Env, email string) (string, error) {
	fd := int(syscall.Stdin)
	if !term.IsTerminal(fd) {
		return "", usageError(fmt.Errorf(
			"stdin is not a terminal, so there is nowhere to prompt: pass --password " +
				"explicitly if you meant to script this"))
	}
	fmt.Fprintf(env.Err, "password for %s: ", email)
	raw, err := term.ReadPassword(fd)
	fmt.Fprintln(env.Err)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

type sessionReport api.Session

func (r sessionReport) Table() Table {
	return Table{
		Columns: []string{"USER", "TENANT", "EXPIRES", "TOKEN"},
		Rows:    [][]string{{r.User.Email, r.User.Tenant, r.ExpiresAt, r.Token}},
		Notes: []string{
			fmt.Sprintf("%d grant(s); run `mcpdoll auth whoami` to see them", len(r.Grants)),
			"nothing keeps this token — a lost one means signing in again",
		},
	}
}

func newAuthWhoamiCmd(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Who your credential is, and what it may do",
		Long: "Also the fastest way to tell which of the three credentials you are actually\n" +
			"using. A `static` kind means the deployment's break-glass token, which holds\n" +
			"everything — worth knowing before assuming a permission check passed for a\n" +
			"reason.",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationOperation: "getSession"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := apiContext(cmd.Context())
			defer cancel()

			var out api.SessionInfo
			if err := apiCall(ctx, env, "GET", "/api/v1/auth/session", nil, &out); err != nil {
				return err
			}
			return env.Emit(sessionInfoReport(out))
		},
	}
}

type sessionInfoReport api.SessionInfo

func (r sessionInfoReport) Table() Table {
	rows := make([][]string, 0, len(r.Grants))
	for _, g := range r.Grants {
		rows = append(rows, []string{g.Role, g.Scope})
	}
	notes := []string{
		fmt.Sprintf("%s as %s", r.Kind, r.Subject),
	}
	if r.Tenant != "" {
		notes = append(notes, "tenant "+r.Tenant)
	}
	switch {
	case r.Kind == "static":
		notes = append(notes,
			"this is the deployment's break-glass token: it holds every permission,",
			"and every use of it is logged")
	case len(r.Permissions) > 0:
		notes = append(notes, "global permissions: "+strings.Join(r.Permissions, " "))
	default:
		notes = append(notes,
			"no permissions at global scope — anything held is scoped to a tenant,",
			"which is the normal shape for a tenant administrator")
	}
	return Table{Columns: []string{"ROLE", "SCOPE"}, Rows: rows, Notes: notes}
}

func newAuthLogoutCmd(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "End the current session",
		Long: "Revokes the session and publishes a revocation list, so it stops working at\n" +
			"once rather than at its expiry.",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationOperation: "logout"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := apiContext(cmd.Context())
			defer cancel()

			if err := apiCall(ctx, env, "DELETE", "/api/v1/auth/session", nil, nil); err != nil {
				return err
			}
			env.Printf("signed out; unset MCPDOLL_TOKEN\n")
			return nil
		},
	}
}

// ----------------------------------------------------------- revocations ----

func newRevocationsCmd(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "revocations",
		Short: "What has been revoked, and whether the gateway has applied it",
		Long: "Two versions, and the gap between them is the exposure. The control plane\n" +
			"publishes a signed list; the gateway applies it; until it does, a revoked\n" +
			"credential still works.\n\n" +
			"Exits 5 when the gateway has not caught up, so a script can wait on it.",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationOperation: "getRevocations"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := apiContext(cmd.Context())
			defer cancel()

			var out api.RevocationReport
			if err := apiCall(ctx, env, "GET", "/api/v1/revocations", nil, &out); err != nil {
				return err
			}
			if err := env.Emit(revocationReport(out)); err != nil {
				return err
			}
			if !out.InEffect {
				// A non-zero exit, because "revoked but not yet in effect" is
				// exactly the state a script waiting on a revocation must not
				// mistake for success.
				return unavailableError(fmt.Errorf(
					"the gateway is applying list %d; the control plane published %d",
					out.ServingVersion, out.Version))
			}
			return nil
		},
	}
}

type revocationReport api.RevocationReport

func (r revocationReport) Table() Table {
	rows := make([][]string, 0, len(r.Revocations))
	for _, e := range r.Revocations {
		reason := e.Reason
		if reason == "" {
			reason = "—"
		}
		rows = append(rows, []string{e.PrincipalID, e.Kind, reason, e.RevokedAt})
	}

	state := "in effect"
	if !r.InEffect {
		state = "NOT YET IN EFFECT"
	}
	notes := []string{
		fmt.Sprintf("published %d, gateway applying %d — %s",
			r.Version, r.ServingVersion, state),
		fmt.Sprintf("the gateway's list is %ss old; that is how long a revoked "+
			"credential keeps working", strconv.FormatFloat(r.ServingAgeSeconds, 'f', 0, 64)),
	}
	if r.Warning != "" {
		notes = append(notes, r.Warning)
	}
	return Table{
		Columns: []string{"PRINCIPAL", "KIND", "REASON", "REVOKED"},
		Rows:    rows,
		Notes:   notes,
	}
}
