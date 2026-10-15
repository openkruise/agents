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

package e2b

import (
	"net/http"
	"net/http/httptest"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/openkruise/agents/pkg/servers/e2b/adapters"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

// frozenHTTPEndpoints is the cross-item alias contract. Routes must emit
// exactly this set and no other endpoint value.
var frozenHTTPEndpoints = []string{
	"create_sandbox",
	"list_sandboxes",
	"describe_sandbox",
	"delete_sandbox",
	"update_sandbox_network",
	"pause_sandbox",
	"resume_sandbox",
	"connect_sandbox",
	"connect_sandbox_v2",
	"set_sandbox_timeout",
	"create_snapshot",
	"list_snapshots",
	"list_templates",
	"get_template",
	"delete_template",
	"list_teams",
	"get_compatible_api_key",
	"list_api_keys",
	"create_api_key",
	"delete_api_key",
	"refresh_traffic_access_token",
}

func TestE2BHTTPMetricAliases(t *testing.T) {
	controller := newMetricsController()
	prefix := adapters.CustomPrefix + "/api"
	routes := []struct {
		method   string
		path     string
		endpoint string
		prefixed bool
	}{
		{http.MethodPost, "/sandboxes", "create_sandbox", true},
		{http.MethodPost, "/v2/sandboxes", "create_sandbox", true},
		{http.MethodGet, "/v2/sandboxes", "list_sandboxes", true},
		{http.MethodGet, "/sandboxes/sb-1", "describe_sandbox", true},
		{http.MethodDelete, "/sandboxes/sb-1", "delete_sandbox", true},
		{http.MethodPut, "/sandboxes/sb-1/network", "update_sandbox_network", true},
		{http.MethodPost, "/sandboxes/sb-1/pause", "pause_sandbox", true},
		{http.MethodPost, "/sandboxes/sb-1/resume", "resume_sandbox", true},
		{http.MethodPost, "/sandboxes/sb-1/connect", "connect_sandbox", true},
		{http.MethodPost, "/v2/sandboxes/sb-1/connect", "connect_sandbox_v2", true},
		{http.MethodPost, prefix + "/sandboxes/sb-1/traffic-access-token", "refresh_traffic_access_token", false},
		{http.MethodPost, "/sandboxes/sb-1/timeout", "set_sandbox_timeout", true},
		{http.MethodPost, "/sandboxes/sb-1/snapshots", "create_snapshot", true},
		{http.MethodGet, "/snapshots", "list_snapshots", true},
		{http.MethodGet, "/templates", "list_templates", true},
		{http.MethodGet, "/templates/tpl-1", "get_template", true},
		{http.MethodDelete, "/templates/tpl-1", "delete_template", true},
		{http.MethodGet, "/teams", "list_teams", true},
		{http.MethodGet, "/api-keys/compatible", "get_compatible_api_key", true},
		{http.MethodGet, "/api-keys", "list_api_keys", true},
		{http.MethodPost, "/api-keys", "create_api_key", true},
		{http.MethodDelete, "/api-keys/key-1", "delete_api_key", true},
	}

	seen := map[string]struct{}{}
	for _, route := range routes {
		seen[route.endpoint] = struct{}{}
		paths := []string{route.path}
		if route.prefixed {
			paths = append(paths, prefix+route.path)
		}
		for _, path := range paths {
			t.Run(route.method+" "+path, func(t *testing.T) {
				beforeCounter := httpCounter(t, route.endpoint, "401")
				beforeHistogram := httpHistogramCount(t, route.endpoint)
				rec := serveInvalidKey(controller, route.method, path)
				require.Equal(t, http.StatusUnauthorized, rec.Code)
				assert.InDelta(t, beforeCounter+1, httpCounter(t, route.endpoint, "401"), 0)
				assert.Equal(t, beforeHistogram+1, httpHistogramCount(t, route.endpoint))
			})
		}
	}
	assert.ElementsMatch(t, frozenHTTPEndpoints, mapKeys(seen))
	assert.ElementsMatch(t, frozenHTTPEndpoints, observedHTTPEndpoints(t))
}

func TestBrowserAndUnregisteredPathsEmitNoHTTPMetrics(t *testing.T) {
	controller := newMetricsController()
	prefix := adapters.CustomPrefix + "/api"
	paths := []struct {
		method string
		path   string
		status int
	}{
		{http.MethodGet, "/browser/sb-1/json/version", http.StatusUnauthorized},
		{http.MethodGet, prefix + "/browser/sb-1/json/version", http.StatusUnauthorized},
		{http.MethodGet, "/missing", http.StatusNotFound},
	}
	for _, path := range paths {
		t.Run(path.method+" "+path.path, func(t *testing.T) {
			before := httpObservationTotal(t)
			rec := serveInvalidKey(controller, path.method, path.path)
			assert.Equal(t, path.status, rec.Code)
			assert.InDelta(t, before, httpObservationTotal(t), 0)
		})
	}
	for _, endpoint := range observedHTTPEndpoints(t) {
		assert.NotEmpty(t, endpoint)
	}
}

func TestSandboxIDIsNotAnHTTPMetricLabel(t *testing.T) {
	controller := newMetricsController()
	rec := serveInvalidKey(controller, http.MethodGet, "/sandboxes/sb-1")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	before := httpSeriesKeys(t, "describe_sandbox")
	rec = serveInvalidKey(controller, http.MethodGet, "/sandboxes/sb-2")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, before, httpSeriesKeys(t, "describe_sandbox"))
	for _, value := range allHTTPLabelValues(t) {
		assert.NotContains(t, value, "sb-1")
		assert.NotContains(t, value, "sb-2")
	}
}

