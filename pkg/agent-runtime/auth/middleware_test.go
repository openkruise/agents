package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// TestBearerAuthMiddleware tests the BearerAuthMiddleware function
func TestBearerAuthMiddleware(t *testing.T) {
	// Define test cases
	tests := []struct {
		name           string
		config         AuthenticationConfig
		method         string
		path           string
		headers        map[string]string
		expectedStatus int
	}{
		{
			name: "Allowed path without token",
			config: AuthenticationConfig{
				ValidTokens:  []string{"valid-token"},
				AllowedPaths: []string{},
			},
			method:         "GET",
			path:           "/health",
			headers:        map[string]string{},
			expectedStatus: http.StatusOK,
		},
		{
			name: "Valid token provided",
			config: AuthenticationConfig{
				ValidTokens:  []string{"valid-token"},
				AllowedPaths: []string{},
			},
			method: "GET",
			path:   "/protected",
			headers: map[string]string{
				"X-Access-Token": "valid-token",
			},
			expectedStatus: http.StatusOK,
		},
		{
			name: "Invalid token provided",
			config: AuthenticationConfig{
				ValidTokens:  []string{"valid-token"},
				AllowedPaths: []string{},
			},
			method: "GET",
			path:   "/protected",
			headers: map[string]string{
				"X-Access-Token": "invalid-token",
			},
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name: "No token provided",
			config: AuthenticationConfig{
				ValidTokens:  []string{"valid-token"},
				AllowedPaths: []string{},
			},
			method:         "GET",
			path:           "/protected",
			headers:        map[string]string{},
			expectedStatus: http.StatusOK,
		},
		{
			name: "Empty valid tokens list",
			config: AuthenticationConfig{
				ValidTokens:  []string{},
				AllowedPaths: []string{},
			},
			method:         "GET",
			path:           "/protected",
			headers:        map[string]string{},
			expectedStatus: http.StatusOK,
		},
	}

	// Run test cases
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set up Gin router
			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.Use(BearerAuthMiddleware(tt.config))

			// Define a simple handler for testing
			router.GET("/*path", func(c *gin.Context) {
				c.Status(http.StatusOK)
			})

			// Create a test request
			req := httptest.NewRequest(tt.method, tt.path, nil)
			for key, value := range tt.headers {
				req.Header.Set(key, value)
			}

			// Create a response recorder
			w := httptest.NewRecorder()

			// Perform the request
			router.ServeHTTP(w, req)

			// Assert the status code
			assert.Equal(t, tt.expectedStatus, w.Code)
		})
	}
}
