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
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// initTestTracer sets up a real TracerProvider with AlwaysSample for testing.
// Returns a cleanup function that restores the previous tracer provider.
func initTestTracer() func() {
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	}
}

// initNoopTracer sets up a no-op tracer for testing.
func initNoopTracer() func() {
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	noopTP := trace.NewNoopTracerProvider()
	otel.SetTracerProvider(noopTP)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	}
}

func TestInjectTraceContext_NilAnnotations(t *testing.T) {
	tests := []struct {
		name    string
		ctx     context.Context
		hasSpan bool
		wantNil bool
	}{
		{
			name:    "nil annotations with no active span stays nil",
			ctx:     context.Background(),
			hasSpan: false,
			wantNil: true,
		},
		{
			name:    "nil annotations with active span allocates and injects",
			ctx:     context.Background(),
			hasSpan: true,
			wantNil: false,
		},
		{
			name:    "baggage without an active span does not allocate annotations",
			ctx:     WithTraceOperation(context.Background(), "resume"),
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.hasSpan {
				cleanup := initTestTracer()
				defer cleanup()
				tracer := Tracer("test")
				tt.ctx, _ = tracer.Start(tt.ctx, "test-span")
			} else {
				cleanup := initNoopTracer()
				defer cleanup()
			}

			result := InjectTraceContext(tt.ctx, nil)
			if tt.wantNil {
				// Nothing to inject: the input must be returned untouched so
				// callers can detect "nothing injected" and skip useless writes.
				assert.Nil(t, result, "nil input should stay nil when nothing is injected")
			} else {
				assert.NotNil(t, result, "should allocate a map when injecting")
				assert.NotEmpty(t, result[TraceContextAnnotationKey], "trace-context annotation should be injected")
			}
		})
	}
}

func TestInjectTraceContext_WithExistingAnnotations(t *testing.T) {
	const staleBaggage = "operation=pause,tenant=old"
	staleAnnotations := map[string]string{
		"existing":                "value",
		TraceContextAnnotationKey: "00-11111111111111111111111111111111-2222222222222222-01",
		TraceBaggageAnnotationKey: staleBaggage,
	}
	otherBaggage, err := baggage.Parse("tenant=current")
	require.NoError(t, err)

	tests := []struct {
		name        string
		ctx         context.Context
		annotations map[string]string
		hasSpan     bool
		wantBaggage string
	}{
		{
			name:        "existing annotations with no active span",
			ctx:         context.Background(),
			annotations: map[string]string{"existing": "value"},
			hasSpan:     false,
		},
		{
			name:        "existing annotations with active span",
			ctx:         context.Background(),
			annotations: map[string]string{"existing": "value"},
			hasSpan:     true,
		},
		{
			name:        "empty baggage clears the previous operation",
			ctx:         context.Background(),
			annotations: staleAnnotations,
			hasSpan:     true,
		},
		{
			name:        "current operation replaces stale baggage",
			ctx:         WithTraceOperation(context.Background(), "resume"),
			annotations: staleAnnotations,
			hasSpan:     true,
			wantBaggage: "operation=resume",
		},
		{
			name:        "other baggage is preserved without an operation",
			ctx:         baggage.ContextWithBaggage(context.Background(), otherBaggage),
			annotations: staleAnnotations,
			hasSpan:     true,
			wantBaggage: "tenant=current",
		},
		{
			name:        "no active span leaves stale annotations untouched",
			ctx:         context.Background(),
			annotations: staleAnnotations,
			wantBaggage: staleBaggage,
		},
		{
			name:        "baggage alone leaves stale annotations untouched",
			ctx:         WithTraceOperation(context.Background(), "resume"),
			annotations: staleAnnotations,
			wantBaggage: staleBaggage,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.hasSpan {
				cleanup := initTestTracer()
				defer cleanup()
				tracer := Tracer("test")
				var span trace.Span
				tt.ctx, span = tracer.Start(tt.ctx, "test-span")
				defer span.End()
			} else {
				cleanup := initNoopTracer()
				defer cleanup()
			}

			// Keep input and expected maps independent so in-place mutations
			// cannot hide stale annotations.
			annotations := maps.Clone(tt.annotations)
			want := maps.Clone(tt.annotations)
			if tt.hasSpan {
				sc := trace.SpanContextFromContext(tt.ctx)
				want[TraceContextAnnotationKey] = "00-" + sc.TraceID().String() + "-" + sc.SpanID().String() + "-" + sc.TraceFlags().String()
			}
			if tt.wantBaggage == "" {
				delete(want, TraceBaggageAnnotationKey)
			} else {
				want[TraceBaggageAnnotationKey] = tt.wantBaggage
			}

			result := InjectTraceContext(tt.ctx, annotations)
			assert.Equal(t, want, result)
			assert.Equal(t, want, annotations, "existing annotations must be updated in place")
			if tt.hasSpan {
				extracted := ExtractTraceContext(context.Background(), result)
				assert.Equal(t, TraceOperationFromContext(tt.ctx), TraceOperationFromContext(extracted))
				assert.Equal(t, baggage.FromContext(tt.ctx).String(), baggage.FromContext(extracted).String())
			}
			if tt.hasSpan && tt.wantBaggage != "" {
				// Reusing the same map without baggage must clear the previous injection.
				ctx := baggage.ContextWithBaggage(tt.ctx, baggage.Baggage{})
				delete(want, TraceBaggageAnnotationKey)
				assert.Equal(t, want, InjectTraceContext(ctx, result))
				assert.Equal(t, want, annotations)
			}
		})
	}
}

