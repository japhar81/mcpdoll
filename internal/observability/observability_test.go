// Copyright 2026 The MCPDoll Authors.

package observability

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func TestSetupRequiresServiceName(t *testing.T) {
	_, err := Setup(context.Background(), Options{})
	require.ErrorContains(t, err, "ServiceName is required")
}

// TestSetupWithoutEndpointStillTraces: with no collector configured the
// pipeline must still mint span contexts, because log correlation depends on
// trace ids existing and no call site should have to branch on whether
// telemetry is exported.
func TestSetupWithoutEndpointStillTraces(t *testing.T) {
	p, err := Setup(context.Background(), Options{
		ServiceName: "mcpdoll-test",
		SampleRatio: 1.0,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, p.Shutdown(context.Background())) })

	_, span := p.Tracer.Start(context.Background(), "op")
	defer span.End()
	require.True(t, span.SpanContext().IsValid())
	require.True(t, span.SpanContext().TraceID().IsValid())
}

func TestSetupInstallsW3CPropagator(t *testing.T) {
	p, err := Setup(context.Background(), Options{ServiceName: "svc", SampleRatio: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	fields := otel.GetTextMapPropagator().Fields()
	require.Contains(t, fields, "traceparent")
	require.Contains(t, fields, "baggage")
}

func TestNewMetricsRegistersEverything(t *testing.T) {
	p := NoopProvider()
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	m, err := NewMetrics(p.Meter)
	require.NoError(t, err)

	// Every field, not a spot check. A newly added instrument that nobody
	// remembered to construct is a nil interface that panics on the serving
	// path the first time it is used — and a spot check is exactly the thing
	// that would not have covered the new one.
	v := reflect.ValueOf(*m)
	for i := 0; i < v.NumField(); i++ {
		field := v.Type().Field(i)
		if !field.IsExported() {
			continue
		}
		require.Falsef(t, v.Field(i).IsNil(),
			"Metrics.%s was declared but never constructed in NewMetrics", field.Name)
	}

	// Recording must not panic with the noop meter provider.
	ctx := context.Background()
	m.ToolCalls.Add(ctx, 1)
	m.ToolLatency.Record(ctx, 12.5)
	m.SnapshotVersion.Record(ctx, 7)
}

// TestExtractFromMetaContinuesTheTrace is the propagation requirement: a trace
// that starts in the client's agent framework must stay one trace through the
// gateway. Regenerating context here would sever the client from the backend.
func TestExtractFromMetaContinuesTheTrace(t *testing.T) {
	p := NoopProvider()
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	const parentSpanID = "00f067aa0ba902b7"
	meta := map[string]any{
		MetaTraceparent: "00-" + traceID + "-" + parentSpanID + "-01",
		MetaTracestate:  "vendor=opaque",
		// An unrelated `_meta` entry must be ignored, not fed to the propagator.
		"progressToken": "tok-1",
	}

	ctx := ExtractFromMeta(context.Background(), meta)
	sc := trace.SpanContextFromContext(ctx)
	require.True(t, sc.IsValid(), "traceparent should have been adopted")
	require.Equal(t, traceID, sc.TraceID().String())
	require.Equal(t, parentSpanID, sc.SpanID().String())
	require.True(t, sc.IsSampled())

	// A span started from that context inherits the trace id and becomes a
	// child of the client's span.
	_, span := p.Tracer.Start(ctx, "gateway.tools/call")
	defer span.End()
	require.Equal(t, traceID, span.SpanContext().TraceID().String(),
		"the gateway's span must join the client's trace, not start a new one")
	require.NotEqual(t, parentSpanID, span.SpanContext().SpanID().String())
}

func TestExtractFromMetaTolerantOfBadInput(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	cases := []map[string]any{
		nil,
		{},
		{MetaTraceparent: "garbage"},
		{MetaTraceparent: 42},                  // wrong type
		{MetaTraceparent: "00-tooshort-00-01"}, // malformed
		{"unrelated": "value"},
	}
	for _, meta := range cases {
		ctx := ExtractFromMeta(context.Background(), meta)
		require.False(t, trace.SpanContextFromContext(ctx).IsValid(),
			"invalid input must leave the context without a parent rather than fabricate one: %v", meta)
	}
}

// TestInjectIntoMetaRoundTrips proves an instrumented backend can join the
// gateway's trace.
func TestInjectIntoMetaRoundTrips(t *testing.T) {
	p := NoopProvider()
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	ctx, span := p.Tracer.Start(context.Background(), "dispatch")
	defer span.End()

	meta := map[string]any{}
	InjectIntoMeta(ctx, meta)
	require.Contains(t, meta, MetaTraceparent)

	// Extracting on the far side yields exactly the span we injected.
	got := trace.SpanContextFromContext(ExtractFromMeta(context.Background(), meta))
	require.Equal(t, span.SpanContext().TraceID(), got.TraceID())
	require.Equal(t, span.SpanContext().SpanID(), got.SpanID())
}

func TestInjectIntoNilMetaIsSafe(t *testing.T) {
	p := NoopProvider()
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	ctx, span := p.Tracer.Start(context.Background(), "op")
	defer span.End()
	require.NotPanics(t, func() { InjectIntoMeta(ctx, nil) })
}

func TestMetaCarrierKeysOnlyExposesPropagationKeys(t *testing.T) {
	c := metaCarrier{meta: map[string]any{
		MetaTraceparent: "tp",
		MetaBaggage:     "bg",
		"progressToken": "tok",
		"anything":      1,
	}}
	require.ElementsMatch(t, []string{MetaTraceparent, MetaBaggage}, c.Keys(),
		"unrelated _meta keys must not be handed to the propagator")
	require.Equal(t, "tp", c.Get(MetaTraceparent))
	require.Empty(t, c.Get("progressToken"))
	require.Empty(t, c.Get("absent"))
}

func TestProviderShutdownIsIdempotent(t *testing.T) {
	p := NoopProvider()
	require.NoError(t, p.Shutdown(context.Background()))
	require.NoError(t, p.Shutdown(context.Background()))
}

// TestEveryInstrumentIsRecordedSomewhere reads the source.
//
// An instrument that is declared and never written produces no series, which
// means an operator who builds an alert on it gets a rule that never fires and
// a dashboard panel that reads "No data" forever. Both look like the system is
// quiet when in fact nobody is measuring.
//
// This check found fourteen such instruments the first time it ran: some were
// for features that had since been built and never wired up, and some were for
// features that do not exist. The first kind got wired; the second kind got
// deleted, because a metric arrives with its feature.
func TestEveryInstrumentIsRecordedSomewhere(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..", "..")
	src, err := os.ReadFile(filepath.Join(root, "internal", "observability", "metrics.go"))
	require.NoError(t, err)

	fields := instrumentField.FindAllStringSubmatch(string(src), -1)
	require.NotEmpty(t, fields, "no instruments found — the struct's shape changed")

	// Every .go file outside this package, so a recording here would not count
	// as usage of itself.
	var sources []string
	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "bin", "proto":
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "metrics.go") {
			sources = append(sources, path)
		}
		return nil
	}))

	var corpus strings.Builder
	for _, path := range sources {
		body, err := os.ReadFile(path)
		require.NoError(t, err)
		corpus.Write(body)
	}
	text := corpus.String()

	var orphans []string
	for _, match := range fields {
		name := match[1]
		// `.Name.Add(` or `.Name.Record(` — the two ways an instrument is
		// written. A mere mention (passing Metrics around) does not count.
		if !strings.Contains(text, "."+name+".Add(") &&
			!strings.Contains(text, "."+name+".Record(") {
			orphans = append(orphans, name)
		}
	}
	sort.Strings(orphans)

	require.Emptyf(t, orphans,
		"instrument(s) declared but never recorded: %s\n\n"+
			"Either wire it where the feature lives, or delete it. A metric that "+
			"cannot emit is a dashboard panel that reads \"No data\" forever and "+
			"an alert that never fires.", strings.Join(orphans, ", "))
}

var instrumentField = regexp.MustCompile(`(?m)^\t(\w+)\s+metric\.\w+$`)
