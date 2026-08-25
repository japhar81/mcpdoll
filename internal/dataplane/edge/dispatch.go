// Copyright 2026 Henry Zektser.

package edge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"

	"github.com/mcpdoll/mcpdoll/internal/dataplane/backends"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/snapshot"
	"github.com/mcpdoll/mcpdoll/internal/observability"
	"github.com/mcpdoll/mcpdoll/internal/platform/logging"
)

// toolHandler builds the dispatch closure for one admitted tool.
//
// The closure captures its tool, server, and audience, so dispatch needs no
// lookup and cannot route to the wrong backend. Each snapshot activation builds
// fresh closures, which is why a handler never has to consult the store.
func (e *Edge) toolHandler(
	view *snapshot.View,
	av *snapshot.PrincipalView,
	tool *snapshot.Tool,
) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		start := time.Now()

		// Continue the client's trace from `_meta` rather than starting a new
		// one, so a call is one trace from the agent framework through the
		// gateway to the backend.
		if req.Params != nil && req.Params.Meta != nil {
			ctx = observability.ExtractFromMeta(ctx, req.Params.Meta)
		}

		ctx, span := e.startSpan(ctx, "mcp.tools/call",
			observability.AttrMethod.String("tools/call"),
			observability.AttrToolName.String(tool.Def.QualifiedName),
			observability.AttrToolDigest.String(tool.Def.Digest),
			observability.AttrNamespace.String(tool.Namespace.Prefix),
			observability.AttrBackend.String(tool.Server.Name),
			observability.AttrTenant.String(tool.Tenant.Slug),
			observability.AttrEffectClass.String(tool.Def.EffectClass.String()),
			observability.AttrSnapshot.Int64(view.Version),
		)
		defer span.End()

		outcome := "ok"
		defer func() {
			e.opts.Metrics.ToolCalls.Add(ctx, 1, metricAttrs(
				observability.AttrToolName.String(tool.Def.QualifiedName),
				observability.AttrTenant.String(tool.Tenant.Slug),
				observability.AttrOutcome.String(outcome),
			))
			e.opts.Metrics.ToolLatency.Record(ctx, float64(time.Since(start).Microseconds())/1000.0,
				metricAttrs(
					observability.AttrToolName.String(tool.Def.QualifiedName),
					observability.AttrOutcome.String(outcome),
				))
		}()

		principal, err := e.principalFor(ctx, req.Extra)
		if err != nil {
			outcome = "unauthenticated"
			recordSpanError(span, err)
			return nil, fmt.Errorf("edge: %w", err)
		}
		span.SetAttributes(observability.AttrPrincipal.String(principal.Subject))
		// The tool's tenant, which is what this call acts in. For a spanning
		// credential (ADR 0027) the view has no single tenant, and even for an
		// ordinary one this is the more direct answer: the tenant a call
		// belongs to is a property of the tool being called.
		principal.Tenant = tool.Tenant.Slug

		// ---- drift block -------------------------------------------------
		// A strict backend whose definition has changed since admission must
		// not have this tool called: the arguments were shaped against a schema
		// the backend no longer implements.
		//
		// Checked before the pipeline rather than after. Running seven hooks to
		// produce a decision that will not be honoured wastes the request
		// budget and writes an audit entry describing a call that never had a
		// chance — and the tool is unfit to serve regardless of what any plugin
		// would have said.
		if e.opts.DriftGuard != nil {
			if reason, blocked := e.opts.DriftGuard.Blocked(tool.Def.QualifiedName); blocked {
				outcome = "drift_blocked"
				span.SetAttributes(observability.AttrOutcome.String(outcome))
				e.log.WarnContext(ctx, "refused a call to a drifted tool",
					logging.FieldToolName, tool.Def.QualifiedName,
					logging.FieldBackend, tool.Server.Name,
					logging.FieldPrincipal, principal.Subject,
					"reason", reason)
				e.opts.Metrics.ToolErrors.Add(ctx, 1, metricAttrs(
					observability.AttrToolName.String(tool.Def.QualifiedName),
					observability.AttrErrorKind.String(outcome),
				))
				return driftBlockedResult(tool, reason), nil
			}
		}

		var arguments any = req.Params.Arguments

		// ---- MRTR retry: unwrap the gateway's requestState envelope --------
		// The client echoed back an opaque token. Verifying it is what binds the
		// approval to this exact call: without the check, a client could present
		// a state issued for a different tool, a different principal, or
		// different arguments and have it honoured.
		var retry Retry
		if req.Params.RequestState != "" {
			retry, err = e.UnwrapForRetry(
				tool, principal.Subject, tool.Tenant.Slug, arguments, req.Params.RequestState)
			if err != nil {
				outcome = "invalid_request_state"
				recordSpanError(span, err)
				e.log.WarnContext(ctx, "rejected an MRTR retry",
					logging.FieldToolName, tool.Def.QualifiedName,
					logging.FieldPrincipal, principal.Subject,
					"err", err)
				return invalidStateResult(tool, err), nil
			}
		}

		// ---- ON_TOOL_CALL ------------------------------------------------
		if e.opts.Pipeline != nil {
			decision, err := e.opts.Pipeline.OnToolCall(ctx, &ToolCallRequest{
				PrincipalView:  av,
				Tool:           tool,
				Principal:      principal,
				Arguments:      arguments,
				Meta:           req.Params.Meta,
				RequestState:   retry.PluginState,
				InputResponses: pluginResponses(retry, req.Params.InputResponses),
			})
			if err != nil {
				outcome = "pipeline_error"
				recordSpanError(span, err)
				return nil, fmt.Errorf("edge: pipeline: %w", err)
			}
			switch {
			case decision.Denied():
				outcome = "denied"
				span.SetAttributes(observability.AttrVerdict.String("deny"))
				// A denial is a tool-level error, not a protocol error, so the
				// model can see the reason and choose differently instead of
				// treating the gateway as broken.
				return deniedResult(tool, decision.Reason), nil
			case decision.Deferred():
				// The plugin's own state is wrapped in the gateway's signed
				// envelope, bound to this principal, audience and argument set.
				// A plugin deferral is an authorization decision about a
				// specific action by a specific person, so an unwrapped state
				// would be forgeable *and* replayable against another target.
				token, err := e.IssuePluginDeferral(
					tool, principal.Subject, tool.Tenant.Slug, arguments, decision.RequestState)
				if err != nil {
					outcome = "mrtr_wrap_error"
					recordSpanError(span, err)
					return nil, fmt.Errorf("edge: wrapping plugin deferral: %w", err)
				}
				outcome = "input_required"
				span.SetAttributes(observability.AttrVerdict.String("defer"))
				return &mcp.CallToolResult{
					InputRequests: decision.InputRequests,
					RequestState:  token,
				}, nil
			}
			if decision.Arguments != nil {
				arguments = decision.Arguments
			}
		}

		// ---- dispatch ----------------------------------------------------
		// Trace context goes outbound in `_meta` so an instrumented backend
		// joins this trace.
		outboundMeta := map[string]any{}
		observability.InjectIntoMeta(ctx, outboundMeta)

		dispatchStart := time.Now()
		res, err := e.opts.Pool.CallTool(ctx, backends.Call{
			Target: backends.Target{
				ServerID: tool.Server.Id,
				TenantID: tool.Tenant.Id,
			},
			ToolName:  tool.Def.Name,
			Arguments: arguments,
			Meta:      outboundMeta,
			// The backend receives its own state, never the gateway's envelope,
			// and only the responses that answer *its* request.
			RequestState:   retry.BackendState,
			InputResponses: backendResponses(retry, req.Params.InputResponses),
		}, principal)
		dispatchMS := float64(time.Since(dispatchStart).Microseconds()) / 1000.0
		e.opts.Metrics.BackendLatency.Record(ctx, dispatchMS, metricAttrs(
			observability.AttrBackend.String(tool.Server.Name),
		))

		if err != nil {
			outcome = classifyDispatchError(err)
			recordSpanError(span, err)
			e.opts.Metrics.BackendDispatches.Add(ctx, 1, metricAttrs(
				observability.AttrBackend.String(tool.Server.Name),
				observability.AttrOutcome.String(outcome),
			))
			e.opts.Metrics.ToolErrors.Add(ctx, 1, metricAttrs(
				observability.AttrToolName.String(tool.Def.QualifiedName),
				observability.AttrErrorKind.String(outcome),
			))
			e.log.WarnContext(ctx, "tool dispatch failed",
				logging.FieldToolName, tool.Def.QualifiedName,
				logging.FieldBackend, tool.Server.Name,
				logging.FieldErrorKind, outcome,
				"err", err)
			return unavailableResult(tool, err), nil
		}

		e.opts.Metrics.BackendDispatches.Add(ctx, 1, metricAttrs(
			observability.AttrBackend.String(tool.Server.Name),
			observability.AttrOutcome.String("ok"),
		))

		// A backend may itself need client input. Its `requestState` has to be
		// wrapped in the gateway's own signed envelope before it reaches the
		// client — see requeststate.go for why an unwrapped one is unsafe.
		if res.NeedsInput() {
			wrapped, err := e.wrapBackendInputRequest(
				tool, principal.Subject, tool.Tenant.Slug, arguments, res)
			if err != nil {
				outcome = "mrtr_wrap_error"
				recordSpanError(span, err)
				return nil, fmt.Errorf("edge: wrapping backend input request: %w", err)
			}
			outcome = "input_required"
			span.SetAttributes(observability.AttrVerdict.String("backend_defer"))
			return wrapped, nil
		}

		// ---- ON_TOOL_RESULT ----------------------------------------------
		if e.opts.Pipeline != nil {
			decision, err := e.opts.Pipeline.OnToolResult(ctx, &ToolResultRequest{
				PrincipalView: av,
				Tool:          tool,
				Principal:     principal,
				Result:        res,
			})
			if err != nil {
				outcome = "pipeline_error"
				recordSpanError(span, err)
				return nil, fmt.Errorf("edge: pipeline: %w", err)
			}
			if decision.Denied() {
				outcome = "result_denied"
				span.SetAttributes(observability.AttrVerdict.String("deny"))
				return deniedResult(tool, decision.Reason), nil
			}
			if decision.Result != nil {
				res = decision.Result
			}
		}

		if res.IsError {
			// The tool said no. That is a normal outcome, distinct from the
			// backend failing, and it must not count against backend health.
			outcome = "tool_error"
		}
		return res, nil
	}
}

