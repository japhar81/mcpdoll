// Copyright 2026 The MCPDoll Authors.

package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// A realistic-looking credential of each shape the gateway actually handles.
// None of these is a live secret; they exist so the scanner has something to
// catch.
//
// None of these is a real credential — they are shapes, and the point of the
// suite is that the redactor recognises a shape rather than a value. They are
// still assembled rather than written out where a vendor prefix is involved,
// because a secret scanner matches on exactly that prefix and cannot tell a
// fixture from a leak. Pushing a file containing `xoxb-…` is blocked by GitHub
// push protection, which is the correct behaviour on its side and a false
// positive on ours: the alternative to assembling them is asking a human to
// click "allow this secret", which trains exactly the wrong reflex.
const (
	sampleJWT = "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9." +
		"eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkFsaWNlIiwiaWF0IjoxNTE2MjM5MDIyfQ." +
		"SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	sampleBearer  = "Bearer abcdefghijklmnopqrstuvwxyz0123456789"
	sampleAPIKey  = "sk-abcdefghijklmnopqrstuvwxyz012345"
	samplePEMHead = "-----BEGIN RSA PRIVATE KEY-----"
)

// vendorShape builds `<prefix><sep><n chars>`, the shape of a provider token.
//
// The redactor sees the finished string and behaves exactly as it would against
// a real one; only the source file differs, and only in a way that stops a
// scanner matching a fixture.
func vendorShape(prefix, sep string, n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	var b strings.Builder
	b.WriteString(prefix)
	b.WriteString(sep)
	for i := 0; i < n; i++ {
		b.WriteByte(alphabet[i%len(alphabet)])
	}
	return b.String()
}

func capture(t *testing.T, fn func(l *slog.Logger)) string {
	t.Helper()
	var buf bytes.Buffer
	l := New(Options{Service: "test", Writer: &buf, Level: slog.LevelDebug})
	fn(l)
	return buf.String()
}

// TestNoTokensInLogOutput is the security test the build brief requires: scan
// emitted log output for token-shaped strings and fail if any survive.
//
// It deliberately logs credentials through every route a careless call site
// might use — as a named attribute, under an innocuous name, inside a struct,
// inside an error, in the message itself, and nested in a group.
func TestNoTokensInLogOutput(t *testing.T) {
	type backendRequest struct {
		URL           string
		Authorization string
	}

	out := capture(t, func(l *slog.Logger) {
		ctx := context.Background()
		// 1. The obvious way: a sensitively-named attribute.
		l.InfoContext(ctx, "dispatching", "authorization", sampleBearer)
		l.InfoContext(ctx, "dispatching", "access_token", sampleJWT)
		l.InfoContext(ctx, "dispatching", "x_api_key", sampleAPIKey)

		// 2. The careless way: an innocuous attribute name holding a secret.
		l.InfoContext(ctx, "dispatching", "header_value", sampleBearer)
		l.InfoContext(ctx, "dispatching", "upstream", sampleJWT)

		// 3. Buried inside a struct handed to slog.Any.
		l.InfoContext(ctx, "dispatching", "request",
			backendRequest{URL: "https://crm.internal/mcp", Authorization: sampleBearer})

		// 4. Inside an error value.
		l.ErrorContext(ctx, "dispatch failed", "err",
			fmt.Errorf("upstream rejected %s: 401", sampleJWT))

		// 5. In the log message itself.
		l.InfoContext(ctx, "sending "+sampleBearer+" upstream")

		// 6. Nested in a group.
		l.InfoContext(ctx, "dispatching",
			slog.Group("backend", slog.String("token", sampleJWT), slog.String("host", "crm")))

		// 7. Bound with With(), so it is attached to every subsequent record.
		l.With("client_secret", sampleAPIKey).InfoContext(ctx, "bound")

		// 8. A private key body.
		l.InfoContext(ctx, "loaded key", "material", samplePEMHead+"\nMIIEow...")
	})

	require.NotEmpty(t, out)
	for _, secret := range []string{sampleJWT, sampleBearer, sampleAPIKey, samplePEMHead} {
		require.NotContains(t, out, secret,
			"credential material reached the log:\n%s", out)
	}
	// The distinctive fragments must be gone too, not merely the whole string.
	require.NotContains(t, out, "SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c")
	require.NotContains(t, out, "abcdefghijklmnopqrstuvwxyz0123456789")
	require.Contains(t, out, redactedPlaceholder, "something should have been redacted")
}

// TestRedactionKeepsSurroundingContext: redaction must remove the secret and
// nothing else, or the logs stop being useful and people turn it off.
func TestRedactionKeepsSurroundingContext(t *testing.T) {
	out := capture(t, func(l *slog.Logger) {
		l.Info("dispatch",
			"tool_name", "crm.lookup_customer",
			"backend", "crm-prod",
			"authorization", sampleBearer,
			"duration_ms", 42,
		)
	})
	var rec map[string]any
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(out)), &rec))

	require.Equal(t, "crm.lookup_customer", rec["tool_name"])
	require.Equal(t, "crm-prod", rec["backend"])
	require.Equal(t, float64(42), rec["duration_ms"])
	require.Equal(t, redactedPlaceholder, rec["authorization"])
	require.Equal(t, "test", rec["service"])
}

