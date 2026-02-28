package extendedapi

import (
	"github.com/gin-gonic/gin"
)

type StorageCSIHandler interface {
	CreateMount(c *gin.Context) // storage related functions for csi solution
}