// classifyDispatchError maps a dispatch failure to a stable metric label and
// error kind, so a dashboard can distinguish "backend is down" from "we gave up"
// from "the breaker is open".
func classifyDispatchError(err error) string {
	var open *backends.ErrCircuitOpen
	if errors.As(err, &open) {
		return "circuit_open"
	}
	var notConnected *backends.ErrNotConnected
	if errors.As(err, &notConnected) {
		return "unreachable"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	return "error"
}

// deniedResult renders a policy denial as a model-legible tool error.
func deniedResult(tool *snapshot.Tool, reason string) *mcp.CallToolResult {
	if reason == "" {
		reason = "the gateway policy for this tool did not permit the call"
	}
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{
			Text: fmt.Sprintf("%s was not permitted: %s", tool.Def.QualifiedName, reason),
		}},
		Meta: mcp.Meta{
			"mcpdoll": map[string]any{
				"outcome": "denied",
				"reason":  reason,
				"tool":    tool.Def.QualifiedName,
			},
		},
	}
}

// driftBlockedResult refuses a tool whose backend definition has changed.
//
// A tool error rather than a protocol error, like every other refusal here: the
// model should read the reason and choose differently rather than conclude the
// gateway is broken. `retryable: false` is the important field — retrying will
// not help until somebody publishes, and without it a model will try again.
func driftBlockedResult(tool *snapshot.Tool, reason string) *mcp.CallToolResult {
	detail := map[string]any{
		"outcome":   "drift_blocked",
		"tool":      tool.Def.QualifiedName,
		"backend":   tool.Server.Name,
		"retryable": false,
		"reason":    reason,
	}
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{
			&mcp.TextContent{
				Text: fmt.Sprintf(
					"%s cannot be called: %s. This is a configuration state, not a "+
						"transient failure — retrying will not help.",
					tool.Def.QualifiedName, reason),
			},
		},
		Meta: map[string]any{"mcpdoll": detail},
	}
}

