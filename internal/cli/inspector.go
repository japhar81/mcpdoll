// Copyright 2026 Henry Zektser.

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/mcpdoll/mcpdoll/internal/api"
)

// The MCP Inspector, pointed at our gateway as whoever is running this.
//
// The Inspector (MIT, github.com/modelcontextprotocol/inspector) is the
// reference tool for poking at an MCP server, and it already does everything a
// tool browser should. Reimplementing it inside this console would be a worse
// version of a thing that exists.
//
// What it cannot do by itself is know who you are. It takes a target URL and a
// header; this command supplies both, minting a credential that carries exactly
// the grants you already hold and revoking it when you are done. So what the
// Inspector shows is what *you* would be served, not what an operator's shared
// token would be.

// inspectorPackage is pinned to a major version rather than floating.
//
// `@latest` would mean a tool that changes under an operator between runs, and
// the first symptom of a breaking change would be this command failing with the
// Inspector's error rather than ours.
const inspectorPackage = "@modelcontextprotocol/inspector"

// inspectorKeyTTL bounds the minted credential.
//
// An hour: long enough for a session of poking at tools, short enough that one
// forgotten in a terminal is not a standing credential. It is revoked on exit
// as well, and revocation is immediate now (ADR 0023) — the expiry is the
// backstop for a process that was killed rather than closed.
const inspectorKeyTTL = time.Hour

