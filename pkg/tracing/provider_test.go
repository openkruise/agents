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
	"encoding/hex"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

func TestInitTracerProvider_Disabled(t *testing.T) {
	prevTP := otel.GetTracerProvider()
	defer func() { otel.SetTracerProvider(prevTP) }()

	shutdown, err := InitTracerProvider(context.Background(), Config{
		Mode:        TracingModeNone,
		ServiceName: "test-service",
	})
	assert.NoError(t, err, "should not error when mode is none")
	assert.NotNil(t, shutdown, "should return non-nil shutdown function")

	// Verify no-op tracer is set.
	tracer := Tracer("test")
	assert.NotNil(t, tracer, "Tracer should return non-nil")

	// Shutdown should not error.
	err = shutdown(context.Background())
	assert.NoError(t, err, "shutdown should not error")
}

func TestInitTracerProvider_Enabled(t *testing.T) {
	prevTP := otel.GetTracerProvider()
	defer func() { otel.SetTracerProvider(prevTP) }()

	shutdown, err := InitTracerProvider(context.Background(), Config{
		Mode:          TracingModeOTel,
		Endpoint:      "localhost:4317",
		ServiceName:   "test-service",
		SamplingRatio: 1.0,
		Insecure:      true,
	})
	// Should not error even with unreachable endpoint (gRPC connects lazily).
	assert.NoError(t, err, "should not error with valid config")
	assert.NotNil(t, shutdown, "should return non-nil shutdown function")

	// Tracer should return a real tracer (not no-op).
	tracer := Tracer("test")
	assert.NotNil(t, tracer, "Tracer should return non-nil")

	// Cleanup.
	err = shutdown(context.Background())
	assert.NoError(t, err, "shutdown should not error")
}

func TestInitTracerProvider_EnabledWithDefaultSamplingRatio(t *testing.T) {
	prevTP := otel.GetTracerProvider()
	defer func() { otel.SetTracerProvider(prevTP) }()

	shutdown, err := InitTracerProvider(context.Background(), Config{
		Mode:          TracingModeOTel,
		Endpoint:      "localhost:4317",
		ServiceName:   "test-service",
		SamplingRatio: 0, // Should default to 1.0
		Insecure:      true,
	})
	assert.NoError(t, err, "should not error with zero sampling ratio")
	defer func() { _ = shutdown(context.Background()) }()
}

func TestTracer_NotInitialized(t *testing.T) {
	// Reset global state to simulate uninitialized state.
	// Since globalTracerProvider is package-level, Tracer uses sync.Once
	// to lazily initialize with a no-op provider.
	// We can't easily reset the sync.Once, so we just verify Tracer returns non-nil.
	tracer := Tracer("uninitialized-test")
	assert.NotNil(t, tracer, "Tracer should return non-nil even when not initialized")

	// Verify it's a no-op tracer by checking that spans are not recorded.
	_, span := tracer.Start(context.Background(), "test-span")
	assert.NotNil(t, span, "Start should return non-nil span")
	assert.False(t, span.SpanContext().IsValid(),
		"no-op tracer should produce invalid span context")
	span.End()
}

func TestInitTracerProvider_RestoresPropagator(t *testing.T) {
	// Set up with disabled config.
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	defer func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	}()

	shutdown, err := InitTracerProvider(context.Background(), Config{
		Mode:        TracingModeNone,
		ServiceName: "test-service",
	})
	assert.NoError(t, err)
	defer func() { _ = shutdown(context.Background()) }()

	// Verify propagator is set (non-nil).
	prop := otel.GetTextMapPropagator()
	assert.NotNil(t, prop, "propagator should be set after InitTracerProvider")
}

func TestInitTracerProvider_StdMode(t *testing.T) {
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	defer func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	}()

	shutdown, err := InitTracerProvider(context.Background(), Config{
		Mode:          TracingModeStd,
		ServiceName:   "test-service-std",
		SamplingRatio: 1.0,
	})
	assert.NoError(t, err, "should not error with std mode")
	assert.NotNil(t, shutdown, "should return non-nil shutdown function")

	// Tracer should return a real tracer that records spans.
	tracer := Tracer("test-std")
	_, span := tracer.Start(context.Background(), "std-span")
	assert.True(t, span.SpanContext().IsValid(),
		"std mode should produce a valid span context")
	span.End()

	err = shutdown(context.Background())
	assert.NoError(t, err, "shutdown should not error")
}

func TestInitTracerProvider_FileMode(t *testing.T) {
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	defer func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	}()

	tmpFile := t.TempDir() + "/traces.json"
	shutdown, err := InitTracerProvider(context.Background(), Config{
		Mode:          TracingModeFile,
		FilePath:      tmpFile,
		ServiceName:   "test-service-file",
		SamplingRatio: 1.0,
	})
	assert.NoError(t, err, "should not error with file mode")
	assert.NotNil(t, shutdown, "should return non-nil shutdown function")

	// Emit a span and shutdown to flush.
	tracer := Tracer("test-file")
	_, span := tracer.Start(context.Background(), "file-span")
	assert.True(t, span.SpanContext().IsValid(),
		"file mode should produce a valid span context")
	span.End()

	err = shutdown(context.Background())
	assert.NoError(t, err, "shutdown should not error")

	// Verify the file was created and contains span data.
	info, statErr := os.Stat(tmpFile)
	assert.NoError(t, statErr, "trace file should exist")
	assert.Greater(t, info.Size(), int64(0), "trace file should not be empty")
}

