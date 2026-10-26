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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func TestInitTracerProvider_UnrecognizedMode_ReturnsError(t *testing.T) {
	tests := []struct {
		name string
		mode TracingMode
	}{
		{name: "typo of otel", mode: "otle"},
		{name: "unknown value", mode: "jaeger"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shutdown, err := InitTracerProvider(context.Background(), Config{
				Mode:        tt.mode,
				ServiceName: "test-service",
			})
			require.Error(t, err, "unrecognized mode must fail fast instead of silently disabling tracing")
			assert.Contains(t, err.Error(), "unrecognized tracing mode")
			assert.Nil(t, shutdown)
		})
	}
}

func TestInitTracerProvider_Disabled(t *testing.T) {
	// 即使显式指定的 Secret 无法读取，none 模式也不初始化认证。
	t.Setenv(otelTokenSecretNameEnv, "not-used")
	previousConfig := secretLookupConfig
	SetSecretLookupConfig(&rest.Config{Host: "://invalid"})
	t.Cleanup(func() { SetSecretLookupConfig(previousConfig) })
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
	// 此用例验证 exporter 初始化，不读取外部 Kubernetes 凭证。
	t.Setenv(otelExporterOTLPHeadersEnv, "test-header=test")
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

func TestInitTracerProvider_SamplingRatioContract(t *testing.T) {
	t.Setenv(otelExporterOTLPHeadersEnv, "test-header=test")
	tests := []struct {
		name        string
		ratio       float64
		expectError string
	}{
		{name: "zero is valid and means sample nothing", ratio: 0, expectError: ""},
		{name: "one is valid and means sample everything", ratio: 1, expectError: ""},
		{name: "negative is a configuration error", ratio: -0.1, expectError: "invalid tracing sampling ratio"},
		{name: "greater than one is a configuration error", ratio: 1.5, expectError: "invalid tracing sampling ratio"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prevTP := otel.GetTracerProvider()
			defer func() { otel.SetTracerProvider(prevTP) }()

			shutdown, err := InitTracerProvider(context.Background(), Config{
				Mode:          TracingModeOTel,
				Endpoint:      "localhost:4317",
				ServiceName:   "test-service",
				SamplingRatio: tt.ratio,
				Insecure:      true,
			})
			if tt.expectError != "" {
				require.Error(t, err, "out-of-range ratio must fail fast")
				assert.Contains(t, err.Error(), tt.expectError)
				assert.Nil(t, shutdown)
				return
			}
			require.NoError(t, err, "in-range ratio must be accepted as-is")
			defer func() { _ = shutdown(context.Background()) }()
		})
	}
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

	// Verify the file was created and contains span data. The file holds
	// identifying data (request IDs, sandbox names), so it must be owner-only.
	info, statErr := os.Stat(tmpFile)
	assert.NoError(t, statErr, "trace file should exist")
	assert.Greater(t, info.Size(), int64(0), "trace file should not be empty")
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm(),
		"trace file should be created with owner-only permissions")
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
	t.Setenv(otelExporterOTLPHeadersEnv, "test-header=test")
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

// sampleWith runs ShouldSample n times against fresh root parameters carrying
// the given TraceID and returns how many decisions were RecordAndSample.
func sampleWith(s sdktrace.Sampler, traceID trace.TraceID, n int) int {
	sampled := 0
	for i := 0; i < n; i++ {
		res := s.ShouldSample(sdktrace.SamplingParameters{
			ParentContext: context.Background(),
			TraceID:       traceID,
			Name:          "op",
		})
		if res.Decision == sdktrace.RecordAndSample {
			sampled++
		}
	}
	return sampled
}

func TestRandomRatioSampler_Boundaries(t *testing.T) {
	traceID := trace.TraceID{0x01}

	tests := []struct {
		name        string
		ratio       float64
		n           int
		wantSampled int
	}{
		{name: "ratio 0 never samples", ratio: 0, n: 1000, wantSampled: 0},
		{name: "ratio 1 always samples", ratio: 1, n: 1000, wantSampled: 1000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sampleWith(randomRatioSampler{ratio: tt.ratio}, traceID, tt.n)
			assert.Equal(t, tt.wantSampled, got)
		})
	}
}

