// Copyright 2026 The MCPDoll Authors.

// Package observability owns MCPDoll's OpenTelemetry setup and the semantic
// conventions every component emits.
//
// The rest of the codebase depends on this package's small interfaces, never on
// the OTel SDK directly. That keeps telemetry a wiring concern rather than a
// dependency that reaches into every function signature, and it means a test
// can run the real pipeline with telemetry pointed nowhere.
package observability

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// ScopeName is the instrumentation scope every MCPDoll tracer and meter uses.
const ScopeName = "github.com/mcpdoll/mcpdoll"

// Attribute keys. MCP-specific attributes are namespaced `mcp.*`; MCPDoll's own
// concepts are `mcpdoll.*`. Reusing the same keys across spans, metrics, and
// logs is what makes an exemplar clickable from a dashboard to a trace.
const (
	AttrProtocolVersion = attribute.Key("mcp.protocol_version")
	AttrMethod          = attribute.Key("mcp.method")
	AttrToolName        = attribute.Key("mcp.tool.name")
	AttrToolDigest      = attribute.Key("mcpdoll.tool.digest")
	AttrNamespace       = attribute.Key("mcpdoll.namespace")
	AttrServer          = attribute.Key("mcpdoll.server")
	AttrBackend         = attribute.Key("mcpdoll.backend")
	AttrTenant          = attribute.Key("mcpdoll.tenant")
	AttrBundle          = attribute.Key("mcpdoll.bundle")
	AttrSnapshot        = attribute.Key("mcpdoll.snapshot.version")
	AttrHook            = attribute.Key("mcpdoll.hook")
	AttrPlugin          = attribute.Key("mcpdoll.plugin")
	AttrPluginRuntime   = attribute.Key("mcpdoll.plugin.runtime")
	AttrRollout         = attribute.Key("mcpdoll.plugin.rollout")
	AttrVerdict         = attribute.Key("mcpdoll.verdict")
	AttrEffectClass     = attribute.Key("mcpdoll.effect_class")
	AttrOutcome         = attribute.Key("mcpdoll.outcome")
	AttrSkipReason      = attribute.Key("mcpdoll.skip_reason")
	AttrDriftClass      = attribute.Key("mcpdoll.drift.class")
	AttrHealthState     = attribute.Key("mcpdoll.health.state")
	AttrAdmissionStage  = attribute.Key("mcpdoll.admission.stage")
	AttrProject         = attribute.Key("mcpdoll.project")
	AttrTeam            = attribute.Key("mcpdoll.team")
	AttrPrincipal       = attribute.Key("mcpdoll.principal")
	AttrCacheResult     = attribute.Key("mcpdoll.cache.result")
	AttrErrorKind       = attribute.Key("mcpdoll.error_kind")
)

// Provider holds the configured telemetry pipeline.
type Provider struct {
	Tracer trace.Tracer
	Meter  metric.Meter

	shutdownOnce sync.Once
	shutdownErr  error
	shutdown     []func(context.Context) error
}

// Options configures [Setup].
type Options struct {
	// ServiceName is the component, e.g. "mcpdoll-dp".
	ServiceName string
	// ServiceVersion is the build version.
	ServiceVersion string
	// OTLPEndpoint is the collector's base HTTP URL. Empty means "create spans
	// but export nothing", which is the right behaviour for tests and for a
	// deployment with no collector: trace ids still exist, so logs stay
	// correlatable, and no code path has to branch on whether tracing is on.
	OTLPEndpoint string
	// SampleRatio in [0,1]. Parent-based, so a sampled inbound request stays
	// sampled through the whole gateway.
	SampleRatio float64
	// MetricInterval is how often metrics are pushed. Defaults to 15s.
	MetricInterval time.Duration
}

