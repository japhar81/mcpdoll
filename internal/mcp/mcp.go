// Copyright 2026 The MCPDoll Authors.

// Package mcp is MCPDoll's thin adapter over the official MCP Go SDK.
//
// Two jobs, and deliberately no others:
//
//   - **Discovery.** Connect to a backend, negotiate a protocol version, and
//     read its catalog. Used by admission (to learn what a publisher is
//     offering) and by the prober (to detect drift).
//
//   - **Conversion.** Translate the SDK's wire types into MCPDoll's canonical
//     tool definition, and back.
//
// Keeping the SDK behind this package means a protocol change touches the
// adapter and the edge, not the registry, the pipeline, or the console's
// contract. The conversion in particular is a deliberate decoupling: the SDK's
// `mcp.Tool` gains fields as the spec moves, and a new optional field appearing
// there must not silently change the digest of every stored definition.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mcpdoll/mcpdoll/internal/platform/canonical"
)

// ClientName and ClientVersion identify MCPDoll to the backends it dials.
const (
	ClientName    = "mcpdoll"
	ClientTitle   = "MCPDoll Gateway"
	ClientVersion = "0.1.0"
)

// DiscoveryResult is what one discovery pass learned about a backend.
type DiscoveryResult struct {
	// NegotiatedVersion is the protocol version actually agreed. A backend that
	// negotiates down is not an error, but it is worth recording: some
	// capabilities are unavailable below 2026-07-28.
	NegotiatedVersion string

	// ServerInfo is the backend's self-description.
	ServerInfo *sdk.Implementation

	// Instructions the backend supplies.
	Instructions string

	// Capabilities the backend advertises.
	Capabilities *sdk.ServerCapabilities

	// Tools as the backend currently serves them. This is *observed* state.
	// Nothing here is served to a client until it has been admitted.
	Tools []*sdk.Tool

	// ObservedAt is when the pass ran, for drift bookkeeping.
	ObservedAt time.Time
}

// DiscoverOptions configures a discovery pass.
type DiscoverOptions struct {
	// Endpoint is the backend's streamable HTTP URL.
	Endpoint string
	// Timeout bounds the whole pass.
	Timeout time.Duration
	// HTTPClient allows a caller to supply credentials or instrumentation.
	HTTPClient *http.Client
	// Header is added to every request, for a backend that needs a static
	// credential during discovery.
	Header http.Header
	// MaxTools bounds pagination so a misbehaving backend cannot make a probe
	// loop forever.
	MaxTools int
}

// DefaultMaxTools bounds a discovery pass. Admission caps a server far below
// this; the limit exists to stop a paginating backend hanging a prober.
const DefaultMaxTools = 5000

// Discover connects to a backend and reads its catalog.
//
// The connection is closed before returning: discovery is a one-shot operation
// run by the control plane, not a pooled serving connection.
func Discover(ctx context.Context, opts DiscoverOptions) (*DiscoveryResult, error) {
	if opts.Endpoint == "" {
		return nil, fmt.Errorf("mcp: discovery needs an endpoint")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 15 * time.Second
	}
	if opts.MaxTools <= 0 {
		opts.MaxTools = DefaultMaxTools
	}

	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: opts.Timeout}
	}
	if len(opts.Header) > 0 {
		httpClient = withStaticHeaders(httpClient, opts.Header)
	}

	client := sdk.NewClient(&sdk.Implementation{
		Name:    ClientName,
		Title:   ClientTitle,
		Version: ClientVersion,
	}, &sdk.ClientOptions{
		// Discovery must not fulfil an input request on the backend's behalf: it
		// is reading a catalog, and a backend that asks for input during
		// discovery is misbehaving rather than something to satisfy.
		MultiRoundTrip: &sdk.MultiRoundTripOptions{Disabled: true},
	})

	session, err := client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint:             opts.Endpoint,
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: connecting to %s: %w", opts.Endpoint, err)
	}
	defer func() { _ = session.Close() }()

	out := &DiscoveryResult{ObservedAt: time.Now().UTC()}
	if init := session.InitializeResult(); init != nil {
		out.NegotiatedVersion = init.ProtocolVersion
		out.ServerInfo = init.ServerInfo
		out.Instructions = init.Instructions
		out.Capabilities = init.Capabilities
	}

	var cursor string
	for {
		res, err := session.ListTools(ctx, &sdk.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("mcp: listing tools on %s: %w", opts.Endpoint, err)
		}
		out.Tools = append(out.Tools, res.Tools...)
		if res.NextCursor == "" {
			break
		}
		if len(out.Tools) >= opts.MaxTools {
			return nil, fmt.Errorf("mcp: %s returned more than %d tools; refusing to page further",
				opts.Endpoint, opts.MaxTools)
		}
		cursor = res.NextCursor
	}

	return out, nil
}

