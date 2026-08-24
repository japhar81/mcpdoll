// Copyright 2026 Henry Zektser.

// Package wiring composes the data plane's parts.
//
// It exists because the two things it joins deliberately do not know about each
// other: the edge speaks MCP and knows nothing about plugins, and the pipeline
// engine runs plugins and knows nothing about MCP. Neither should import the
// other — a protocol change would ripple into the hook engine, and a hook change
// would ripple into the protocol adapter.
//
// So the translation lives here, in one file, where it can be read as a single
// mapping rather than discovered across two packages.
package wiring

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mcpdoll/mcpdoll/internal/dataplane/backends"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/edge"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/pipeline"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/snapshot"
	"github.com/mcpdoll/mcpdoll/internal/platform/ids"
	"github.com/mcpdoll/mcpdoll/internal/platform/logging"
	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// EdgePipeline adapts the hook engine to the edge's Pipeline interface.
type EdgePipeline struct {
	engine *pipeline.Engine
	log    *slog.Logger
}

// NewEdgePipeline builds the adapter.
func NewEdgePipeline(engine *pipeline.Engine, log *slog.Logger) *EdgePipeline {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &EdgePipeline{engine: engine, log: log}
}

var _ edge.Pipeline = (*EdgePipeline)(nil)

// OnCatalog runs the catalog hook.
func (p *EdgePipeline) OnCatalog(ctx context.Context, req *edge.CatalogRequest) (*edge.CatalogDecision, error) {
	payload := catalogPayload{
		Tenant:    req.PrincipalView.Tenant.Slug,
		Principal: principalOf(req.Principal),
		Catalog:   make([]toolPayload, 0, len(req.Tools)),
	}
	for _, tool := range req.Tools {
		payload.Catalog = append(payload.Catalog, toolPayloadFrom(req.PrincipalView, tool.Name, tool))
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("wiring: encoding the catalog: %w", err)
	}

	trace := newTrace(req.PrincipalView.Tenant.Slug, req.Principal.Subject, "")
	result, err := p.engine.Run(ctx, &pipeline.HookRequest{
		RequestID:     trace.RequestID,
		PrincipalView: req.PrincipalView,
		Hook:          snapshotpb.Hook_HOOK_ON_CATALOG,
		// A catalog is a read by construction, so a fail-closed plugin here is
		// governed by the read policy.
		EffectClass: snapshotpb.EffectClass_EFFECT_CLASS_READ,
		Payload:     raw,
		Trace:       trace,
	})
	if err != nil {
		return nil, err
	}
	p.finish(ctx, trace, result)

	decision := &edge.CatalogDecision{Annotations: result.Annotations}

	switch result.Decision {
	case pipeline.DecisionDeny:
		// A denial at ON_CATALOG means "this principal gets nothing". An empty
		// non-nil slice says that explicitly; nil would mean "unchanged".
		decision.Tools = []*sdk.Tool{}
		decision.IdentityFiltered = true
		return decision, nil

	case pipeline.DecisionMutate:
		var patched catalogPayload
		if err := json.Unmarshal(result.Payload, &patched); err != nil {
			return nil, fmt.Errorf("wiring: a plugin left the catalog unreadable: %w", err)
		}
		// Rebuild from the *original* tools, matched by name. A plugin may only
		// remove and reorder; it cannot invent a tool or edit a description,
		// because what the gateway serves is the admitted definition (ADR 0006)
		// and a plugin is not an admission decision.
		byName := make(map[string]*sdk.Tool, len(req.Tools))
		for _, tool := range req.Tools {
			byName[tool.Name] = tool
		}
		kept := make([]*sdk.Tool, 0, len(patched.Catalog))
		for _, entry := range patched.Catalog {
			if tool, ok := byName[entry.Name]; ok {
				kept = append(kept, tool)
			}
			// An entry naming a tool that was not in the input is silently
			// dropped rather than honoured: a plugin cannot add to a catalog.
		}
		decision.Tools = kept
		decision.IdentityFiltered = len(kept) != len(req.Tools)
		return decision, nil

	default:
		return decision, nil
	}
}

// OnToolCall runs the pre-dispatch hook.
func (p *EdgePipeline) OnToolCall(ctx context.Context, req *edge.ToolCallRequest) (*edge.ToolCallDecision, error) {
	arguments, err := asObject(req.Arguments)
	if err != nil {
		return nil, err
	}

	payload := callPayload{
		Tenant:         req.PrincipalView.Tenant.Slug,
		Principal:      principalOf(req.Principal),
		Tool:           toolPayloadFromSnapshot(req.Tool),
		Arguments:      arguments,
		PluginState:    req.RequestState,
		InputResponses: inputResponsesOf(req.InputResponses),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("wiring: encoding the call: %w", err)
	}

	trace := newTrace(req.PrincipalView.Tenant.Slug, req.Principal.Subject, req.Tool.Def.QualifiedName)
	result, err := p.engine.Run(ctx, &pipeline.HookRequest{
		RequestID:     trace.RequestID,
		PrincipalView: req.PrincipalView,
		Hook:          snapshotpb.Hook_HOOK_ON_TOOL_CALL,
		EffectClass:   req.Tool.Def.EffectClass,
		Payload:       raw,
		Trace:         trace,
	})
	if err != nil {
		return nil, err
	}
	p.finish(ctx, trace, result)

	decision := &edge.ToolCallDecision{
		Decision:    string(result.Decision),
		Reason:      result.Reason,
		Annotations: result.Annotations,
	}

	switch result.Decision {
	case pipeline.DecisionDefer:
		requests, err := decodeInputRequests(result.InputRequests)
		if err != nil {
			return nil, err
		}
		decision.InputRequests = requests
		decision.RequestState = result.PluginState

	case pipeline.DecisionMutate:
		var patched callPayload
		if err := json.Unmarshal(result.Payload, &patched); err != nil {
			return nil, fmt.Errorf("wiring: a plugin left the call unreadable: %w", err)
		}
		decision.Arguments = patched.Arguments
	}
	return decision, nil
}

