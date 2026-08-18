// Copyright 2026 The MCPDoll Authors.

// Package logging configures MCPDoll's structured logging and defines the
// field vocabulary every component uses.
//
// Two rules make these logs useful rather than merely voluminous:
//
//  1. Every record emitted inside a span carries `trace_id` and `span_id`, so
//     Grafana can pivot from a Tempo span straight to the lines it produced.
//     The Loki datasource's derived field matches on `"trace_id":"…"`.
//
//  2. Credentials never reach a log. That is enforced mechanically by
//     [RedactHandler] rather than trusted to call sites, because a gateway
//     handles other people's bearer tokens on every single request and one
//     careless `slog.Any("request", req)` would spill them.
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"

	"go.opentelemetry.io/otel/trace"
)

// The field vocabulary. Using these constants rather than bare strings is what
// keeps a query like `{service="mcpdoll-dp"} | json | tool_name="crm.lookup"`
// working across every component.
const (
	FieldTraceID    = "trace_id"
	FieldSpanID     = "span_id"
	FieldRequestID  = "request_id"
	FieldPrincipal  = "principal"
	FieldTenant     = "tenant"
	FieldProject    = "project"
	FieldAudience   = "audience"
	FieldBundle     = "bundle"
	FieldNamespace  = "namespace"
	FieldServer     = "server"
	FieldToolName   = "tool_name"
	FieldToolDigest = "tool_digest"
	FieldSnapshot   = "snapshot_version"
	FieldHook       = "hook"
	FieldPlugin     = "plugin"
	FieldVerdict    = "verdict"
	FieldEffect     = "effect_class"
	FieldDurationMS = "duration_ms"
	FieldBudgetMS   = "budget_ms"
	FieldOutcome    = "outcome"
	FieldErrorKind  = "error_kind"
	FieldBackend    = "backend"
	FieldProtocol   = "protocol_version"
	FieldDriftClass = "drift_class"
)

// Options configures [New].
type Options struct {
	// Level is the minimum level to emit. Defaults to Info.
	Level slog.Level
	// Format is "json" (default) or "text". Text is for a developer's
	// terminal; anything shipping to Loki should stay JSON.
	Format string
	// Service names the emitting component, e.g. "mcpdoll-dp".
	Service string
	// Writer defaults to os.Stderr when nil.
	Writer io.Writer
}

// New builds the process logger: a JSON (or text) handler wrapped in trace
// correlation and secret redaction.
func New(opts Options) *slog.Logger {
	w := opts.Writer
	if w == nil {
		w = defaultWriter
	}
	handlerOpts := &slog.HandlerOptions{Level: opts.Level}

	var base slog.Handler
	if strings.EqualFold(opts.Format, "text") {
		base = slog.NewTextHandler(w, handlerOpts)
	} else {
		base = slog.NewJSONHandler(w, handlerOpts)
	}

	h := slog.Handler(&RedactHandler{Inner: &traceHandler{Inner: base}})
	logger := slog.New(h)
	if opts.Service != "" {
		logger = logger.With("service", opts.Service)
	}
	return logger
}

// traceHandler stamps the active span's ids onto every record.
type traceHandler struct{ Inner slog.Handler }

func (h *traceHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.Inner.Enabled(ctx, l)
}

func (h *traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String(FieldTraceID, sc.TraceID().String()),
			slog.String(FieldSpanID, sc.SpanID().String()),
		)
	}
	return h.Inner.Handle(ctx, r)
}

func (h *traceHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &traceHandler{Inner: h.Inner.WithAttrs(as)}
}

func (h *traceHandler) WithGroup(name string) slog.Handler {
	return &traceHandler{Inner: h.Inner.WithGroup(name)}
}

// redactedPlaceholder is what a redacted value becomes. It is deliberately
// distinctive so a grep for it finds every near-miss during review.
const redactedPlaceholder = "[REDACTED]"