// unavailableResult renders a backend failure as a structured, model-legible
// error.
//
// This is the grace-window behaviour: while a backend is unreachable its tools
// stay in the catalog and their calls fail fast with a description the model can
// act on. Removing the tools instead would invalidate every client's prompt
// cache, and an opaque transport error would send the model into a retry loop.
func unavailableResult(tool *snapshot.Tool, cause error) *mcp.CallToolResult {
	detail := map[string]any{
		"outcome": "backend_unavailable",
		"tool":    tool.Def.QualifiedName,
		"backend": tool.Server.Name,
		"kind":    classifyDispatchError(cause),
	}

	advice := "The upstream service for this tool is temporarily unavailable. " +
		"Do not retry immediately; try a different approach or tell the user."

	var open *backends.ErrCircuitOpen
	if errors.As(cause, &open) && !open.Until.IsZero() {
		detail["retry_after"] = open.Until.UTC().Format(time.RFC3339)
		advice = fmt.Sprintf(
			"The upstream service for this tool is unavailable and will not be retried "+
				"before %s. Try a different approach or tell the user.",
			open.Until.UTC().Format(time.RFC3339))
	}

	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{
			Text: fmt.Sprintf("%s is currently unavailable. %s", tool.Def.QualifiedName, advice),
		}},
		Meta: mcp.Meta{"mcpdoll": detail},
	}
}