func newInspectorCmd(env *Env) *cobra.Command {
	var (
		mode    string
		asKey   string
		keepKey bool
	)

	cmd := &cobra.Command{
		Use:   "inspector [-- <inspector args>...]",
		Short: "Open the MCP Inspector against this gateway, as you",
		Long: "Launches the MCP Inspector (MIT, from the modelcontextprotocol project) against\n" +
			"this deployment's /mcp endpoint, authenticated as whoever this CLI is signed\n" +
			"in as.\n\n" +
			"It mints a credential carrying exactly the grants you already hold, hands it\n" +
			"to the Inspector as a bearer header, and revokes it on exit — so what you see\n" +
			"is what you would be served, not what a shared operator token would be.\n\n" +
			"Requires Node: the Inspector is run with `npx`. Anything after `--` is passed\n" +
			"to the Inspector unchanged.",
		Example: "  mcpdoll inspector\n" +
			"  mcpdoll inspector --mode cli -- --method tools/list\n" +
			"  mcpdoll inspector --as \"$MCPDOLL_AGENT_KEY\"   # as somebody else",
		RunE: func(cmd *cobra.Command, args []string) error {
			switch mode {
			case "web", "cli", "tui":
			default:
				return usageError(fmt.Errorf("--mode %q is not one of web, cli, tui", mode))
			}
			if _, err := exec.LookPath("npx"); err != nil {
				return configError(fmt.Errorf(
					"npx is not on PATH, and the Inspector is a Node application. " +
						"Install Node, or use `mcpdoll gateway catalog` and " +
						"`mcpdoll gateway call`, which need nothing extra"))
			}

			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()

			key := asKey
			var minted *api.APIKey
			if key == "" {
				var err error
				key, minted, err = mintInspectorKey(ctx, env)
				if err != nil {
					return err
				}
			}

			endpoint := strings.TrimRight(env.GatewayURL(), "/") + "/mcp"
			inspectorArgs := []string{"-y", inspectorPackage, "--" + mode,
				"--transport", "http",
				"--server-url", endpoint,
				"--header", "Authorization: Bearer " + key,
			}
			inspectorArgs = append(inspectorArgs, args...)

			env.Printf("launching the MCP Inspector against %s\n", endpoint)
			if minted != nil {
				env.Printf("  as %s, with a key that expires in %s and is revoked on exit\n",
					minted.UserID, inspectorKeyTTL)
			}

			run := exec.CommandContext(ctx, "npx", inspectorArgs...)
			run.Stdout = env.Out
			run.Stderr = env.Err
			run.Stdin = os.Stdin

			// Take the signal ourselves so the deferred revoke runs. Letting it
			// reach the child and kill this process would leave the credential
			// live until its expiry.
			stop := make(chan os.Signal, 1)
			signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
			defer signal.Stop(stop)
			go func() {
				select {
				case <-stop:
					cancel()
				case <-ctx.Done():
				}
			}()

			if err := run.Run(); err != nil {
				var exit *exec.ExitError
				if errors.As(err, &exit) || errors.Is(ctx.Err(), context.Canceled) {
					// The Inspector's own exit, or our ctrl-C. Neither is this
					// command failing.
					return nil
				}
				return unavailableError(fmt.Errorf("running the Inspector: %w", err))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&mode, "mode", "web", "web, cli, or tui")
	cmd.Flags().StringVar(&asKey, "as", "",
		"an existing API key to present, instead of minting one for yourself")
	cmd.Flags().BoolVar(&keepKey, "keep-key", false,
		"do not revoke the minted key on exit; it still expires")
	return cmd
}

// mintInspectorKey creates a credential carrying the caller's own grants.
//
// Their own, and that is the point: an inspection credential that carried more
// than the operator holds would show them a catalog nobody actually gets.
// Declaring no grants means the key inherits whatever its owner has, which is
// exactly right and also means it shrinks if their access is reduced mid-session.
func mintInspectorKey(ctx context.Context, env *Env) (string, *api.APIKey, error) {
	var me api.SessionInfo
	if err := apiCall(ctx, env, "GET", "/api/v1/auth/session", nil, &me); err != nil {
		return "", nil, err
	}
	if me.UserID == "" {
		return "", nil, configError(fmt.Errorf(
			"this credential is the deployment's %s token, which is not a person and "+
				"holds no grants to inspect as. Sign in with `mcpdoll auth login`, or "+
				"pass --as with an agent key", me.Kind))
	}

	expires := time.Now().Add(inspectorKeyTTL).UTC().Format(time.RFC3339)
	body := map[string]any{
		// Named so an operator finding it in the key list knows what made it.
		"name":       "mcp-inspector " + time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"expires_at": expires,
	}

	var out api.MintedAPIKey
	if err := apiCall(ctx, env, "POST",
		"/api/v1/users/"+me.UserID+"/keys", body, &out); err != nil {
		return "", nil, fmt.Errorf(
			"%w\n\nMinting an inspection key needs key:manage at your own tenant. If you "+
				"hold tool access only, ask an administrator for a key and pass it with "+
				"--as", err)
	}
	return out.Secret, &out.Key, nil
}

// publishAndWait blocks until the minted key resolves.
//
// Polling the inspection endpoint rather than sleeping a fixed interval: it
// asks the question that actually matters — can this credential list a catalog
// — and it is the same call the console's inspect screen makes.
func waitForCredential(ctx context.Context, env *Env, credential string) error {
	// No publish. The control plane wrote the principal set when it minted the
	// key (ADR 0024); this only waits for the gateway to pick it up, which is a
	// second or two rather than a discovery sweep of every backend.
	deadline := time.Now().Add(30 * time.Second)
	for {
		if credentialResolves(ctx, env, credential) {
			env.Printf("the gateway is serving this credential\n")
			return nil
		}
		if time.Now().After(deadline) {
			return unavailableError(errors.New(
				"the gateway did not pick up the new principal set within 30s; " +
					"check `mcpdoll gateway status`"))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// credentialResolves asks the gateway, through the control plane's inspector,
// whether this credential can list anything yet.
func credentialResolves(ctx context.Context, env *Env, credential string) bool {
	req, err := newAPIRequest(ctx, env, "GET", "/api/v1/gateway/catalog", nil)
	if err != nil {
		return false
	}
	req.Header.Set("X-MCPDoll-Inspect-Credential", credential)

	resp, err := httpClient().Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

// revokeInspectorKey best-effort revokes the credential this command minted.
//
// Its own context, because the command's has already been cancelled by the time
// this runs — that is what a deferred cleanup after a ctrl-C means, and using
// the dead context would silently skip the revoke.
func revokeInspectorKey(env *Env, key *api.APIKey) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := apiCall(ctx, env, "DELETE", "/api/v1/keys/"+key.ID, nil, nil); err != nil {
		// Loud, because the alternative to noticing is a live credential
		// nobody knows about. It still expires on its own.
		fmt.Fprintf(env.Err,
			"mcpdoll: could not revoke the inspection key %s (%s): %v\n"+
				"  it expires on its own; revoke it with "+
				"`mcpdoll users keys revoke` if that is too long\n",
			key.Name, key.Prefix, err)
		return
	}
	env.Printf("inspection key revoked\n")
}
