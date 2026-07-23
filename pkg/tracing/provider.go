/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tracing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// TracingMode selects the distributed tracing backend.
// It serves as the single on/off switch for tracing: "none" disables
// tracing entirely, while "otel", "std", and "file" enable it with
// different exporters.
type TracingMode string

const (
	// TracingModeOTel uses OpenTelemetry with OTLP gRPC exporter.
	TracingModeOTel TracingMode = "otel"

	// TracingModeStd exports spans to standard output; useful for local
	// debugging without an OTel Collector.
	TracingModeStd TracingMode = "std"

	// TracingModeFile exports spans to a file at the path specified by
	// Config.FilePath; useful for persistent local debugging without an
	// OTel Collector.
	TracingModeFile TracingMode = "file"

	// TracingModeNone disables tracing entirely. A no-op TracerProvider is
	// installed so that all tracing calls become zero-cost no-ops.
	// This is the default for both sandbox-manager and sandbox-controller.
	TracingModeNone TracingMode = "none"
)

// DefaultEndpoint is the default OTLP gRPC endpoint for tracing export.
// Enterprise deployments may override this via inner_provider.go init().
var DefaultEndpoint = "otel-collector:4317"

// defaultServiceVersion is the fallback service.version resource attribute.
// Deployments should override it via the standard OTel env var
// OTEL_RESOURCE_ATTRIBUTES (e.g. "service.version=1.2.3").
const defaultServiceVersion = "0.1.0"

// Config holds the configuration for distributed tracing.
type Config struct {
	Mode          TracingMode
	Endpoint      string  // OTLP gRPC endpoint, e.g., "otel-collector:4317"
	FilePath      string  // Output file path for "file" mode, e.g., "/tmp/traces.json"
	ServiceName   string  // e.g., "sandbox-controller" or "sandbox-manager"
	SamplingRatio float64 // 0.0 to 1.0, default 1.0
	Insecure      bool    // Use insecure gRPC (dev environment)
}

// InitTracerProvider initializes the global TracerProvider and returns a shutdown function.
// Must be called once at startup, before any controller or HTTP server starts.
//
// When cfg.Mode is "none" (or any unrecognized value), tracing is disabled:
// a no-op TracerProvider is explicitly installed. The OpenTelemetry API is
// designed so that its default global TracerProvider is already a no-op
// (API/SDK separation — libraries can embed tracing calls without the
// application installing an SDK). We set it explicitly here to guarantee a
// clean, deterministic state regardless of any third-party library that might
// have registered a real provider, and to install the W3C TraceContext
// propagator so that Inject/Extract calls are safe.
func InitTracerProvider(ctx context.Context, cfg Config) (func(context.Context) error, error) {
	// Set default sampling ratio.
	if cfg.SamplingRatio <= 0 {
		cfg.SamplingRatio = 1.0
	}

	// Create the span exporter according to the tracing mode.
	var exporter sdktrace.SpanExporter
	// fileWriter holds the *os.File when mode is "file"; it is closed
	// by the returned shutdown function.
	var fileWriter *os.File
	var err error
	switch cfg.Mode {
	case TracingModeStd:
		// Export spans to standard output for local debugging.
		exporter, err = stdouttrace.New(stdouttrace.WithPrettyPrint())
		if err != nil {
			return nil, fmt.Errorf("failed to create stdout exporter: %w", err)
		}
	case TracingModeFile:
		// Export spans to a file for persistent local debugging.
		if cfg.FilePath == "" {
			return nil, fmt.Errorf("tracing file path is required for file mode")
		}
		fileWriter, err = os.OpenFile(cfg.FilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return nil, fmt.Errorf("failed to open tracing file %q: %w", cfg.FilePath, err)
		}
		exporter, err = stdouttrace.New(stdouttrace.WithWriter(fileWriter), stdouttrace.WithPrettyPrint())
		if err != nil {
			fileWriter.Close()
			return nil, fmt.Errorf("failed to create file exporter: %w", err)
		}
	case TracingModeOTel:
		opts := []otlptracegrpc.Option{
			otlptracegrpc.WithEndpoint(cfg.Endpoint),
		}
		if cfg.Insecure {
			opts = append(opts, otlptracegrpc.WithTLSCredentials(insecure.NewCredentials()))
		} else {
			opts = append(opts, otlptracegrpc.WithTLSCredentials(credentials.NewTLS(nil)))
		}
		exporter, err = otlptracegrpc.New(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("failed to create OTLP gRPC exporter: %w", err)
		}
	default:
		// TracingModeNone or any unrecognized value: disable tracing.
		noopTP := noop.NewTracerProvider()
		otel.SetTracerProvider(noopTP)
		// Install W3C propagator for deterministic Inject/Extract behavior,
		// even though no spans will be recorded in this mode.
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		))
		return func(context.Context) error { return nil }, nil
	}

	// Create resource with service attributes. Code-provided values act as
	// defaults; the standard OTel env vars OTEL_SERVICE_NAME and
	// OTEL_RESOURCE_ATTRIBUTES (e.g. "service.version=1.2.3") set on the
	// Deployment take precedence because WithFromEnv is applied last.
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(defaultServiceVersion),
		),
		resource.WithFromEnv(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	// Create TracerProvider with a FilteringSpanProcessor wrapping the batch
	// processor. The filter drops Reconcile Spans marked no-op (no write
	// operation), keeping empty Reconcile iterations out of exported traces.
	// Use custom RequestIDGenerator so that TraceID equals the request ID,
	// enabling unified trace-log correlation.
	batcher := sdktrace.NewBatchSpanProcessor(exporter)
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(NewFilteringSpanProcessor(batcher)),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SamplingRatio))),
		sdktrace.WithIDGenerator(&RequestIDGenerator{}),
	)

	// Set global tracer provider and propagator.
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	// If file mode, wrap shutdown so the file is closed after flushing.
	if fileWriter != nil {
		return func(ctx context.Context) error {
			shutdownErr := tp.Shutdown(ctx)
			closeErr := fileWriter.Close()
			if shutdownErr != nil {
				return shutdownErr
			}
			return closeErr
		}, nil
	}

	return tp.Shutdown, nil
}