func newMetricsController() *Controller {
	controller := &Controller{
		mux:    http.NewServeMux(),
		keys:   &lookupKeyStorage{},
		keyCfg: &keys.Config{},
	}
	controller.registerRoutes()
	return controller
}

func serveInvalidKey(controller *Controller, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set(models.HeaderApiKey, "invalid-key")
	rec := httptest.NewRecorder()
	controller.mux.ServeHTTP(rec, req)
	return rec
}

func httpCounter(t *testing.T, endpoint, code string) float64 {
	t.Helper()
	for _, family := range mustGatherHTTP(t) {
		if family.GetName() != "sandbox_http_requests_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			if httpLabel(metric, "endpoint") == endpoint && httpLabel(metric, "code") == code {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func httpHistogramCount(t *testing.T, endpoint string) uint64 {
	t.Helper()
	for _, family := range mustGatherHTTP(t) {
		if family.GetName() != "sandbox_http_request_duration_seconds" {
			continue
		}
		for _, metric := range family.GetMetric() {
			if httpLabel(metric, "endpoint") == endpoint {
				return metric.GetHistogram().GetSampleCount()
			}
		}
	}
	return 0
}

func httpObservationTotal(t *testing.T) float64 {
	t.Helper()
	var total float64
	for _, family := range mustGatherHTTP(t) {
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

func observedHTTPEndpoints(t *testing.T) []string {
	t.Helper()
	seen := map[string]struct{}{}
	for _, family := range mustGatherHTTP(t) {
		switch family.GetName() {
		case "sandbox_http_requests_total", "sandbox_http_request_duration_seconds":
			for _, metric := range family.GetMetric() {
				seen[httpLabel(metric, "endpoint")] = struct{}{}
			}
		}
	}
	return mapKeys(seen)
}

func httpSeriesKeys(t *testing.T, endpoint string) map[string]struct{} {
	t.Helper()
	keys := map[string]struct{}{}
	for _, family := range mustGatherHTTP(t) {
		switch family.GetName() {
		case "sandbox_http_requests_total", "sandbox_http_request_duration_seconds":
			for _, metric := range family.GetMetric() {
				if httpLabel(metric, "endpoint") != endpoint {
					continue
				}
				keys[family.GetName()+"/"+httpLabel(metric, "code")] = struct{}{}
			}
		}
	}
	return keys
}

func allHTTPLabelValues(t *testing.T) []string {
	t.Helper()
	var values []string
	for _, family := range mustGatherHTTP(t) {
		switch family.GetName() {
		case "sandbox_http_requests_total", "sandbox_http_request_duration_seconds":
			for _, metric := range family.GetMetric() {
				for _, label := range metric.GetLabel() {
					values = append(values, label.GetValue())
				}
			}
		}
	}
	return values
}

func mustGatherHTTP(t *testing.T) []*dto.MetricFamily {
	t.Helper()
	families, err := metrics.Registry.Gather()
	require.NoError(t, err)
	return families
}

func httpLabel(metric *dto.Metric, name string) string {
	for _, label := range metric.GetLabel() {
		if label.GetName() == name {
			return label.GetValue()
		}
	}
	return ""
}

func mapKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}