// OnToolResult runs the post-dispatch hook.
func (p *EdgePipeline) OnToolResult(ctx context.Context, req *edge.ToolResultRequest) (*edge.ToolResultDecision, error) {
	payload := resultPayload{
		Tenant:    req.PrincipalView.Tenant.Slug,
		Principal: principalOf(req.Principal),
		Tool:      toolPayloadFromSnapshot(req.Tool),
		Result:    resultOf(req.Result),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("wiring: encoding the result: %w", err)
	}

	trace := newTrace(req.PrincipalView.Tenant.Slug, req.Principal.Subject, req.Tool.Def.QualifiedName)
	hookResult, err := p.engine.Run(ctx, &pipeline.HookRequest{
		RequestID:     trace.RequestID,
		PrincipalView: req.PrincipalView,
		Hook:          snapshotpb.Hook_HOOK_ON_TOOL_RESULT,
		EffectClass:   req.Tool.Def.EffectClass,
		Payload:       raw,
		Trace:         trace,
	})
	if err != nil {
		return nil, err
	}
	p.finish(ctx, trace, hookResult)

	decision := &edge.ToolResultDecision{
		Decision:    string(hookResult.Decision),
		Reason:      hookResult.Reason,
		Annotations: hookResult.Annotations,
	}

	if hookResult.Decision == pipeline.DecisionMutate {
		var patched resultPayload
		if err := json.Unmarshal(hookResult.Payload, &patched); err != nil {
			return nil, fmt.Errorf("wiring: a plugin left the result unreadable: %w", err)
		}
		decision.Result = applyResult(req.Result, patched.Result)
	}
	return decision, nil
}

func (p *EdgePipeline) finish(ctx context.Context, trace *pipeline.Trace, result *pipeline.HookResult) {
	trace.Decision = result.Decision
	trace.Reason = result.Reason
	trace.DecidedBy = result.DecidedBy
	p.engine.EmitTrace(trace)

	if result.Decision != pipeline.DecisionAllow {
		p.log.DebugContext(ctx, "pipeline decided",
			logging.FieldVerdict, string(result.Decision),
			logging.FieldPlugin, result.DecidedBy,
			logging.FieldToolName, trace.Tool,
			"reason", result.Reason)
	}
	for _, diverged := range trace.Divergences() {
		p.log.InfoContext(ctx, "shadow plugin would have acted",
			logging.FieldPlugin, diverged.PluginName,
			logging.FieldHook, diverged.Hook,
			logging.FieldVerdict, string(diverged.Verdict.Decision),
			logging.FieldToolName, trace.Tool,
			"reason", diverged.Verdict.Reason)
	}
}

func newTrace(tenant, principal, tool string) *pipeline.Trace {
	return &pipeline.Trace{
		RequestID: ids.New(ids.KindRequest),
		Tenant:    tenant,
		Principal: principal,
		Tool:      tool,
	}
}

// ---------------------------------------------------------------- payloads --

// The payload shapes are the plugin-facing contract, mirrored in plugins/sdk.
// They are hand-written on both sides rather than generated, because the SDK
// must compile for wasip1 with no dependency on this repository's internals.

type catalogPayload struct {
	Tenant    string           `json:"tenant,omitempty"`
	Principal principalPayload `json:"principal"`
	Catalog   []toolPayload    `json:"catalog"`
}

type callPayload struct {
	Tenant         string           `json:"tenant,omitempty"`
	Principal      principalPayload `json:"principal"`
	Tool           toolPayload      `json:"tool"`
	Arguments      map[string]any   `json:"arguments,omitempty"`
	PluginState    string           `json:"pluginState,omitempty"`
	InputResponses map[string]any   `json:"inputResponses,omitempty"`
}

type resultPayload struct {
	Tenant    string           `json:"tenant,omitempty"`
	Principal principalPayload `json:"principal"`
	Tool      toolPayload      `json:"tool"`
	Result    resultBody       `json:"result"`
}

type principalPayload struct {
	Subject string            `json:"subject"`
	Groups  []string          `json:"groups,omitempty"`
	Claims  map[string]string `json:"claims,omitempty"`
}