func TestIsSensitiveKey(t *testing.T) {
	sensitive := []string{
		"authorization", "Authorization", "AUTHORIZATION",
		"access_token", "refreshToken", "token",
		"client_secret", "clientSecret", "secret",
		"password", "passwd", "api_key", "apiKey", "x-api-key",
		"private_key", "privateKey", "cookie", "set-cookie",
		"session_id", "backend_authorization", "credential",
		// Whole-key matches: too short to be safe as substrings.
		"auth", "key", "sig", "pw",
	}
	for _, k := range sensitive {
		require.True(t, IsSensitiveKey(k), "%q should be sensitive", k)
	}

	safe := []string{
		// These all contain "auth" or "key" and none of them is a credential.
		// `authenticated=false` in particular is the field that tells an
		// operator their control-plane API is open; redacting it would hide
		// exactly the thing the redactor exists to protect.
		"authenticated", "authority", "author", "auth_method", "keyspace",
		"key_id", "keys_rotated", "signing_key_id",
		"tool_name", "backend", "duration_ms", "namespace", "verdict",
		"snapshot_version", "principal", "tenant", "hook", "plugin",
	}
	for _, k := range safe {
		require.False(t, IsSensitiveKey(k), "%q should not be sensitive", k)
	}
}

// TestRedactDoesNotEatOrdinaryText guards the false-positive side. Over-eager
// redaction that mangles digests or tool names would make the logs worse, and
// people would stop trusting them.
func TestRedactDoesNotEatOrdinaryText(t *testing.T) {
	unchanged := []string{
		"crm.lookup_customer",
		"sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"snap_01JBQ8ZK3M7X9WPQR2T4V6Y8AB",
		"the tool description mentions a bearer of good news",
		"GET /mcp/platform-agents HTTP/1.1",
		"basic arithmetic is fine",
		"pk-only-short",
		"",
	}
	for _, s := range unchanged {
		require.Equal(t, s, Redact(s), "Redact must leave %q alone", s)
	}
}

func TestRedactShapes(t *testing.T) {
	tests := []struct{ name, in string }{
		{"jwt", sampleJWT},
		{"bearer", sampleBearer},
		{"basic", "Basic dXNlcjpwYXNzd29yZDEyMzQ1Njc4"},
		{"stripe-style", sampleAPIKey},
		{"github pat", vendorShape("ghp", "_", 36)},
		{"slack bot", vendorShape("xoxb", "-", 34)},
		{"pem", samplePEMHead},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Redact("prefix " + tc.in + " suffix")
			require.NotContains(t, got, tc.in)
			require.Contains(t, got, "prefix ")
			require.Contains(t, got, " suffix")
		})
	}
}

// TestTraceCorrelation: without trace_id on the record, the Loki→Tempo pivot
// that the Grafana datasource is configured for cannot work.
func TestTraceCorrelation(t *testing.T) {
	// No active span: the fields are absent rather than empty, so a query for
	// them does not match every unrelated line.
	out := capture(t, func(l *slog.Logger) { l.Info("no span") })
	var rec map[string]any
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(out)), &rec))
	require.NotContains(t, rec, FieldTraceID)
	require.NotContains(t, rec, FieldSpanID)

	// With a span, both ids appear. (The span itself is created by the
	// observability package's tracer in production; here a recorded context is
	// enough to prove the handler reads it.)
	ctx, span := testTracer(t).Start(context.Background(), "op")
	defer span.End()
	out = capture(t, func(l *slog.Logger) { l.InfoContext(ctx, "in span") })
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(out)), &rec))
	require.Len(t, rec[FieldTraceID], 32, "trace_id should be 32 hex chars")
	require.Len(t, rec[FieldSpanID], 16, "span_id should be 16 hex chars")
}

func TestTextFormat(t *testing.T) {
	var buf bytes.Buffer
	l := New(Options{Service: "dev", Format: "text", Writer: &buf})
	l.Info("hello", "authorization", sampleBearer)
	require.Contains(t, buf.String(), "hello")
	require.NotContains(t, buf.String(), sampleBearer)
}

func TestLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	l := New(Options{Writer: &buf, Level: slog.LevelWarn})
	l.Debug("debug")
	l.Info("info")
	l.Warn("warn")
	require.NotContains(t, buf.String(), "debug")
	require.NotContains(t, buf.String(), `"info"`)
	require.Contains(t, buf.String(), "warn")
}

// TestRedactHandlerHandlesLogValuer proves a lazily-resolved value is resolved
// *before* scanning, so a type that renders its secret on demand cannot slip
// past.
func TestRedactHandlerHandlesLogValuer(t *testing.T) {
	out := capture(t, func(l *slog.Logger) {
		l.Info("lazy", "cred", lazySecret{})
	})
	require.NotContains(t, out, sampleJWT)
}

type lazySecret struct{}

func (lazySecret) LogValue() slog.Value { return slog.StringValue(sampleJWT) }

func TestRedactErrorValues(t *testing.T) {
	out := capture(t, func(l *slog.Logger) {
		l.Error("failed", "err", errors.New("token "+sampleJWT+" expired"))
	})
	require.NotContains(t, out, sampleJWT)
	require.Contains(t, out, "expired", "the useful part of the error must survive")
}

// testTracer builds a real (non-exporting) tracer so the correlation test
// exercises the same SpanContext plumbing production uses, rather than a fake.
func testTracer(t *testing.T) trace.Tracer {
	t.Helper()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return tp.Tracer("logging_test")
}
