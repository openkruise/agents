package auth

import (
	"net/http"
	"slices"

	"github.com/gin-gonic/gin"
)

const (
	AccessTokenHeader     = "X-Access-Token"
)

type AuthenticationConfig struct {
	ValidTokens    []string // Valid access tokens
	AllowedPaths   []string // Paths that don't require authentication
	ValidUserNames []string // Valid user names for userName authentication
	EnableSigning  bool     // Whether to enable signature validation
}

// Default allowed paths that don't require authentication
var defaultAllowedPaths = []string{
	"GET/health",
	"GET/files",
	"POST/files",
}

// BearerAuthMiddleware creates access token authentication middleware
func BearerAuthMiddleware(config AuthenticationConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Combine default paths with configured paths
		allowedPaths := append(defaultAllowedPaths, config.AllowedPaths...)
		// Check if the path is allowed without authentication (e.g., health check, endpoints supporting signing)
		requestPath := c.Request.Method + c.FullPath()
		if slices.Contains(allowedPaths, requestPath) {
			c.Next()
			return
		}

		// Only proceed with authentication if AccessToken is set
		if len(config.ValidTokens) == 0 {
			c.Next()
			return
		}

		// Check access token from header
		authHeader := c.GetHeader(AccessTokenHeader)
		if authHeader != "" {
			// Validate access token
			isValidToken := false
			for _, token := range config.ValidTokens {
				if authHeader == token {
					isValidToken = true
					break
				}
			}
			if isValidToken {
				// Validation passed, continue processing
				c.Next()
				return
			}
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "Unauthorized access, please provide a valid access token",
			})
			return
		}
	}
}