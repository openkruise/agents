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

package web

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/openkruise/agents/pkg/tracing"
)

func TestHTTPEndpointMetrics(t *testing.T) {
	ok := func(code int, body string) Handler[string] {
		return func(r *http.Request) (ApiResponse[string], *ApiError) {
			return ApiResponse[string]{Code: code, Body: body}, nil
		}
	}
	tests := []struct {
		name        string
		endpoint    string
		handler     Handler[string]
		middleware  MiddleWare
		wantStatus  int
		wantHandled int
	}{
		{
			name:        "zero success code is recorded as 200 once",
			endpoint:    "create_sandbox",
			handler:     ok(0, "created"),
			wantStatus:  http.StatusOK,
			wantHandled: 1,
		},
		{
			name:        "explicit 201 is not collapsed",
			endpoint:    "connect_sandbox",
			handler:     ok(http.StatusCreated, "connected"),
			wantStatus:  http.StatusCreated,
			wantHandled: 1,
		},
		{
			name:        "204 is recorded and has an empty body",
			endpoint:    "delete_sandbox",
			handler:     ok(http.StatusNoContent, ""),
			wantStatus:  http.StatusNoContent,
			wantHandled: 1,
		},
		{
			name:     "zero error code is recorded as 500 once",
			endpoint: "list_sandboxes",
			handler: func(r *http.Request) (ApiResponse[string], *ApiError) {
				return ApiResponse[string]{}, &ApiError{Message: "failed"}
			},
			wantStatus:  http.StatusInternalServerError,
			wantHandled: 1,
		},
		{
			name:     "middleware failure is recorded without calling the handler",
			endpoint: "pause_sandbox",
			handler:  ok(http.StatusOK, "nope"),
			middleware: func(ctx context.Context, r *http.Request) (context.Context, *ApiError) {
				return ctx, &ApiError{Code: http.StatusUnauthorized, Message: "Unauthorized"}
			},
			wantStatus:  http.StatusUnauthorized,
			wantHandled: 0,
		},
		{
			name:     "panic is recorded once as 500",
			endpoint: "resume_sandbox",
			handler: func(r *http.Request) (ApiResponse[string], *ApiError) {
				panic("test panic")
			},
			wantStatus:  http.StatusInternalServerError,
			wantHandled: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handled := 0
			handler := func(r *http.Request) (ApiResponse[string], *ApiError) {
				handled++
				return tt.handler(r)
			}
			beforeCounter := testutil.ToFloat64(httpRequestsTotal.WithLabelValues(tt.endpoint, strconv.Itoa(tt.wantStatus)))
			beforeHist := histogramSampleCount(t, tt.endpoint)
			beforeTotal := httpObservationTotal(t)

			mux := http.NewServeMux()
			if tt.middleware != nil {
				RegisterRoute(mux, http.MethodPost, "/items", tt.endpoint, handler, tt.middleware)
			} else {
				RegisterRoute(mux, http.MethodPost, "/items", tt.endpoint, handler)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/items", nil))

			assert.Equal(t, tt.wantStatus, rec.Code)
			assert.Equal(t, tt.wantHandled, handled)
			if tt.wantStatus == http.StatusNoContent {
				assert.Empty(t, rec.Body.String())
			}
			assert.InDelta(t, beforeCounter+1, testutil.ToFloat64(httpRequestsTotal.WithLabelValues(tt.endpoint, strconv.Itoa(tt.wantStatus))), 0)
			assert.Equal(t, beforeHist+1, histogramSampleCount(t, tt.endpoint))
			// The panic defer also calls safeWriteJson. A second observation
			// would move the total by 4 (counter and histogram, twice).
			assert.InDelta(t, beforeTotal+2, httpObservationTotal(t), 0)
			assertLabelNames(t, tt.endpoint)
		})
	}
}