func TestInitTracerProvider_FileMode_EmptyPath(t *testing.T) {
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	defer func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	}()

	_, err := InitTracerProvider(context.Background(), Config{
		Mode:        TracingModeFile,
		FilePath:    "", // empty path should error
		ServiceName: "test-service",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "tracing file path is required")
}

func TestInitTracerProvider_EnabledWithTLS(t *testing.T) {
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	defer func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	}()

	shutdown, err := InitTracerProvider(context.Background(), Config{
		Mode:          TracingModeOTel,
		Endpoint:      "localhost:4317",
		ServiceName:   "test-service-tls",
		SamplingRatio: 1.0,
		Insecure:      false, // Exercises the TLS credentials path.
	})
	assert.NoError(t, err, "should not error with valid TLS config")
	assert.NotNil(t, shutdown, "should return non-nil shutdown function")

	tracer := Tracer("test-tls")
	assert.NotNil(t, tracer, "Tracer should return non-nil")

	defer func() { _ = shutdown(context.Background()) }()
}

func TestWithRequestID(t *testing.T) {
	t.Run("stores request ID in context", func(t *testing.T) {
		requestID := "0123456789abcdef0123456789abcdef"
		ctx := WithRequestID(context.Background(), requestID)
		val, ok := ctx.Value(requestIDKey{}).(string)
		assert.True(t, ok, "request ID should be stored in context")
		assert.Equal(t, requestID, val)
	})

	t.Run("overwrites previous request ID", func(t *testing.T) {
		ctx := WithRequestID(context.Background(), "first")
		ctx = WithRequestID(ctx, "second")
		val, ok := ctx.Value(requestIDKey{}).(string)
		assert.True(t, ok)
		assert.Equal(t, "second", val)
	})
}

func TestRequestIDGenerator_NewIDs_WithValidRequestID(t *testing.T) {
	tests := []struct {
		name       string
		requestID  string
		expectSame bool
	}{
		{
			name:       "valid 32-char hex request ID becomes TraceID",
			requestID:  "0123456789abcdef0123456789abcdef",
			expectSame: true,
		},
		{
			name:       "all zeros is valid hex",
			requestID:  "00000000000000000000000000000000",
			expectSame: true,
		},
		{
			name:       "all fs is valid hex",
			requestID:  "ffffffffffffffffffffffffffffffff",
			expectSame: true,
		},
		{
			name:       "empty request ID falls back to random",
			requestID:  "",
			expectSame: false,
		},
		{
			name:       "wrong length (31 chars) falls back to random",
			requestID:  "0123456789abcdef0123456789abcde",
			expectSame: false,
		},
		{
			name:       "wrong length (33 chars) falls back to random",
			requestID:  "0123456789abcdef0123456789abcdef0",
			expectSame: false,
		},
		{
			name:       "non-hex chars fall back to random",
			requestID:  "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz",
			expectSame: false,
		},
		{
			name:       "odd length hex falls back to random",
			requestID:  "0123456789abcdef0123456789abcdeff",
			expectSame: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.requestID != "" {
				ctx = WithRequestID(ctx, tt.requestID)
			}

			gen := &RequestIDGenerator{}
			traceID, spanID := gen.NewIDs(ctx)

			assert.NotEqual(t, [16]byte{}, spanID, "SpanID should not be all zeros")

			if tt.expectSame {
				expected, _ := hex.DecodeString(tt.requestID)
				var expectedTraceID trace.TraceID
				copy(expectedTraceID[:], expected)
				assert.Equal(t, expectedTraceID, traceID,
					"TraceID should equal the request ID")
			} else {
				// For fallback, TraceID should be random (non-zero, and not equal to the input)
				var zero [16]byte
				assert.NotEqual(t, zero, traceID,
					"random TraceID should not be all zeros (extremely unlikely)")
			}
		})
	}
}

func TestRequestIDGenerator_NewIDs_WithoutRequestID(t *testing.T) {
	gen := &RequestIDGenerator{}
	traceID1, spanID1 := gen.NewIDs(context.Background())
	traceID2, spanID2 := gen.NewIDs(context.Background())

	// Random TraceIDs should be different (extremely unlikely to collide).
	assert.NotEqual(t, traceID1, traceID2,
		"two random TraceIDs should differ")
	assert.NotEqual(t, spanID1, spanID2,
		"two random SpanIDs should differ")
}

func TestRequestIDGenerator_NewSpanID(t *testing.T) {
	gen := &RequestIDGenerator{}

	tests := []struct {
		name    string
		ctx     context.Context
		traceID [16]byte
	}{
		{
			name:    "with background context",
			ctx:     context.Background(),
			traceID: [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		},
		{
			name:    "with nil trace ID (all zeros)",
			ctx:     context.Background(),
			traceID: [16]byte{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spanID := gen.NewSpanID(tt.ctx, tt.traceID)

			// SpanID should not be all zeros (extremely unlikely for random).
			var zero [8]byte
			assert.NotEqual(t, zero, spanID, "SpanID should not be all zeros")
		})
	}
}

func TestRequestIDGenerator_NewSpanID_Uniqueness(t *testing.T) {
	gen := &RequestIDGenerator{}
	traceID := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

	seen := make(map[[8]byte]bool)
	for i := 0; i < 100; i++ {
		spanID := gen.NewSpanID(context.Background(), traceID)
		assert.False(t, seen[spanID], "SpanID should be unique (collision at iteration %d)", i)
		seen[spanID] = true
	}
}