// ------------------------------------------------------ catalog middleware ----

// catalogMiddleware owns the fields the gateway is uniquely responsible for on a
// list result: the identity filtering, `ttlMs`, and `cacheScope`.
//
// It is middleware because `tools/list` is generated by the SDK from the
// registered tool set — there is no handler of ours to wrap — and because the
// same interception point covers pagination without the edge reimplementing it.
func (e *Edge) catalogMiddleware(view *snapshot.View, av *snapshot.PrincipalView) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/list" {
				return next(ctx, method, req)
			}
			start := time.Now()

			result, err := next(ctx, method, req)
			if err != nil {
				return nil, err
			}
			list, ok := result.(*mcp.ListToolsResult)
			if !ok {
				return result, nil
			}

			ctx, span := e.startSpan(ctx, "mcp.tools/list",
				observability.AttrMethod.String("tools/list"),
				observability.AttrTenant.String(av.TenantLabel()),
				observability.AttrSnapshot.Int64(view.Version),
			)
			defer span.End()

			principal, perr := e.principalFor(ctx, req.GetExtra())
			if perr != nil {
				recordSpanError(span, perr)
				return nil, perr
			}
			// A catalog is not a call and has no single tenant to act in: a
			// spanning credential lists several at once (ADR 0027). The label
			// names them all rather than picking one and inviting the reader
			// to assume it is the only one.
			principal.Tenant = av.TenantLabel()

			// Always identity-filtered now: the catalog *is* the principal's
			// grants (ADR 0016), so there is no unfiltered case left. Kept as a
			// variable rather than folded away because the plugin path below
			// still reads it, and because a future public case would have to
			// be argued for here.
			identityFiltered := true

			// ---- ON_CATALOG ----------------------------------------------
			if e.opts.Pipeline != nil {
				decision, err := e.opts.Pipeline.OnCatalog(ctx, &CatalogRequest{
					PrincipalView: av,
					Principal:     principal,
					Tools:         list.Tools,
				})
				if err != nil {
					recordSpanError(span, err)
					return nil, fmt.Errorf("edge: pipeline: %w", err)
				}
				if decision.Tools != nil {
					list.Tools = decision.Tools
				}
				// A plugin that actually filtered has made this view
				// principal-specific, whatever the manifest claimed.
				if decision.IdentityFiltered {
					identityFiltered = true
				}
			}

			list.TTLMs = av.TTLMs
			list.CacheScope = cacheScopeFor(identityFiltered)

			span.SetAttributes(
				attribute.Int("mcpdoll.catalog.tools", len(list.Tools)),
				attribute.Int("mcpdoll.catalog.ttl_ms", list.TTLMs),
				attribute.String("mcpdoll.catalog.cache_scope", list.CacheScope),
			)
			e.opts.Metrics.CatalogLists.Add(ctx, 1, metricAttrs(
				observability.AttrTenant.String(av.TenantLabel()),
				observability.AttrCacheResult.String(list.CacheScope),
			))
			e.opts.Metrics.CatalogSize.Record(ctx, int64(len(list.Tools)), metricAttrs(
				observability.AttrTenant.String(av.TenantLabel()),
			))
			e.log.DebugContext(ctx, "served catalog",
				logging.FieldTenant, av.TenantLabel(),
				logging.FieldPrincipal, principal.Subject,
				"tools", len(list.Tools),
				"cache_scope", list.CacheScope,
				logging.FieldDurationMS, time.Since(start).Milliseconds())

			return list, nil
		}
	}
}