// Setup builds the telemetry pipeline and installs the global propagator.
//
// The returned Provider's Shutdown must be called on a clean exit, or the last
// batch of spans and the final metric interval are lost — which is exactly the
// data you want when diagnosing why the process exited.
func Setup(ctx context.Context, opts Options) (*Provider, error) {
	if opts.ServiceName == "" {
		return nil, errors.New("observability: ServiceName is required")
	}
	if opts.MetricInterval <= 0 {
		opts.MetricInterval = 15 * time.Second
	}

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(opts.ServiceName),
		semconv.ServiceVersion(opts.ServiceVersion),
		// Matches RAGdoll's convention so both products' telemetry lands in one
		// namespace and can be correlated in a shared Grafana.
		semconv.ServiceNamespace("mcpdoll"),
	))
	if err != nil {
		return nil, fmt.Errorf("observability: building resource: %w", err)
	}

	p := &Provider{}

	// W3C trace context plus baggage, installed globally. This is the only
	// propagator MCPDoll uses: incoming context is *propagated*, never
	// regenerated, so a trace that starts in the client's agent framework stays
	// one trace through the gateway and into the backend.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	traceOpts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(opts.SampleRatio))),
	}
	if opts.OTLPEndpoint != "" {
		exp, err := otlptracehttp.New(ctx,
			otlptracehttp.WithEndpointURL(opts.OTLPEndpoint+"/v1/traces"))
		if err != nil {
			return nil, fmt.Errorf("observability: trace exporter: %w", err)
		}
		traceOpts = append(traceOpts, sdktrace.WithBatcher(exp))
	}
	tp := sdktrace.NewTracerProvider(traceOpts...)
	otel.SetTracerProvider(tp)
	p.shutdown = append(p.shutdown, tp.Shutdown)
	p.Tracer = tp.Tracer(ScopeName)

	metricOpts := []sdkmetric.Option{sdkmetric.WithResource(res)}
	if opts.OTLPEndpoint != "" {
		exp, err := otlpmetrichttp.New(ctx,
			otlpmetrichttp.WithEndpointURL(opts.OTLPEndpoint+"/v1/metrics"))
		if err != nil {
			return nil, fmt.Errorf("observability: metric exporter: %w", err)
		}
		metricOpts = append(metricOpts, sdkmetric.WithReader(
			sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(opts.MetricInterval))))
	}
	mp := sdkmetric.NewMeterProvider(metricOpts...)
	otel.SetMeterProvider(mp)
	p.shutdown = append(p.shutdown, mp.Shutdown)
	p.Meter = mp.Meter(ScopeName)

	return p, nil
}

// Shutdown flushes and stops every configured exporter. Errors from all of them
// are joined rather than short-circuited, so one failing exporter does not hide
// another's.
func (p *Provider) Shutdown(ctx context.Context) error {
	// Idempotent: a service that shuts down from both a signal handler and a
	// deferred call must not report the second call's "already shut down" as a
	// failure, or a clean exit looks like a broken one.
	p.shutdownOnce.Do(func() {
		var errs []error
		for _, fn := range p.shutdown {
			if err := fn(ctx); err != nil {
				errs = append(errs, err)
			}
		}
		p.shutdownErr = errors.Join(errs...)
	})
	return p.shutdownErr
}

// NoopProvider returns a Provider that creates real spans (so trace ids exist
// and log correlation works) but exports nothing. Tests and the `file`
// snapshot-source mode use it.
func NoopProvider() *Provider {
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	mp := sdkmetric.NewMeterProvider()
	return &Provider{
		Tracer:   tp.Tracer(ScopeName),
		Meter:    mp.Meter(ScopeName),
		shutdown: []func(context.Context) error{tp.Shutdown, mp.Shutdown},
	}
}

// MetricAttrs is a terser spelling of metric.WithAttributes, so a call site that
// records a measurement reads as the measurement rather than as the plumbing.
func MetricAttrs(kv ...attribute.KeyValue) metric.MeasurementOption {
	return metric.WithAttributes(kv...)
}
