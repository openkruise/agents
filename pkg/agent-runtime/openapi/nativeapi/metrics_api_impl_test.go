package nativeapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"

	"github.com/openkruise/agents/pkg/agent-runtime/host"
)

// mockMetricsProvider is a mock implementation of host.MetricsProvider
type mockMetricsProvider struct{}

// GetMetrics returns predefined mock metrics data
func (m *mockMetricsProvider) GetMetrics() (*host.Metrics, error) {
	return &host.Metrics{
		Timestamp:      1710000000,   // Mock timestamp
		CPUCount:       4,            // Mock CPU count
		CPUUsedPercent: 45.67,        // Mock CPU usage percentage
		MemTotalMiB:    8192,         // Mock total memory in MiB
		MemUsedMiB:     2048,         // Mock used memory in MiB
		MemTotal:       8589934592,   // Mock total memory in bytes
		MemUsed:        2147483648,   // Mock used memory in bytes
		DiskUsed:       53687091200,  // Mock used disk space in bytes
		DiskTotal:      107374182400, // Mock total disk space in bytes
	}, nil
}

// TestGetMetrics tests the GetMetrics function
func TestGetMetrics(t *testing.T) {
	// Set Gin to test mode to avoid unnecessary logging during tests
	gin.SetMode(gin.TestMode)

	// Create a new Gin router and register the GetMetrics endpoint
	router := gin.New()
	apiServer := &OpenE2BAPIServerImpl{}
	router.GET("/metrics", apiServer.GetMetrics)

	// Replace the real MetricsProvider with the mock implementation for testing
	originalProvider := host.GetMetricsProvider()
	t.Logf("Original provider type: %T", originalProvider)
	host.SetMetricsProvider(&mockMetricsProvider{})
	defer func() {
		host.SetMetricsProvider(originalProvider) // Restore the original provider after the test
	}()

	// Verify that the current provider is the mock implementation
	provider := host.GetMetricsProvider()
	t.Logf("Current provider type: %T", provider)
	assert.IsType(t, &mockMetricsProvider{}, provider)

	// Create a test HTTP request to the /metrics endpoint
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()

	// Perform the HTTP request using the router
	router.ServeHTTP(w, req)

	// Assert that the response status code is 200 OK
	assert.Equal(t, http.StatusOK, w.Code)

	// Define the expected JSON response body
	expectedBody := `{
		"ts": 1710000000,
		"cpu_count": 4,
		"cpu_used_pct": 45.67,
		"mem_total_mib": 8192,
		"mem_used_mib": 2048,
		"mem_total": 8589934592,
		"mem_used": 2147483648,
		"disk_used": 53687091200,
		"disk_total": 107374182400
	}`

	// Assert that the response body matches the expected JSON
	assert.JSONEq(t, expectedBody, w.Body.String())
}