func TestHTTPEndpointMetricsRejectInvalidRequestID(t *testing.T) {
	cfg := tracing.Config{
		Mode:          tracing.TracingModeFile,
		FilePath:      filepath.Join(t.TempDir(), "traces.json"),
		ServiceName:   "framework-test",
		SamplingRatio: 1.0,
	}
	shutdown, err := tracing.InitTracerProvider(context.Background(), cfg)
	require.NoError(t, err)
	defer func() { _ = shutdown(context.Background()) }()

	handled := 0
	beforeCounter := testutil.ToFloat64(httpRequestsTotal.WithLabelValues("list_templates", "400"))
	beforeHist := histogramSampleCount(t, "list_templates")
	mux := http.NewServeMux()
	RegisterRoute(mux, http.MethodGet, "/templates", "list_templates", func(r *http.Request) (ApiResponse[string], *ApiError) {
		handled++
		return ApiResponse[string]{Code: http.StatusOK, Body: "ok"}, nil
	})
	req := httptest.NewRequest(http.MethodGet, "/templates", nil)
	req.Header.Set("X-Request-ID", "not-a-trace-id")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Zero(t, handled)
	assert.InDelta(t, beforeCounter+1, testutil.ToFloat64(httpRequestsTotal.WithLabelValues("list_templates", "400")), 0)
	assert.Equal(t, beforeHist+1, histogramSampleCount(t, "list_templates"))
}