// sensitiveKeys are attribute names whose value is never logged.
//
// Matching is against a *normalized* key — lowercased with separators removed
// — so one entry covers every spelling the same concept arrives under:
// `api_key`, `apiKey`, `x-api-key`, and `API-KEY` all normalize to `apikey`.
// Matching is by substring, which catches `backend_authorization` and
// `upstream_client_secret` without enumerating them.
//
// Entries must therefore be written separator-free.
var sensitiveKeys = []string{
	"authorization", "token", "secret", "password", "passwd", "credential",
	"apikey", "privatekey", "cookie", "sessionid", "bearer", "signature",
	"assertion", "refresh", "auth",
}

// keySeparators are stripped before matching an attribute name.
var keySeparators = strings.NewReplacer("-", "", "_", "", ".", "", " ", "")

// tokenShaped matches credential material by *shape* rather than by the name
// it was logged under. A value that reaches a log through an unexpected path —
// inside a struct, an error string, a URL query — still gets caught.
//
// The patterns are deliberately conservative: each requires either an explicit
// scheme prefix or a structure (three dot-separated base64url segments) that
// ordinary prose and identifiers do not produce.
var tokenShaped = regexp.MustCompile(strings.Join([]string{
	// JWT / JWS compact serialization.
	`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`,
	// HTTP Bearer / Basic credentials.
	`(?i)\b(?:bearer|basic)\s+[A-Za-z0-9._~+/=-]{16,}`,
	// Common provider key prefixes.
	`\b(?:sk|pk|rk|ghp|gho|ghs|ghu|ghr|glpat|xoxb|xoxp|xapp|xoxa|AKIA)[-_][A-Za-z0-9_-]{16,}`,
	// PEM private key blocks.
	`-----BEGIN [A-Z ]*PRIVATE KEY-----`,
}, "|"))

// RedactHandler strips credential material from every record before it is
// written.
//
// This is a defence in depth, not a substitute for care at the call site: it
// exists because the cost of one leaked bearer token is unbounded and the cost
// of scanning every attribute is a few hundred nanoseconds on a path that is
// already doing I/O.
type RedactHandler struct{ Inner slog.Handler }

func (h *RedactHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.Inner.Enabled(ctx, l)
}

func (h *RedactHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, Redact(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redactAttr(a))
		return true
	})
	return h.Inner.Handle(ctx, out)
}

func (h *RedactHandler) WithAttrs(as []slog.Attr) slog.Handler {
	redacted := make([]slog.Attr, len(as))
	for i, a := range as {
		redacted[i] = redactAttr(a)
	}
	return &RedactHandler{Inner: h.Inner.WithAttrs(redacted)}
}

func (h *RedactHandler) WithGroup(name string) slog.Handler {
	return &RedactHandler{Inner: h.Inner.WithGroup(name)}
}

func redactAttr(a slog.Attr) slog.Attr {
	if IsSensitiveKey(a.Key) {
		return slog.String(a.Key, redactedPlaceholder)
	}
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindGroup:
		attrs := v.Group()
		out := make([]any, 0, len(attrs))
		for _, sub := range attrs {
			out = append(out, redactAttr(sub))
		}
		return slog.Group(a.Key, out...)
	case slog.KindString:
		return slog.String(a.Key, Redact(v.String()))
	case slog.KindAny:
		// Structs, errors, and maps stringify unpredictably; scan the rendered
		// form rather than reflecting over the value.
		s := v.String()
		if redacted := Redact(s); redacted != s {
			return slog.String(a.Key, redacted)
		}
		return a
	default:
		return a
	}
}

// IsSensitiveKey reports whether an attribute name denotes credential material.
func IsSensitiveKey(key string) bool {
	normalized := keySeparators.Replace(strings.ToLower(key))
	for _, s := range sensitiveKeys {
		if strings.Contains(normalized, s) {
			return true
		}
	}
	return false
}

// Redact replaces token-shaped substrings in s with a placeholder.
func Redact(s string) string {
	if s == "" {
		return s
	}
	return tokenShaped.ReplaceAllString(s, redactedPlaceholder)
}

// defaultWriter is a package variable so tests can swap it without changing
// the New() signature.
var defaultWriter io.Writer = os.Stderr