// TestRandomRatioSampler_NotTraceIDDeterministic verifies the decision is not
// a deterministic function of the TraceID: with the request-ID-as-TraceID
// scheme, a caller must not be able to pick an ID that is always sampled.
// With ratio 0.5 and 400 draws on a FIXED TraceID, a TraceID-deterministic
// sampler would return exactly 0 or 400; the random sampler lands in between
// (the probability of hitting either extreme is 2^-400).
func TestRandomRatioSampler_NotTraceIDDeterministic(t *testing.T) {
	fixedTraceID := trace.TraceID{0xde, 0xad, 0xbe, 0xef}
	const n = 400

	got := sampleWith(randomRatioSampler{ratio: 0.5}, fixedTraceID, n)
	assert.Greater(t, got, 0, "decision must not be deterministically negative for a fixed TraceID")
	assert.Less(t, got, n, "decision must not be deterministically positive for a fixed TraceID")
}

// TestDefaultOTLPHeaders 验证配置优先级、可选认证和通过真实客户端读取 Secret。
func TestDefaultOTLPHeaders(t *testing.T) {
	tests := []struct {
		name         string
		env          map[string]string
		data         map[string][]byte
		status       int
		noConfig     bool
		cancel       bool
		callInit     bool
		want         map[string]string
		wantError    string
		wantRequests int32
	}{
		{
			name: "standard OTLP headers bypass token and Secret",
			env: map[string]string{
				otelExporterOTLPHeadersEnv: "Authentication=explicit",
				otelTokenEnv:               "env-token",
				otelTokenSecretNameEnv:     "missing",
			},
			status: http.StatusForbidden,
		},
		{
			name: "trace-specific OTLP headers bypass Secret",
			env: map[string]string{
				otelExporterOTLPTracesHeadersEnv: "Authorization=explicit",
				otelTokenSecretNameEnv:           "missing",
			},
			status: http.StatusForbidden,
		},
		{
			name:   "env token bypasses Secret without endpoint restrictions",
			env:    map[string]string{otelTokenEnv: "env-token"},
			status: http.StatusForbidden,
			want:   map[string]string{"Authentication": "env-token"},
		},
		{
			name: "custom header with env token",
			env:  map[string]string{otelTokenEnv: "Bearer env-token", otelTokenHeaderEnv: "Authorization"},
			want: map[string]string{"Authorization": "Bearer env-token"},
		},
		{
			name:         "default Secret and key with trimmed token",
			data:         map[string][]byte{"OTEL_TOKEN": []byte("  test-token\n")},
			want:         map[string]string{"Authentication": "test-token"},
			wantRequests: 1,
		},
		{
			name: "namespace name key and header overrides",
			env: map[string]string{
				otelTokenSecretNamespaceEnv: "observability",
				otelTokenSecretNameEnv:      "collector-auth",
				otelTokenSecretKeyEnv:       "credential",
				otelTokenHeaderEnv:          "Authorization",
			},
			data:         map[string][]byte{"credential": []byte("Bearer test-token")},
			want:         map[string]string{"Authorization": "Bearer test-token"},
			wantRequests: 1,
		},
		{
			name:         "absent default Secret allows unauthenticated export",
			status:       http.StatusNotFound,
			wantRequests: 1,
		},
		{
			name:         "absent explicitly selected Secret fails",
			env:          map[string]string{otelTokenSecretNameEnv: "otel-token"},
			status:       http.StatusNotFound,
			wantError:    "get OTLP token Secret",
			wantRequests: 1,
		},
		{
			name:         "namespace override makes Secret required",
			env:          map[string]string{otelTokenSecretNamespaceEnv: "observability"},
			status:       http.StatusNotFound,
			wantError:    "get OTLP token Secret",
			wantRequests: 1,
		},
		{
			name:         "key override makes Secret required",
			env:          map[string]string{otelTokenSecretKeyEnv: "credential"},
			status:       http.StatusNotFound,
			wantError:    "get OTLP token Secret",
			wantRequests: 1,
		},
		{
			name:         "missing key in existing Secret fails",
			data:         map[string][]byte{"OTHER_KEY": []byte("test-token")},
			wantError:    "OTLP token Secret has a missing or empty token",
			wantRequests: 1,
		},
		{
			name:         "empty token in existing Secret fails",
			data:         map[string][]byte{"OTEL_TOKEN": []byte(" \n")},
			wantError:    "OTLP token Secret has a missing or empty token",
			wantRequests: 1,
		},
		{
			name: "initialization error omits Secret selectors for an empty token",
			env: map[string]string{
				otelTokenSecretNamespaceEnv: "private-namespace-marker",
				otelTokenSecretNameEnv:      "private-name-marker",
				otelTokenSecretKeyEnv:       "private-key-marker\nforged log entry",
			},
			data:         map[string][]byte{"OTHER_KEY": []byte("private-token-marker")},
			callInit:     true,
			wantError:    "OTLP token Secret has a missing or empty token",
			wantRequests: 1,
		},
		{
			name: "initialization error omits Secret selectors on lookup failure",
			env: map[string]string{
				otelTokenSecretNamespaceEnv: "private-namespace-marker",
				otelTokenSecretNameEnv:      "private-name-marker",
				otelTokenSecretKeyEnv:       "private-key-marker",
			},
			status:       http.StatusForbidden,
			callInit:     true,
			wantError:    "get OTLP token Secret: test API failure",
			wantRequests: 1,
		},
		{
			name:         "forbidden Secret read is not ignored",
			status:       http.StatusForbidden,
			wantError:    "get OTLP token Secret",
			wantRequests: 1,
		},
		{
			name:         "server error is not ignored",
			status:       http.StatusInternalServerError,
			wantError:    "get OTLP token Secret",
			wantRequests: 1,
		},
		{
			name:     "outside Kubernetes without explicit Secret allows no authentication",
			noConfig: true,
		},
		{
			name:      "outside Kubernetes with explicit Secret fails",
			noConfig:  true,
			env:       map[string]string{otelTokenSecretNameEnv: "collector-auth"},
			wantError: "load Kubernetes config for OTLP token Secret",
		},
		{
			name:      "canceled Secret read preserves cancellation",
			cancel:    true,
			wantError: "context canceled",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 清除环境输入，避免访问真实集群或继承外部认证配置。
			for _, key := range []string{
				otelExporterOTLPHeadersEnv, otelExporterOTLPTracesHeadersEnv,
				otelTokenEnv, otelTokenSecretNameEnv, otelTokenSecretKeyEnv,
				otelTokenSecretNamespaceEnv, otelTokenHeaderEnv,
				"KUBERNETES_SERVICE_HOST", "KUBERNETES_SERVICE_PORT",
			} {
				t.Setenv(key, "")
			}
			for key, value := range tt.env {
				t.Setenv(key, value)
			}

			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				namespace, name := "sandbox-system", "otel-token"
				if override := tt.env[otelTokenSecretNamespaceEnv]; override != "" {
					namespace = override
				}
				if override := tt.env[otelTokenSecretNameEnv]; override != "" {
					name = override
				}
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, "/api/v1/namespaces/"+namespace+"/secrets/"+name, r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				if tt.status != 0 {
					reason := metav1.StatusReasonInternalError
					switch tt.status {
					case http.StatusNotFound:
						reason = metav1.StatusReasonNotFound
					case http.StatusForbidden:
						reason = metav1.StatusReasonForbidden
					}
					w.WriteHeader(tt.status)
					assert.NoError(t, json.NewEncoder(w).Encode(metav1.Status{
						TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
						Status:   metav1.StatusFailure, Reason: reason, Code: int32(tt.status),
						Message: "test API failure",
					}))
					return
				}
				assert.NoError(t, json.NewEncoder(w).Encode(corev1.Secret{
					TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
					ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
					Data:       tt.data,
				}))
			}))
			t.Cleanup(server.Close)
			previousConfig := secretLookupConfig
			t.Cleanup(func() { SetSecretLookupConfig(previousConfig) })
			if tt.noConfig {
				SetSecretLookupConfig(nil)
			} else {
				SetSecretLookupConfig(&rest.Config{Host: server.URL})
			}

			ctx := context.Background()
			if tt.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			var got map[string]string
			var err error
			if tt.callInit {
				var shutdown func(context.Context) error
				shutdown, err = InitTracerProvider(ctx, Config{Mode: TracingModeOTel, Endpoint: "localhost:4317"})
				assert.Nil(t, shutdown)
			} else {
				got, err = defaultOTLPHeaders(ctx)
			}
			if tt.wantError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantError)
				if tt.callInit {
					for _, key := range []string{otelTokenSecretNamespaceEnv, otelTokenSecretNameEnv, otelTokenSecretKeyEnv} {
						assert.NotContains(t, err.Error(), tt.env[key])
					}
					assert.NotContains(t, err.Error(), "private-token-marker")
					assert.NotContains(t, err.Error(), "forged log entry")
				}
				if tt.status == http.StatusNotFound {
					assert.True(t, apierrors.IsNotFound(err))
				}
				if tt.status == http.StatusForbidden {
					assert.True(t, apierrors.IsForbidden(err))
				}
				if tt.cancel {
					assert.ErrorIs(t, err, context.Canceled)
				}
				if tt.noConfig {
					assert.ErrorIs(t, err, rest.ErrNotInCluster)
				}
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantRequests, requests.Load())
		})
	}
}
