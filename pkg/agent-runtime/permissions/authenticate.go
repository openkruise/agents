package permissions

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
)

func WithGinAuthenticateUserName(provider UserProvider) gin.HandlerFunc {
	return func(c *gin.Context) {
		username, _, ok := c.Request.BasicAuth()
		if !ok {
			// When no username is provided, ignore the authentication method (not all endpoints require it)
			// Missing user is then handled in the GetAuthUser function
			c.Next()
			return
		}

		user, err := provider.GetUser(username)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": fmt.Sprintf("invalid username: '%s'", username),
			})
			return
		}
		c.Set("user", user)
		c.Next()
	}
}
