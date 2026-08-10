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
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"sync/atomic"

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
	SamplingRatio float64 // 0.0 to 1.0; 0 disables sampling, 1 samples everything
	Insecure      bool    // Use insecure gRPC (dev environment)
}

// BindFlags registers the tracing command-line flags on fs with shared
// defaults and help text, storing parsed values into c. Both entrypoints
// bind flag.CommandLine (the sandbox-manager pulls it into pflag via
// AddGoFlagSet), so flag definitions cannot drift between binaries.
// ServiceName is not a flag; each entrypoint sets it before Init.
func (c *Config) BindFlags(fs *flag.FlagSet) {
	fs.StringVar((*string)(&c.Mode), "tracing-mode", string(TracingModeNone), "Tracing mode: otel, std, file, none")
	fs.StringVar(&c.Endpoint, "tracing-endpoint", DefaultEndpoint, "OTLP gRPC endpoint for tracing export")
	fs.Float64Var(&c.SamplingRatio, "tracing-sampling-ratio", 1.0, "Trace sampling ratio within [0, 1]; 0 samples nothing, 1 samples everything")
	fs.BoolVar(&c.Insecure, "tracing-insecure", false, "Use insecure gRPC for tracing export (dev environment)")
	fs.StringVar(&c.FilePath, "tracing-file", "", "Output file path for tracing export (file mode)")
}

// InitTracerProvider initializes the global TracerProvider and returns a shutdown function.
// Must be called once at startup, before any controller or HTTP server starts.
//
// When cfg.Mode is "none" (or empty, which is accepted as an alias so a
// zero-value Config keeps working), tracing is disabled: a no-op
// TracerProvider is explicitly installed. Any other unrecognized value is a
// configuration error — returning it instead of silently disabling tracing
// makes a typo in --tracing-mode fail fast at startup rather than being
// diagnosed operationally. The OpenTelemetry API is
// designed so that its default global TracerProvider is already a no-op
// (API/SDK separation — libraries can embed tracing calls without the
// application installing an SDK). We set it explicitly here to guarantee a
// clean, deterministic state regardless of any third-party library that might
// have registered a real provider, and to install the W3C TraceContext
// propagator so that Inject/Extract calls are safe.
func InitTracerProvider(ctx context.Context, cfg Config) (func(context.Context) error, error) {
	// Validate the sampling ratio. An explicit 0 means "sample nothing" and
	// must be honored rather than treated as "unset"; out-of-range values are
	// a configuration error instead of being silently clamped. The default of
	// 1.0 is supplied by BindFlags, not here.
	if cfg.SamplingRatio < 0 || cfg.SamplingRatio > 1 {
		return nil, fmt.Errorf("invalid tracing sampling ratio %g: must be within [0, 1]", cfg.SamplingRatio)
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
		// Export spans to a file for persistent local debugging. The output
		// is an appended stream of pretty-printed JSON objects (one per
		// exported span), not a single valid JSON document. Traces carry
		// identifying data (request IDs, sandbox names, namespaces), so the
		// file is created owner-only (0600).
		if cfg.FilePath == "" {
			return nil, fmt.Errorf("tracing file path is required for file mode")
		}
		fileWriter, err = os.OpenFile(cfg.FilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
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
	case TracingModeNone, "":
		// Tracing explicitly disabled (empty string is an alias for "none").
		enabledFlag.Store(false)
		noopTP := noop.NewTracerProvider()
		otel.SetTracerProvider(noopTP)
		// Install W3C propagator for deterministic Inject/Extract behavior,
		// even though no spans will be recorded in this mode.
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		))
		return func(context.Context) error { return nil }, nil
	default:
		// A typo in --tracing-mode must fail fast instead of silently
		// disabling tracing, which is difficult to diagnose operationally.
		return nil, fmt.Errorf("unrecognized tracing mode %q: valid values are otel, std, file, none", cfg.Mode)
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
	// enabling unified trace-log correlation. Sampling uses randomRatioSampler
	// rather than TraceIDRatioBased: since the TraceID is the caller-supplied
	// request ID, a TraceID-derived decision would let callers pick IDs that
	// are always sampled (see randomRatioSampler below).
	batcher := sdktrace.NewBatchSpanProcessor(exporter)
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(NewFilteringSpanProcessor(batcher)),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(randomRatioSampler{ratio: cfg.SamplingRatio})),
		sdktrace.WithIDGenerator(&RequestIDGenerator{}),
	)

	// Set global tracer provider and propagator.
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	enabledFlag.Store(true)

	// If file mode, wrap shutdown so the file is closed after flushing.
	if fileWriter != nil {
		return func(ctx context.Context) error {
			enabledFlag.Store(false)
			shutdownErr := tp.Shutdown(ctx)
			closeErr := fileWriter.Close()
			if shutdownErr != nil {
				return shutdownErr
			}
			return closeErr
		}, nil
	}

	return func(ctx context.Context) error {
		enabledFlag.Store(false)
		return tp.Shutdown(ctx)
	}, nil
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