func TestExtractTraceContext_NilAnnotations(t *testing.T) {
	cleanup := initNoopTracer()
	defer cleanup()

	ctx := context.Background()
	result := ExtractTraceContext(ctx, nil)
	assert.Equal(t, ctx, result, "should return original context when annotations is nil")
}

func TestExtractTraceContext_EmptyAnnotations(t *testing.T) {
	cleanup := initNoopTracer()
	defer cleanup()

	ctx := context.Background()
	result := ExtractTraceContext(ctx, map[string]string{})
	assert.NotNil(t, result, "should return non-nil context")
}

func TestInjectExtract_RoundTrip(t *testing.T) {
	cleanup := initTestTracer()
	defer cleanup()

	// Create a parent span and inject trace context.
	tracer := Tracer("test")
	ctx, span := tracer.Start(context.Background(), "parent-span")

	annotations := map[string]string{}
	annotations = InjectTraceContext(ctx, annotations)

	// Verify trace-context annotation was injected (W3C "traceparent" key is mapped to TraceContextAnnotationKey).
	traceparent, ok := annotations[TraceContextAnnotationKey]
	assert.True(t, ok, "trace-context annotation should be injected")
	assert.NotEmpty(t, traceparent, "trace-context annotation should not be empty")

	// Extract trace context from annotations into a fresh context.
	extractedCtx := ExtractTraceContext(context.Background(), annotations)

	// Create a child span from extracted context.
	childCtx, childSpan := tracer.Start(extractedCtx, "child-span")
	childSpan.End()
	span.End()

	// Verify parent-child relationship via SpanContext.
	parentSpanID := span.SpanContext().SpanID()
	childParentSpanID := trace.SpanFromContext(childCtx).SpanContext().SpanID()
	assert.NotEqual(t, parentSpanID, childParentSpanID,
		"child SpanID should differ from parent SpanID")
	assert.True(t, trace.SpanFromContext(childCtx).SpanContext().IsValid(),
		"child span context should be valid")
}

func TestInjectTraceContext_NoActiveSpanWithNoopTracer(t *testing.T) {
	cleanup := initNoopTracer()
	defer cleanup()

	annotations := map[string]string{"foo": "bar"}
	result := InjectTraceContext(context.Background(), annotations)

	// With no active span, no trace-context should be injected.
	_, hasTrace := result[TraceContextAnnotationKey]
	assert.False(t, hasTrace, "should not inject trace-context when no active span")
	assert.Equal(t, "bar", result["foo"], "existing annotations should be preserved")
}

func TestWithRootSpanContext_InjectUsesRootSpanID(t *testing.T) {
	tests := []struct {
		name             string
		useRootSpanCtx   bool
		operation        string
		clearCurrentSpan bool
		unsampled        bool
	}{
		{
			name:           "WithRootSpanContext: injected SpanID is root span's SpanID",
			useRootSpanCtx: true,
		},
		{
			name:           "without WithRootSpanContext: injected SpanID is child span's SpanID",
			useRootSpanCtx: false,
		},
		{
			name:           "operation added after saving root context is injected",
			useRootSpanCtx: true,
			operation:      "resume",
		},
		{
			name:             "stored root clears stale baggage without a valid current span",
			useRootSpanCtx:   true,
			clearCurrentSpan: true,
		},
		{
			name:           "valid unsampled root clears stale baggage",
			useRootSpanCtx: true,
			unsampled:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanup := initTestTracer()
			defer cleanup()

			tracer := Tracer("test")

			// Create root span (simulates HTTP middleware root span).
			ctx, rootSpan := tracer.Start(context.Background(), "root-span")
			rootSpanCtx := rootSpan.SpanContext()
			if tt.unsampled {
				rootSpanCtx = rootSpanCtx.WithTraceFlags(0)
				ctx = trace.ContextWithSpanContext(ctx, rootSpanCtx)
			}

			// Optionally capture root span context before creating child spans.
			if tt.useRootSpanCtx {
				ctx = WithRootSpanContext(ctx)
			}
			ctx = WithTraceOperation(ctx, tt.operation)

			// Create child span (simulates manager/infra span).
			childCtx, childSpan := tracer.Start(ctx, "child-span")
			if tt.clearCurrentSpan {
				childCtx = trace.ContextWithSpanContext(childCtx, trace.SpanContext{})
			}

			// Inject trace context from child ctx.
			annotations := InjectTraceContext(childCtx, map[string]string{
				TraceBaggageAnnotationKey: "operation=pause",
			})

			// Verify trace-context annotation was injected.
			traceparent, ok := annotations[TraceContextAnnotationKey]
			assert.True(t, ok, "trace-context annotation should be injected")
			assert.NotEmpty(t, traceparent, "traceparent should not be empty")

			// Extract context from annotations (simulates controller Reconcile).
			extractedCtx := ExtractTraceContext(context.Background(), annotations)
			extractedSpanCtx := trace.SpanContextFromContext(extractedCtx)
			extractedSpanID := extractedSpanCtx.SpanID()
			assert.Equal(t, rootSpanCtx.TraceID(), extractedSpanCtx.TraceID())
			assert.Equal(t, tt.operation, TraceOperationFromContext(extractedCtx))
			if tt.operation == "" {
				assert.NotContains(t, annotations, TraceBaggageAnnotationKey)
			} else {
				assert.Equal(t, "operation="+tt.operation, annotations[TraceBaggageAnnotationKey])
			}

			if tt.useRootSpanCtx {
				assert.Equal(t, rootSpanCtx.TraceFlags(), extractedSpanCtx.TraceFlags())
				assert.Equal(t, rootSpan.SpanContext().SpanID(), extractedSpanID,
					"extracted SpanID should be root span's SpanID")
				assert.NotEqual(t, childSpan.SpanContext().SpanID(), extractedSpanID,
					"extracted SpanID should NOT be child span's SpanID")
			} else {
				assert.Equal(t, childSpan.SpanContext().SpanID(), extractedSpanID,
					"extracted SpanID should be child span's SpanID")
			}

			childSpan.End()
			rootSpan.End()
		})
	}
}