// staticHeaderTransport adds fixed headers to outbound requests.
type staticHeaderTransport struct {
	base   http.RoundTripper
	header http.Header
}

func (t *staticHeaderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	for k, values := range t.header {
		for _, v := range values {
			out.Header.Add(k, v)
		}
	}
	return t.base.RoundTrip(out)
}

func withStaticHeaders(client *http.Client, header http.Header) *http.Client {
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	clone := *client
	clone.Transport = &staticHeaderTransport{base: base, header: header.Clone()}
	return &clone
}

// ---------------------------------------------------------- conversion -------

// ToCanonical converts an SDK tool into MCPDoll's canonical definition.
//
// Only the fields MCPDoll's identity covers are carried across. A field the SDK
// adds later is *ignored* rather than folded in, so an SDK upgrade cannot
// silently change the digest of every already-admitted definition — that has to
// be a deliberate, versioned change to the canonical form.
func ToCanonical(t *sdk.Tool) (*canonical.ToolDefinition, error) {
	if t == nil {
		return nil, fmt.Errorf("mcp: nil tool")
	}
	if t.Name == "" {
		return nil, fmt.Errorf("mcp: tool has no name")
	}
	out := &canonical.ToolDefinition{
		Name:        t.Name,
		Title:       t.Title,
		Description: t.Description,
	}

	inputSchema, err := schemaToRaw(t.InputSchema)
	if err != nil {
		return nil, fmt.Errorf("mcp: tool %q input schema: %w", t.Name, err)
	}
	if inputSchema != nil {
		out.InputSchema = inputSchema
	}

	outputSchema, err := schemaToRaw(t.OutputSchema)
	if err != nil {
		return nil, fmt.Errorf("mcp: tool %q output schema: %w", t.Name, err)
	}
	if outputSchema != nil {
		out.OutputSchema = outputSchema
	}

	if t.Annotations != nil {
		raw, err := json.Marshal(t.Annotations)
		if err != nil {
			return nil, fmt.Errorf("mcp: tool %q annotations: %w", t.Name, err)
		}
		var annotations map[string]any
		if err := json.Unmarshal(raw, &annotations); err != nil {
			return nil, fmt.Errorf("mcp: tool %q annotations: %w", t.Name, err)
		}
		if len(annotations) > 0 {
			out.Annotations = annotations
		}
	}

	return out, nil
}

// schemaToRaw normalizes the SDK's several schema representations into raw JSON.
//
// `InputSchema` is typed `any` and in practice arrives as a *jsonschema.Schema,
// a map, or raw bytes depending on how the tool was registered. Handling all
// three here means no caller has to care which.
func schemaToRaw(schema any) (json.RawMessage, error) {
	if schema == nil {
		return nil, nil
	}
	switch s := schema.(type) {
	case json.RawMessage:
		if len(s) == 0 {
			return nil, nil
		}
		return s, nil
	case []byte:
		if len(s) == 0 {
			return nil, nil
		}
		return json.RawMessage(s), nil
	case string:
		if s == "" {
			return nil, nil
		}
		return json.RawMessage(s), nil
	default:
		raw, err := json.Marshal(schema)
		if err != nil {
			return nil, err
		}
		// A schema that marshals to "null" carries no information; treat it as
		// absent rather than as an explicit null, which would change the digest.
		if string(raw) == "null" {
			return nil, nil
		}
		return raw, nil
	}
}

// DigestTools canonicalizes and digests a discovered catalog, keyed by tool
// name.
//
// This is the input to drift detection: comparing these digests against the
// admitted ones is the whole mechanism.
func DigestTools(tools []*sdk.Tool) (map[string]ToolDigests, error) {
	out := make(map[string]ToolDigests, len(tools))
	for _, t := range tools {
		def, err := ToCanonical(t)
		if err != nil {
			return nil, err
		}
		full, err := def.Digest()
		if err != nil {
			return nil, err
		}
		semantic, err := def.SemanticDigest()
		if err != nil {
			return nil, err
		}
		if _, dup := out[t.Name]; dup {
			return nil, fmt.Errorf("mcp: backend published tool %q twice", t.Name)
		}
		out[t.Name] = ToolDigests{
			Full:       full,
			Semantic:   semantic,
			Definition: def,
		}
	}
	return out, nil
}

// ToolDigests is a discovered tool's pair of content addresses.
type ToolDigests struct {
	Full     canonical.Digest
	Semantic canonical.Digest
	// Definition is the canonical form, kept so a drift diff can show what
	// actually changed rather than only that something did.
	Definition *canonical.ToolDefinition
}