// enabledFlag records whether a real (non-noop) TracerProvider was installed
// by InitTracerProvider. It gates request-ID validation at the web framework
// boundary: only when tracing is on must a caller-provided request ID be
// usable as an OTel TraceID.
var enabledFlag atomic.Bool

// Enabled returns true when tracing was initialized with a real exporter
// (mode "otel", "std" or "file"), and false for mode "none", before
// InitTracerProvider is called, or after shutdown.
func Enabled() bool {
	return enabledFlag.Load()
}

// NewRequestID returns a server-generated request ID directly in the
// representation required by the tracing scheme: 32 lowercase hex characters
// (16 random bytes), usable as an OTel TraceID as-is. Generating the required
// form up front means caller-visible request IDs never need rewriting.
func NewRequestID() string {
	b := make([]byte, 16)
	// crypto/rand.Read does not fail on supported platforms; the OTel SDK
	// relies on the same source for its random IDs.
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// IsValidRequestID reports whether id can serve as an OTel TraceID:
// exactly 32 hex characters and not all-zero (an all-zero trace ID is
// invalid per the W3C Trace Context and OTel specifications). Both cases
// of hex digits are accepted, and an accepted ID is used verbatim: the API
// layer never rewrites a caller-provided request ID, because callers grep
// logs for the exact value they sent. Uppercase hex decodes to the same
// TraceID bytes; only the canonical string form in trace backends is
// lowercase.
func IsValidRequestID(id string) bool {
	if len(id) != 32 {
		return false
	}
	b, err := hex.DecodeString(id)
	if err != nil {
		return false
	}
	for _, x := range b {
		if x != 0 {
			return true
		}
	}
	return false
}

// randomRatioSampler samples root spans with the configured probability using
// a server-controlled random draw instead of the TraceID.
//
// With the custom RequestIDGenerator, the TraceID equals the caller-provided
// request ID. The standard TraceIDRatioBased sampler derives its decision
// deterministically from the TraceID, so a caller could choose request IDs
// that always (or never) fall inside the sampled range, defeating the
// configured rate as a capacity/cost limit and making sampling
// caller-controllable. Drawing from crypto/rand keeps the decision
// unpredictable to callers; the cost of one 8-byte read per root span is
// negligible next to span creation itself.
//
// ratio semantics: >= 1 always samples, 0 never samples (values outside
// [0, 1] are rejected by InitTracerProvider before this sampler is built).
type randomRatioSampler struct {
	ratio float64
}

// ShouldSample implements sdktrace.Sampler.
func (s randomRatioSampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	psc := trace.SpanContextFromContext(p.ParentContext)
	decision := sdktrace.Drop
	if s.ratio >= 1 || randomFloat64() < s.ratio {
		decision = sdktrace.RecordAndSample
	}
	return sdktrace.SamplingResult{
		Decision:   decision,
		Tracestate: psc.TraceState(),
	}
}

// randomFloat64 returns a uniform float64 in [0, 1) drawn from crypto/rand,
// using the top 53 bits for full mantissa precision.
func randomFloat64() float64 {
	var b [8]byte
	// crypto/rand.Read does not fail on supported platforms.
	_, _ = rand.Read(b[:])
	return float64(binary.BigEndian.Uint64(b[:])>>11) / (1 << 53)
}

// Description implements sdktrace.Sampler.
func (s randomRatioSampler) Description() string {
	return fmt.Sprintf("RandomRatioBased{%g}", s.ratio)
}
