package permissions

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/user"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// mockUserProvider is a mock implementation of UserProvider for testing
type mockUserProvider struct{}

// GetUser mocks the user retrieval logic
func mockGetUser(username string) (*user.User, error) {
	if username == "validUser" {
		return &user.User{Name: username}, nil
	}
	return nil, fmt.Errorf("user not found")
}

func (m *mockUserProvider) GetUser(username string) (*user.User, error) {
	return mockGetUser(username)
}

// TestWithGinAuthenticateUserName tests the WithGinAuthenticateUserName middleware
func TestWithGinAuthenticateUserName(t *testing.T) {
	// Set Gin to test mode
	gin.SetMode(gin.TestMode)

	// Create a test router
	router := gin.New()
	router.Use(WithGinAuthenticateUserName(&mockUserProvider{}))

	// Define a simple handler to verify middleware behavior
	router.GET("/test", func(c *gin.Context) {
		userGet, exists := c.Get("user")
		if !exists {
			c.JSON(http.StatusOK, gin.H{"message": "no user"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"user": userGet.(*user.User).Name})
	})

	// Test cases
	tests := []struct {
		name           string
		username       string
		password       string
		expectedStatus int
		expectedBody   string
	}{
		{
			name:           "Valid user",
			username:       "validUser",
			password:       "password",
			expectedStatus: http.StatusOK,
			expectedBody:   `{"user":"validUser"}`,
		},
		{
			name:           "Invalid user",
			username:       "invalidUser",
			password:       "password",
			expectedStatus: http.StatusUnauthorized,
			expectedBody:   `{"error":"invalid username: 'invalidUser'"}`,
		},
		{
			name:           "No credentials",
			username:       "",
			password:       "",
			expectedStatus: http.StatusOK,
			expectedBody:   `{"message":"no user"}`,
		},
	}

	// Run test cases
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a test request
			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			if tt.username != "" {
				req.SetBasicAuth(tt.username, tt.password)
			}
			w := httptest.NewRecorder()

			// Perform the request
			router.ServeHTTP(w, req)

			// Assert the results
			assert.Equal(t, tt.expectedStatus, w.Code)
			assert.JSONEq(t, tt.expectedBody, w.Body.String())
		})
	}
}
