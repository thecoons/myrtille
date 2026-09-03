package main

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
)

// initTracing sets up the global TracerProvider stubservice's handlers
// record spans on, exporting via OTLP/HTTP. No endpoint is configured
// explicitly: otlptracehttp defaults to localhost:4318, the standard
// OTLP/HTTP port — the same one k6/x/oteltrace listens on when
// service.traces.enabled — so this needs zero configuration to talk to
// myrtille, exactly the same way stubservice's /metrics needs no
// configuration to be scraped by service.metrics.url.
//
// WithInsecure() is required: confirmed against a real standalone run that
// otlptracehttp defaults to *https* even with no endpoint configured
// ("dial tcp [::1]:4318: connect: connection refused" turned out to be
// https://localhost:4318, not http://) — k6/x/oteltrace's receiver only
// ever speaks plain HTTP (see its own doc comment on why: OTLP/HTTP only,
// no gRPC, in this v1), so without this option every export would fail
// with a TLS handshake error against a plain HTTP server, not just when
// nothing is listening at all.
//
// Export failures (nothing listening — e.g. running stubservice
// standalone, or service.traces.enabled unset) are non-fatal by design in
// the OTel SDK: a batch that fails to export is logged and dropped, never
// crashes the exporting process — verified against a real run, see
// docs/plans/otel-span-metrics.md tranche 5.
func initTracing(ctx context.Context) (shutdown func(context.Context) error, err error) {
	exporter, err := otlptracehttp.New(ctx, otlptracehttp.WithInsecure())
	if err != nil {
		return nil, err
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName("stubservice")),
	)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	return tp.Shutdown, nil
}

var tracer = otel.Tracer("stubservice")

// spanAttr is a small convenience alias so handler code below doesn't need
// its own "go.opentelemetry.io/otel/attribute" import just for this.
var spanAttr = attribute.String
