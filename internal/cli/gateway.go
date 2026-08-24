// Copyright 2026 Henry Zektser.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mcpdoll/mcpdoll/internal/dataplane/edge"
	"github.com/spf13/cobra"
)

// The gateway commands are the CLI half of the console's inspector: connect to a
// live data plane as a chosen identity and see exactly what that identity sees.
//
// This matters more than it sounds. "Which tools can this agent actually call?"
// is the question every entitlement bug reduces to, and answering it by reading
// policy is how people get it wrong. Connecting as the principal and looking is
// the only answer that cannot be mistaken.

func newGatewayCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "gateway",
		Aliases: []string{"gw"},
		Short:   "Inspect and exercise a running data plane",
	}
	cmd.AddCommand(
		newGatewayStatusCmd(env),
		newGatewayCatalogCmd(env),
		newGatewayCallCmd(env),
		newGatewayBackendsCmd(env),
	)
	return cmd
}

// ----------------------------------------------------------------- status ----

func newGatewayStatusCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report which snapshot a data plane is serving",
		Annotations: map[string]string{
			annotationOperation: "getGatewayStatus",
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			url := strings.TrimRight(env.GatewayURL(), "/") + "/readyz"
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return err
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return unavailableError(fmt.Errorf("cannot reach the data plane at %s: %w",
					env.GatewayURL(), err))
			}
			defer resp.Body.Close()

			var payload struct {
				Status  string `json:"status"`
				Version int64  `json:"snapshot_version"`
				Tenants int    `json:"tenants"`
				Tools   int    `json:"tools"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
				return unavailableError(fmt.Errorf("%s returned an unreadable body: %w", url, err))
			}
			if resp.StatusCode != http.StatusOK {
				// A data plane with no snapshot is a real, reportable state, not a
				// CLI failure — so report it and exit non-zero for scripting.
				_ = env.Emit(statusReport{
					GatewayURL: env.GatewayURL(),
					Status:     payload.Status,
					Ready:      false,
				})
				return unavailableError(fmt.Errorf("data plane is not ready: %s", payload.Status))
			}
			return env.Emit(statusReport{
				GatewayURL:      env.GatewayURL(),
				Status:          payload.Status,
				Ready:           true,
				SnapshotVersion: payload.Version,
				Tenants:         payload.Tenants,
				Tools:           payload.Tools,
			})
		},
	}
	return cmd
}

type statusReport struct {
	GatewayURL      string `json:"gateway_url" yaml:"gateway_url"`
	Status          string `json:"status" yaml:"status"`
	Ready           bool   `json:"ready" yaml:"ready"`
	SnapshotVersion int64  `json:"snapshot_version" yaml:"snapshot_version"`
	Tenants         int    `json:"tenants" yaml:"tenants"`
	Tools           int    `json:"tools" yaml:"tools"`
}

func (r statusReport) Table() Table {
	return Table{
		Columns: []string{"GATEWAY", "READY", "SNAPSHOT", "TENANTS", "TOOLS"},
		Rows: [][]string{{
			r.GatewayURL,
			strconv.FormatBool(r.Ready),
			strconv.FormatInt(r.SnapshotVersion, 10),
			strconv.Itoa(r.Tenants),
			strconv.Itoa(r.Tools),
		}},
	}
}

// ---------------------------------------------------------------- catalog ----

func newGatewayCatalogCmd(env *Env) *cobra.Command {
	var (
		credential string
		full       bool
	)

	cmd := &cobra.Command{
		Use:   "catalog",
		Short: "List the tools an identity actually sees",
		Long: "Connects to the data plane as a real MCP client, as the given identity, and\n" +
			"prints the catalog it receives — including ttlMs and cacheScope.\n\n" +
			"This is the answer to \"which tools can this agent call?\" that cannot be wrong,\n" +
			"because it is the same request the agent makes.",
		Example: "  mcpdoll gateway catalog --as $MCPDOLL_AGENT_KEY ",
		Annotations: map[string]string{
			annotationOperation: "getCatalog",
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, resolved, err := env.connectMCP(cmd.Context(), credential)
			if err != nil {
				return err
			}
			defer session.Close()

			res, err := session.ListTools(cmd.Context(), nil)
			if err != nil {
				return unavailableError(fmt.Errorf("listing tools: %w", err))
			}

			// Who the gateway decided this credential is. Nothing on this side
			// could have known — the tenant comes from the key.
			tenant, subject := resolved.get()
			report := catalogReport{
				Tenant:     tenant,
				Subject:    subject,
				TTLMs:      res.TTLMs,
				CacheScope: res.CacheScope,
				full:       full,
			}
			if init := session.InitializeResult(); init != nil {
				report.ProtocolVersion = init.ProtocolVersion
				report.ServerName = init.ServerInfo.Name
			}
			for _, tool := range res.Tools {
				entry := catalogEntry{Name: tool.Name, Title: tool.Title}
				if full {
					entry.Description = tool.Description
				} else {
					entry.Description = firstLine(tool.Description)
				}
				namespace, _, _ := strings.Cut(tool.Name, ".")
				entry.Namespace = namespace
				report.Tools = append(report.Tools, entry)
			}
			return env.Emit(report)
		},
	}

	cmd.Flags().StringVar(&credential, "as", "",
		"API key to inspect as; inspection presents what the principal presents")
	cmd.Flags().BoolVar(&full, "full", false, "print full descriptions rather than the first line")
	_ = cmd.MarkFlagRequired("as")
	return cmd
}

type catalogReport struct {
	Tenant          string         `json:"tenant" yaml:"tenant"`
	Subject         string         `json:"subject,omitempty" yaml:"subject,omitempty"`
	ProtocolVersion string         `json:"protocol_version" yaml:"protocol_version"`
	ServerName      string         `json:"server_name" yaml:"server_name"`
	TTLMs           int            `json:"ttl_ms" yaml:"ttl_ms"`
	CacheScope      string         `json:"cache_scope" yaml:"cache_scope"`
	Tools           []catalogEntry `json:"tools" yaml:"tools"`

	full bool
}

type catalogEntry struct {
	Name        string `json:"name" yaml:"name"`
	Namespace   string `json:"namespace" yaml:"namespace"`
	Title       string `json:"title,omitempty" yaml:"title,omitempty"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
}

