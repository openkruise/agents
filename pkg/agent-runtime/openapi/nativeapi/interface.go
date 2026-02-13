package nativeapi

import (
	"github.com/gin-gonic/gin"
)

type OpenAPIServerInterface interface {
	// Get the environment variables
	// (GET /envs)
	GetEnvs(c *gin.Context) // env related functions
	// Download a file
	// (GET /files)
	GetFiles(c *gin.Context) // load file related functions
	// Upload a file and ensure the parent directories exist. If the file exists, it will be overwritten.
	// (POST /files)
	PostFiles(c *gin.Context) // upload file related functions
	// Check the health of the service
	// (GET /health)
	GetHealth(c *gin.Context) // health check related functions
	// Set initial vars, ensure the time and metadata is synced with the host
	// (POST /init)
	PostInit(c *gin.Context) // init related functions
	// Get the stats of the service
	// (GET /metrics)
	GetMetrics(c *gin.Context) // metrics related functions
}