// CacheScopePublic and CacheScopePrivate are the two values the spec defines.
const (
	CacheScopePublic  = "public"
	CacheScopePrivate = "private"
)

// cacheScopeFor is the single place the value is decided.
//
// It now returns "private" unconditionally. Under ADR 0016 a catalog *is* a
// principal's grants, so the condition that once permitted "public" — nothing
// identity-specific applied — is never true.
//
// The function and its parameter are kept rather than folded into a constant so
// the invariant retains a name, a test, and one place where a future public
// case would have to be argued for. There is exactly one expression in the
// codebase that decides a catalog's cache scope, and this is it.
func cacheScopeFor(identityFiltered bool) string {
	_ = identityFiltered
	return CacheScopePrivate
}

// ------------------------------------------------------------ arguments ------

// backendResponses and pluginResponses route a retry's input responses to
// whoever asked for them.
//
// The keys in an InputResponseMap are chosen by the requester, so the backend's
// namespace and a plugin's namespace are unrelated. Forwarding a plugin's
// responses to a backend makes the backend believe it was answered — it sees a
// non-empty map, takes its second-round branch, and finds none of its own keys.
// The result is a confusing "response missing" error from a backend that was
// never actually asked twice.
func backendResponses(retry Retry, responses mcp.InputResponseMap) mcp.InputResponseMap {
	// A first-round call has no retry at all; pass responses straight through so
	// a client that pre-empted an expected request still works.
	if retry.Source == "" {
		return responses
	}
	if retry.AnswersBackend() {
		return responses
	}
	return nil
}

func pluginResponses(retry Retry, responses mcp.InputResponseMap) mcp.InputResponseMap {
	if retry.Source == "" {
		return responses
	}
	if retry.AnswersPlugin() {
		return responses
	}
	return nil
}

// invalidStateResult renders a rejected MRTR retry.
//
// It is a tool error rather than a protocol error so the model sees why the
// retry failed, but it deliberately does not say *which* check failed: a client
// probing the envelope should not be told whether it got the tool, the
// principal, or the arguments wrong.
func invalidStateResult(tool *snapshot.Tool, cause error) *mcp.CallToolResult {
	kind := "invalid"
	switch {
	case errors.Is(cause, ErrExpiredRequestState):
		kind = "expired"
	case errors.Is(cause, ErrMismatchedRequestState):
		kind = "mismatched"
	}
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{
			Text: fmt.Sprintf(
				"The confirmation for %s could not be verified (%s). Start the operation again.",
				tool.Def.QualifiedName, kind),
		}},
		Meta: mcp.Meta{"mcpdoll": map[string]any{
			"outcome": "invalid_request_state",
			"kind":    kind,
			"tool":    tool.Def.QualifiedName,
		}},
	}
}