func (r catalogReport) Table() Table {
	rows := make([][]string, 0, len(r.Tools))
	for _, t := range r.Tools {
		rows = append(rows, []string{t.Name, t.Namespace, t.Description})
	}
	notes := []string{
		fmt.Sprintf("%d tool(s) for tenant %q via %s", len(r.Tools), r.Tenant, r.ProtocolVersion),
		fmt.Sprintf("ttlMs=%d cacheScope=%s", r.TTLMs, r.CacheScope),
	}
	if r.CacheScope == "private" {
		notes = append(notes,
			"this catalog is identity-filtered, so intermediaries must not share it")
	}
	return Table{
		Columns: []string{"TOOL", "NAMESPACE", "DESCRIPTION"},
		Rows:    rows,
		Notes:   notes,
	}
}

// ------------------------------------------------------------------- call ----

func newGatewayCallCmd(env *Env) *cobra.Command {
	var (
		credential   string
		argsJSON     string
		requestState string
		respond      []string
	)

	cmd := &cobra.Command{
		Use:   "call <tool>",
		Short: "Call a tool through the gateway",
		Long: "Invokes a tool as a real MCP client would, so what you see is what an agent\n" +
			"would see — including a policy denial or a grace-window unavailability, both of\n" +
			"which arrive as tool errors rather than transport failures.",
		Example: `  mcpdoll gateway call crm.lookup_customer --as $MCPDOLL_AGENT_KEY \
      --args '{"customer_id":"cus_1"}'

  # A tool that needs confirmation returns resultType input_required plus an
  # opaque requestState. Answer it and retry with both:
  mcpdoll gateway call dep.promote_release --as $MCPDOLL_AGENT_KEY \
      --args '{"build":"v1"}' --respond confirm=accept --request-state "$STATE"`,
		Args: cobra.ExactArgs(1),
		Annotations: map[string]string{
			annotationOperation: "callTool",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			var arguments map[string]any
			if argsJSON != "" {
				if err := json.Unmarshal([]byte(argsJSON), &arguments); err != nil {
					return usageError(fmt.Errorf("--args is not a JSON object: %w", err))
				}
			}

			responses, err := parseResponses(respond)
			if err != nil {
				return err
			}
			if len(responses) > 0 && requestState == "" {
				// Retrying without the state would drop the binding between the
				// approval and what it approved, so the gateway would refuse it.
				// Say so here rather than letting the user read a verification
				// error and wonder what they did wrong.
				return usageError(fmt.Errorf(
					"--respond needs --request-state as well: the state is what binds your " +
						"answer to the call it was asked about"))
			}

			session, _, err := env.connectMCP(cmd.Context(), credential)
			if err != nil {
				return err
			}
			defer session.Close()

			start := time.Now()
			res, err := session.CallTool(cmd.Context(), &sdk.CallToolParams{
				Name:           args[0],
				Arguments:      arguments,
				InputResponses: responses,
				RequestState:   requestState,
			})
			elapsed := time.Since(start)
			if err != nil {
				return unavailableError(fmt.Errorf("calling %s: %w", args[0], err))
			}

			report := callReport{
				Tool:       args[0],
				IsError:    res.IsError,
				NeedsInput: res.NeedsInput(),
				DurationMS: elapsed.Milliseconds(),
				Text:       resultText(res),
			}
			if detail, ok := res.Meta["mcpdoll"]; ok {
				raw, _ := json.Marshal(detail)
				report.GatewayDetail = json.RawMessage(raw)
			}
			if res.NeedsInput() {
				for id := range res.InputRequests {
					report.InputRequests = append(report.InputRequests, id)
				}
				sort.Strings(report.InputRequests)
				report.RequestState = res.RequestState
			}

			if err := env.Emit(report); err != nil {
				return err
			}
			// A tool error is a real outcome the caller may want to branch on, so
			// it exits non-zero — but it is not a CLI failure, so nothing is
			// printed to stderr beyond the code.
			if res.IsError {
				return validationError(fmt.Errorf("%s returned an error", args[0]))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&credential, "as", "",
		"API key to inspect as; inspection presents what the principal presents")
	cmd.Flags().StringVar(&argsJSON, "args", "", "tool arguments as a JSON object")
	cmd.Flags().StringVar(&requestState, "request-state", "",
		"opaque requestState from a previous input_required result")
	cmd.Flags().StringArrayVar(&respond, "respond", nil,
		"answer an input request as id=accept|decline|cancel, or id=text:<value> (repeatable)")
	_ = cmd.MarkFlagRequired("as")
	return cmd
}

// parseResponses turns --respond flags into an MCP input-response map.
//
// Only elicitation responses are supported, which covers confirmation — the case
// a human actually completes from a terminal. Sampling and roots requests exist
// in the protocol but answering them by hand is not a workflow; a client
// framework does that.
func parseResponses(flags []string) (sdk.InputResponseMap, error) {
	if len(flags) == 0 {
		return nil, nil
	}
	out := sdk.InputResponseMap{}
	for _, raw := range flags {
		id, value, ok := strings.Cut(raw, "=")
		if !ok || id == "" || value == "" {
			return nil, usageError(fmt.Errorf(
				"--respond %q is not in id=value form", raw))
		}
		switch {
		case value == "accept" || value == "decline" || value == "cancel":
			out[id] = &sdk.ElicitResult{Action: value}
		case strings.HasPrefix(value, "text:"):
			out[id] = &sdk.ElicitResult{
				Action:  "accept",
				Content: map[string]any{"value": strings.TrimPrefix(value, "text:")},
			}
		default:
			return nil, usageError(fmt.Errorf(
				"--respond %q: value must be accept, decline, cancel, or text:<value>", raw))
		}
	}
	return out, nil
}

type callReport struct {
	Tool          string          `json:"tool" yaml:"tool"`
	IsError       bool            `json:"is_error" yaml:"is_error"`
	NeedsInput    bool            `json:"needs_input" yaml:"needs_input"`
	DurationMS    int64           `json:"duration_ms" yaml:"duration_ms"`
	Text          string          `json:"text" yaml:"text"`
	GatewayDetail json.RawMessage `json:"gateway_detail,omitempty" yaml:"gateway_detail,omitempty"`
	InputRequests []string        `json:"input_requests,omitempty" yaml:"input_requests,omitempty"`
	RequestState  string          `json:"request_state,omitempty" yaml:"request_state,omitempty"`
}

func (r callReport) Table() Table {
	notes := []string{r.Text}
	if r.NeedsInput {
		notes = append(notes,
			fmt.Sprintf("this call needs input: %s", strings.Join(r.InputRequests, ", ")),
			"retry with the responses and the requestState from the JSON output")
	}
	if len(r.GatewayDetail) > 0 {
		notes = append(notes, "gateway detail: "+string(r.GatewayDetail))
	}
	return Table{
		Columns: []string{"TOOL", "OUTCOME", "MS"},
		Rows:    [][]string{{r.Tool, r.outcome(), strconv.FormatInt(r.DurationMS, 10)}},
		Notes:   notes,
	}
}

func (r callReport) outcome() string {
	switch {
	case r.NeedsInput:
		return "input_required"
	case r.IsError:
		return "error"
	default:
		return "ok"
	}
}

// --------------------------------------------------------------- plumbing ----

// connectMCP opens a real MCP client session against the data plane.
// resolvedIdentity is who the gateway said a credential turned out to be.
//
// Read from a response header rather than derived: the tenant comes from the
// key (ADR 0019), so nothing on this side knows it until the gateway answers.
type resolvedIdentity struct {
	mu      sync.Mutex
	tenant  string
	subject string
}

func (r *resolvedIdentity) record(header http.Header) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v := header.Get(edge.HeaderResolvedTenant); v != "" {
		r.tenant = v
	}
	if v := header.Get(edge.HeaderResolvedSubject); v != "" {
		r.subject = v
	}
}

func (r *resolvedIdentity) get() (tenant, subject string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tenant, r.subject
}

func (e *Env) connectMCP(ctx context.Context, credential string) (*sdk.ClientSession, *resolvedIdentity, error) {
	if credential == "" {
		return nil, nil, usageError(fmt.Errorf(
			"--as is required: inspection presents the principal's own credential " +
				"rather than re-deriving what they should see"))
	}

	header := http.Header{}
	header.Set("Authorization", "Bearer "+credential)

	client := sdk.NewClient(&sdk.Implementation{
		Name: "mcpdoll-cli", Title: "MCPDoll CLI", Version: Version,
	}, &sdk.ClientOptions{
		// The CLI shows the raw MRTR result rather than fulfilling it: an
		// operator inspecting an interactive tool wants to see that it asked,
		// not to have the CLI answer on their behalf.
		MultiRoundTrip: &sdk.MultiRoundTripOptions{Disabled: true},
	})

	// One endpoint. The tenant and the toolset both come from the credential
	// (ADR 0019).
	endpoint := strings.TrimRight(e.GatewayURL(), "/") + "/mcp"
	resolved := &resolvedIdentity{}
	session, err := client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           headerClient(header, resolved),
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		return nil, nil, unavailableError(fmt.Errorf("connecting to %s: %w", endpoint, err))
	}
	return session, resolved, nil
}

func headerClient(header http.Header, resolved *resolvedIdentity) *http.Client {
	return &http.Client{
		Timeout: 60 * time.Second,
		Transport: &staticHeaders{
			base: http.DefaultTransport, header: header, resolved: resolved,
		},
	}
}

type staticHeaders struct {
	base     http.RoundTripper
	header   http.Header
	resolved *resolvedIdentity
}

func (t *staticHeaders) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	for k, values := range t.header {
		for _, v := range values {
			out.Header.Set(k, v)
		}
	}
	resp, err := t.base.RoundTrip(out)
	if err == nil && t.resolved != nil {
		t.resolved.record(resp.Header)
	}
	return resp, err
}

func resultText(res *sdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if text, ok := c.(*sdk.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

func firstLine(s string) string {
	if line, _, ok := strings.Cut(s, "\n"); ok {
		return line
	}
	return s
}