func TestEmptyEndpointEmitsNoHTTPSeries(t *testing.T) {
	before := httpObservationTotal(t)
	mux := http.NewServeMux()
	RegisterRoute(mux, http.MethodGet, "/browser/sb-1/json/version", "", func(r *http.Request) (ApiResponse[string], *ApiError) {
		return ApiResponse[string]{Code: http.StatusOK, Body: "cdp"}, nil
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/browser/sb-1/json/version", nil))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.InDelta(t, before, httpObservationTotal(t), 0)
	assertNoEmptyEndpoint(t)
}

func TestUnmatchedPathEmitsNoHTTPSeries(t *testing.T) {
	beforeTotal := httpObservationTotal(t)
	mux := http.NewServeMux()
	RegisterRoute(mux, http.MethodGet, "/sandboxes/{sandboxID}", "describe_sandbox", func(r *http.Request) (ApiResponse[string], *ApiError) {
		return ApiResponse[string]{Code: http.StatusOK, Body: "ok"}, nil
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/missing", nil))

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.InDelta(t, beforeTotal, httpObservationTotal(t), 0)
}

func TestTrailingSlashSharesEndpoint(t *testing.T) {
	before := testutil.ToFloat64(httpRequestsTotal.WithLabelValues("describe_sandbox", "200"))
	mux := http.NewServeMux()
	RegisterRoute(mux, http.MethodGet, "/sandboxes/{sandboxID}", "describe_sandbox", func(r *http.Request) (ApiResponse[string], *ApiError) {
		return ApiResponse[string]{Code: http.StatusOK, Body: "ok"}, nil
	})
	for _, path := range []string{"/sandboxes/sb-1", "/sandboxes/sb-1/"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusOK, rec.Code)
	}
	assert.InDelta(t, before+2, testutil.ToFloat64(httpRequestsTotal.WithLabelValues("describe_sandbox", "200")), 0)
	assert.Equal(t, 1, endpointSeriesCount(t, "sandbox_http_requests_total", "describe_sandbox", "200"))
}

func TestHTTPDurationUsesManagerLatencyBuckets(t *testing.T) {
	mux := http.NewServeMux()
	RegisterRoute(mux, http.MethodGet, "/templates", "get_template", func(r *http.Request) (ApiResponse[string], *ApiError) {
		return ApiResponse[string]{Code: http.StatusOK, Body: "ok"}, nil
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/templates", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var bounds []float64
	var familyText bytes.Buffer
	for _, family := range mustGather(t) {
		if family.GetName() != "sandbox_http_request_duration_seconds" {
			continue
		}
		_, err := expfmt.MetricFamilyToText(&familyText, family)
		require.NoError(t, err)
		for _, metric := range family.GetMetric() {
			if labelValue(metric, "endpoint") != "get_template" {
				continue
			}
			for _, bucket := range metric.GetHistogram().GetBucket() {
				bounds = append(bounds, bucket.GetUpperBound())
			}
		}
	}
	// The client stores only the finite bounds. Text exposition adds le="+Inf".
	require.Len(t, bounds, 12)
	assert.InDeltaSlice(t, prometheus.ExponentialBuckets(0.02, 2, 12), bounds, 1e-9)
	bucketLines := 0
	for _, line := range strings.Split(familyText.String(), "\n") {
		if strings.Contains(line, `endpoint="get_template"`) && strings.Contains(line, "_bucket{") {
			bucketLines++
		}
	}
	assert.Equal(t, 13, bucketLines)
	assert.Contains(t, familyText.String(), `endpoint="get_template",le="+Inf"`)
}

func histogramSampleCount(t *testing.T, endpoint string) uint64 {
	t.Helper()
	for _, family := range mustGather(t) {
		if family.GetName() != "sandbox_http_request_duration_seconds" {
			continue
		}
		for _, metric := range family.GetMetric() {
			if labelValue(metric, "endpoint") == endpoint {
				return metric.GetHistogram().GetSampleCount()
			}
		}
	}
	return 0
}

func httpObservationTotal(t *testing.T) float64 {
	t.Helper()
	var total float64
	for _, family := range mustGather(t) {
		switch family.GetName() {
		case "sandbox_http_requests_total":
			for _, metric := range family.GetMetric() {
				total += metric.GetCounter().GetValue()
			}
		case "sandbox_http_request_duration_seconds":
			for _, metric := range family.GetMetric() {
				total += float64(metric.GetHistogram().GetSampleCount())
			}
		}
	}
	return total
}

func assertNoEmptyEndpoint(t *testing.T) {
	t.Helper()
	for _, family := range mustGather(t) {
		switch family.GetName() {
		case "sandbox_http_requests_total", "sandbox_http_request_duration_seconds":
			for _, metric := range family.GetMetric() {
				assert.NotEmpty(t, labelValue(metric, "endpoint"), family.GetName())
			}
		}
	}
}

func assertLabelNames(t *testing.T, endpoint string) {
	t.Helper()
	for _, family := range mustGather(t) {
		switch family.GetName() {
		case "sandbox_http_requests_total":
			for _, metric := range family.GetMetric() {
				if labelValue(metric, "endpoint") != endpoint {
					continue
				}
				assert.ElementsMatch(t, []string{"endpoint", "code"}, labelNames(metric))
			}
		case "sandbox_http_request_duration_seconds":
			for _, metric := range family.GetMetric() {
				if labelValue(metric, "endpoint") != endpoint {
					continue
				}
				assert.ElementsMatch(t, []string{"endpoint"}, labelNames(metric))
			}
		}
	}
}

func endpointSeriesCount(t *testing.T, name, endpoint, code string) int {
	t.Helper()
	count := 0
	for _, family := range mustGather(t) {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			if labelValue(metric, "endpoint") == endpoint && (code == "" || labelValue(metric, "code") == code) {
				count++
			}
		}
	}
	return count
}

func mustGather(t *testing.T) []*dto.MetricFamily {
	t.Helper()
	families, err := metrics.Registry.Gather()
	require.NoError(t, err)
	return families
}

func labelValue(metric *dto.Metric, name string) string {
	for _, label := range metric.GetLabel() {
		if label.GetName() == name {
			return label.GetValue()
		}
	}
	return ""
}

func labelNames(metric *dto.Metric) []string {
	names := make([]string, 0, len(metric.GetLabel()))
	for _, label := range metric.GetLabel() {
		names = append(names, label.GetName())
	}
	return names
}