func TestWithRootSpanContext_NoActiveSpan(t *testing.T) {
	cleanup := initNoopTracer()
	defer cleanup()

	// With no active span, WithRootSpanContext should return ctx unchanged.
	ctx := context.Background()
	result := WithRootSpanContext(ctx)
	assert.Equal(t, ctx, result, "should return original ctx when no active span")
}

func TestExtractTraceContext_InvalidTraceparent(t *testing.T) {
	cleanup := initNoopTracer()
	defer cleanup()

	tests := []struct {
		name        string
		annotations map[string]string
	}{
		{
			name:        "invalid traceparent format",
			annotations: map[string]string{TraceContextAnnotationKey: "invalid-value"},
		},
		{
			name:        "empty traceparent",
			annotations: map[string]string{TraceContextAnnotationKey: ""},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			result := ExtractTraceContext(ctx, tt.annotations)
			// Should not panic, returns context (possibly unchanged).
			assert.NotNil(t, result, "should return non-nil context")
		})
	}
}

func TestAnnotationCarrier_GetSet(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		getKey      string
		wantGet     string
		setKey      string
		setValue    string
		wantSetKey  string // the key the value should be stored under after Set
	}{
		{
			name:        "Get traceparent returns TraceContextAnnotationKey value",
			annotations: map[string]string{TraceContextAnnotationKey: "00-trace-id-span-id-01"},
			getKey:      "traceparent",
			wantGet:     "00-trace-id-span-id-01",
		},
		{
			name:        "Get non-traceparent key returns direct value",
			annotations: map[string]string{"foo": "bar"},
			getKey:      "foo",
			wantGet:     "bar",
		},
		{
			name:        "Get missing key returns empty string",
			annotations: map[string]string{"foo": "bar"},
			getKey:      "nonexistent",
			wantGet:     "",
		},
		{
			name:        "Get from nil annotations returns empty string",
			annotations: nil,
			getKey:      "traceparent",
			wantGet:     "",
		},
		{
			name:        "Set traceparent stores under TraceContextAnnotationKey",
			annotations: map[string]string{},
			setKey:      "traceparent",
			setValue:    "00-trace-id-span-id-01",
			wantSetKey:  TraceContextAnnotationKey,
		},
		{
			name:        "Set non-traceparent stores under the same key",
			annotations: map[string]string{},
			setKey:      "custom-key",
			setValue:    "custom-value",
			wantSetKey:  "custom-key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &annotationCarrier{annotations: tt.annotations}

			if tt.getKey != "" {
				got := c.Get(tt.getKey)
				assert.Equal(t, tt.wantGet, got)
			}

			if tt.setKey != "" {
				c.Set(tt.setKey, tt.setValue)
				assert.Equal(t, tt.setValue, c.annotations[tt.wantSetKey])
			}
		})
	}
}

func TestAnnotationCarrier_Keys(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		wantLen     int
	}{
		{
			name:        "empty annotations returns empty slice",
			annotations: map[string]string{},
			wantLen:     0,
		},
		{
			name:        "nil annotations returns empty slice",
			annotations: nil,
			wantLen:     0,
		},
		{
			name:        "single annotation returns one key",
			annotations: map[string]string{TraceContextAnnotationKey: "value"},
			wantLen:     1,
		},
		{
			name:        "multiple annotations returns all keys",
			annotations: map[string]string{TraceContextAnnotationKey: "value1", "other-key": "value2"},
			wantLen:     2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &annotationCarrier{annotations: tt.annotations}
			keys := c.Keys()
			assert.NotNil(t, keys, "Keys should never return nil")
			assert.Equal(t, tt.wantLen, len(keys), "key count should match")
		})
	}
}