type toolPayload struct {
	Name        string `json:"name"`
	Namespace   string `json:"namespace,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	EffectClass string `json:"effect_class,omitempty"`
	Backend     string `json:"backend,omitempty"`
	Digest      string `json:"digest,omitempty"`
}

type resultBody struct {
	Content []contentPayload `json:"content"`
	IsError bool             `json:"isError,omitempty"`
}

type contentPayload struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

func principalOf(p backends.Principal) principalPayload {
	return principalPayload{
		Subject: p.Subject,
		Groups:  p.Groups,
		Claims:  p.Claims,
	}
}

func toolPayloadFromSnapshot(tool *snapshot.Tool) toolPayload {
	return toolPayload{
		Name:        tool.Def.QualifiedName,
		Namespace:   tool.Namespace.Prefix,
		Title:       tool.Def.Title,
		Description: tool.Def.Description,
		EffectClass: effectClassName(tool.Def.EffectClass),
		Backend:     tool.Server.Name,
		Digest:      tool.Def.Digest,
	}
}

func toolPayloadFrom(av *snapshot.PrincipalView, name string, mcpTool *sdk.Tool) toolPayload {
	out := toolPayload{
		Name:        name,
		Title:       mcpTool.Title,
		Description: mcpTool.Description,
	}
	if prefix, _, ok := strings.Cut(name, "."); ok {
		out.Namespace = prefix
	}
	if tool := av.Tool(name); tool != nil {
		out.EffectClass = effectClassName(tool.Def.EffectClass)
		out.Backend = tool.Server.Name
		out.Digest = tool.Def.Digest
	}
	return out
}

func resultOf(res *sdk.CallToolResult) resultBody {
	out := resultBody{IsError: res.IsError, Content: make([]contentPayload, 0, len(res.Content))}
	for _, c := range res.Content {
		switch typed := c.(type) {
		case *sdk.TextContent:
			out.Content = append(out.Content, contentPayload{Type: "text", Text: typed.Text})
		default:
			// Non-text content is represented but its body is not exposed. A
			// plugin cannot meaningfully patch an image, and passing binary
			// through JSON would bloat every invocation.
			out.Content = append(out.Content, contentPayload{Type: "other"})
		}
	}
	return out
}

// applyResult folds a plugin's patched result back onto the original.
//
// Only text content is written back, and only where the original had text. A
// plugin cannot introduce content the backend did not return, change a content
// block's type, or alter `isError` — all of which would let a plugin fabricate a
// backend response rather than shape one.
func applyResult(original *sdk.CallToolResult, patched resultBody) *sdk.CallToolResult {
	out := &sdk.CallToolResult{
		IsError:           original.IsError,
		StructuredContent: original.StructuredContent,
		Meta:              original.Meta,
		Content:           make([]sdk.Content, 0, len(original.Content)),
	}
	for i, c := range original.Content {
		text, ok := c.(*sdk.TextContent)
		if !ok || i >= len(patched.Content) {
			out.Content = append(out.Content, c)
			continue
		}
		out.Content = append(out.Content, &sdk.TextContent{
			Text: patched.Content[i].Text,
			Meta: text.Meta,
		})
	}
	return out
}

func asObject(arguments any) (map[string]any, error) {
	if arguments == nil {
		return map[string]any{}, nil
	}
	raw, err := json.Marshal(arguments)
	if err != nil {
		return nil, fmt.Errorf("wiring: encoding arguments: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		// Non-object arguments are legal JSON but not legal MCP tool arguments.
		return nil, fmt.Errorf("wiring: tool arguments are not a JSON object: %w", err)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

func inputResponsesOf(responses sdk.InputResponseMap) map[string]any {
	if len(responses) == 0 {
		return nil
	}
	raw, err := json.Marshal(responses)
	if err != nil {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// decodeInputRequests turns a plugin's declared input requests into the SDK's
// typed map.
//
// Only elicitation is supported. Sampling and roots are server→client requests
// the gateway would have to originate, which stateless mode forbids — so a plugin
// asking for them is asking for something that cannot happen, and saying so is
// better than silently dropping it.
func decodeInputRequests(raw []byte) (sdk.InputRequestMap, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var declared map[string]struct {
		Message string `json:"message"`
		Schema  any    `json:"requestedSchema,omitempty"`
	}
	if err := json.Unmarshal(raw, &declared); err != nil {
		return nil, fmt.Errorf("wiring: a plugin's input requests are unreadable: %w", err)
	}
	out := make(sdk.InputRequestMap, len(declared))
	for id, req := range declared {
		if req.Message == "" {
			return nil, fmt.Errorf(
				"wiring: input request %q has no message; a human has to be told what they are approving", id)
		}
		out[id] = &sdk.ElicitParams{Message: req.Message}
	}
	return out, nil
}

func effectClassName(ec snapshotpb.EffectClass) string {
	switch ec {
	case snapshotpb.EffectClass_EFFECT_CLASS_WRITE:
		return "write"
	case snapshotpb.EffectClass_EFFECT_CLASS_DESTRUCTIVE:
		return "destructive"
	default:
		return "read"
	}
}