// Tracer returns a tracer for the specified instrumentation scope.
// Uses the global OTel TracerProvider, which is set by InitTracerProvider.
// If InitTracerProvider has not been called, the OTel SDK returns a no-op tracer.
func Tracer(name string) trace.Tracer {
	return otel.GetTracerProvider().Tracer(name)
}

// requestIDKey is the context key for storing the request ID.
// The custom IDGenerator reads it to produce a TraceID equal to the request ID,
// enabling unified trace-log correlation without manual span context construction.
type requestIDKey struct{}

// WithRequestID stores the request ID in the context so that the custom
// IDGenerator can use it as the TraceID when creating a new root span.
// The requestID must be a 32-char hex string (UUID without hyphens) to match
// the OTel TraceID format, ensuring requestID == traceID.
func WithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, requestID)
}

// RequestIDGenerator implements sdktrace.IDGenerator.
// When the context carries a request ID (via WithRequestID), it is converted
// to the TraceID. Otherwise, a random TraceID is generated as fallback.
type RequestIDGenerator struct{}

// NewIDs returns a new TraceID and SpanID.
// If the context contains a valid request ID (UUID), it is used as the TraceID.
func (g *RequestIDGenerator) NewIDs(ctx context.Context) (trace.TraceID, trace.SpanID) {
	var traceID trace.TraceID
	if requestID, ok := ctx.Value(requestIDKey{}).(string); ok {
		if len(requestID) == 32 {
			if bytes, err := hex.DecodeString(requestID); err == nil && len(bytes) == 16 {
				copy(traceID[:], bytes)
				return traceID, g.NewSpanID(ctx, traceID)
			}
		}
	}
	// Fallback: random trace ID
	_, _ = rand.Read(traceID[:])
	return traceID, g.NewSpanID(ctx, traceID)
}

// NewSpanID returns a new random SpanID.
func (g *RequestIDGenerator) NewSpanID(_ context.Context, _ trace.TraceID) trace.SpanID {
	var spanID trace.SpanID
	_, _ = rand.Read(spanID[:])
	return spanID
}
