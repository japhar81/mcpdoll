// Copyright 2026 The MCPDoll Authors.

package observability

import (
	"context"

	"go.opentelemetry.io/otel"
)

// The MCP `_meta` keys that carry W3C trace context.
//
// MCP has no transport-level header for this on the JSON-RPC message itself, so
// context travels in the request's `_meta` object. An agent framework that
// instruments its MCP client puts it there; the gateway's job is to pick it up
// and continue the trace, not to start a new one.
const (
	MetaTraceparent = "traceparent"
	MetaTracestate  = "tracestate"
	MetaBaggage     = "baggage"
)

// metaCarrier adapts an MCP `_meta` map to OTel's TextMapCarrier.
//
// Only the three propagation keys are exposed. `_meta` is a general-purpose
// extension point that carries unrelated application data, and handing all of
// it to the propagator would be both wasteful and a way for a client to inject
// keys the propagator did not expect.
type metaCarrier struct {
	meta map[string]any
}

func (c metaCarrier) Get(key string) string {
	if c.meta == nil || !isPropagationKey(key) {
		return ""
	}
	v, ok := c.meta[key].(string)
	if !ok {
		return ""
	}
	return v
}

// isPropagationKey gates both Get and Set to the three W3C keys, so `_meta`
// remains a general extension point that the propagator cannot read from or
// write to beyond its own concern.
func isPropagationKey(key string) bool {
	switch key {
	case MetaTraceparent, MetaTracestate, MetaBaggage:
		return true
	default:
		return false
	}
}

func (c metaCarrier) Set(key, value string) {
	if c.meta == nil || !isPropagationKey(key) {
		return
	}
	c.meta[key] = value
}

func (c metaCarrier) Keys() []string {
	if c.meta == nil {
		return nil
	}
	var out []string
	for _, k := range []string{MetaTraceparent, MetaTracestate, MetaBaggage} {
		if _, ok := c.meta[k]; ok {
			out = append(out, k)
		}
	}
	return out
}

// ExtractFromMeta continues the caller's trace from an MCP request's `_meta`.
//
// If `_meta` carries no valid traceparent the returned context is unchanged and
// the next span starts a new trace. That is the correct fallback: fabricating a
// parent would produce a trace whose root does not exist.
func ExtractFromMeta(ctx context.Context, meta map[string]any) context.Context {
	if len(meta) == 0 {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, metaCarrier{meta: meta})
}

// InjectIntoMeta writes the current span's context into an MCP request's
// `_meta`, so a backend that is itself instrumented joins the same trace.
//
// The map must be non-nil; callers building an outbound request own it.
func InjectIntoMeta(ctx context.Context, meta map[string]any) {
	if meta == nil {
		return
	}
	otel.GetTextMapPropagator().Inject(ctx, metaCarrier{meta: meta})
}
