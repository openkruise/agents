package extendedapi

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

func (s *ExtendedAPIServerImpl) CreateMount(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Service initialized successfully",
		"data": gin.H{
			"initialized": true,
			"timestamp":   time.Now().UTC(),
		},
	})
}