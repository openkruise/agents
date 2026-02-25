package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// mockService implements the Service interface for testing purposes
type mockService struct{}

func (m *mockService) CreateSandbox(c *gin.Context) {
	c.Status(http.StatusCreated)
}
func (m *mockService) ListSandboxes(c *gin.Context) {
	c.Status(http.StatusOK)
}
func (m *mockService) GetSandbox(c *gin.Context) {
	c.Status(http.StatusOK)
}
func (m *mockService) RefreshSandbox(c *gin.Context) {
	c.Status(http.StatusOK)
}
func (m *mockService) KillSandbox(c *gin.Context) {
	c.Status(http.StatusNoContent)
}

func TestNewServerWiring(t *testing.T) {
	// 1. Setup
	gin.SetMode(gin.TestMode)
	mock := &mockService{}

	// This call hits the lines in NewServer where you added the middleware
	srv := NewServer(":8080", mock)

	// 2. Create a test request
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/sandboxes", nil)

	// 3. Serve the request (Verify the router connects to the service)
	srv.server.Handler.ServeHTTP(w, req)

	// 4. Assertions
	assert.Equal(t, http.StatusOK, w.Code)
}
