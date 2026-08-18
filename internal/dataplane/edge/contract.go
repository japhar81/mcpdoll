// Copyright 2026 The MCPDoll Authors.

package edge

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mcpdoll/mcpdoll/internal/dataplane/backends"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/snapshot"
)

// The types in this file are the contract between the edge and the pipeline.
//
// They live in the edge package because the edge is the only thing that speaks
// MCP: keeping the MCP types on this side of the boundary means the pipeline and
// plugin packages never import the SDK, so a protocol change cannot ripple into
// the hook engine.

// CatalogRequest is an ON_CATALOG invocation.
type CatalogRequest struct {
	Audience  *snapshot.AudienceView
	Principal backends.Principal
	// Tools as the snapshot would serve them, in the stable order.
	Tools []*mcp.Tool
}

// CatalogDecision is the pipeline's answer for a catalog.
type CatalogDecision struct {
	// Tools to serve. Nil means "unchanged"; an empty non-nil slice means
	// "serve nothing", which is a legitimate answer for a principal entitled to
	// no tools. The distinction matters: conflating them would make an
	// entitlement filter that removed everything look like a no-op.
	Tools []*mcp.Tool

	// IdentityFiltered reports that the result depends on who asked, forcing
	// cacheScope: private.
	IdentityFiltered bool

	// Annotations for the audit trail and the console's trace waterfall.
	Annotations map[string]any
}

// ToolCallRequest is an ON_TOOL_CALL invocation.
type ToolCallRequest struct {
	Audience  *snapshot.AudienceView
	Tool      *snapshot.Tool
	Principal backends.Principal
	Arguments any
	Meta      map[string]any

	// RequestState and InputResponses are set when this is an MRTR retry, so a
	// plugin that deferred can pick up where it left off.
	RequestState   string
	InputResponses mcp.InputResponseMap
}

// ToolCallDecision is the pipeline's answer before dispatch.
type ToolCallDecision struct {
	// Decision is the verdict: "allow", "deny", "mutate", "annotate", "defer".
	Decision string
	Reason   string

	// Arguments replaces the call arguments when the verdict is a mutation.
	Arguments any

	// InputRequests and RequestState are set for a deferral. RequestState is the
	// plugin's *own* opaque state; the edge wraps it in a signed envelope before
	// it reaches the client, so a plugin never has to think about forgery.
	InputRequests mcp.InputRequestMap
	RequestState  string

	Annotations map[string]any
}

// Denied reports whether dispatch must not happen.
func (d *ToolCallDecision) Denied() bool {
	return d != nil && d.Decision == "deny"
}

// Deferred reports whether the call needs client input first.
func (d *ToolCallDecision) Deferred() bool {
	return d != nil && d.Decision == "defer" && len(d.InputRequests) > 0
}

// ToolResultRequest is an ON_TOOL_RESULT invocation.
type ToolResultRequest struct {
	Audience  *snapshot.AudienceView
	Tool      *snapshot.Tool
	Principal backends.Principal
	Result    *mcp.CallToolResult
}

// ToolResultDecision is the pipeline's answer after dispatch.
type ToolResultDecision struct {
	Decision string
	Reason   string

	// Result replaces the backend's result when the verdict is a mutation —
	// how the redaction plugin removes content before it reaches the model.
	Result *mcp.CallToolResult

	Annotations map[string]any
}

// Denied reports whether the result must be withheld.
func (d *ToolResultDecision) Denied() bool {
	return d != nil && d.Decision == "deny"
}
