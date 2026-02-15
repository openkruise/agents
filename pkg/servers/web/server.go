/*
Copyright 2025.

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

package web

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
)

// Service defines the interface that the E2B service must implement.
// We define it here to avoid importing the "e2b" package, preventing import cycles.
type Service interface {
	CreateSandbox(c *gin.Context)
	ListSandboxes(c *gin.Context)
	GetSandbox(c *gin.Context)
	RefreshSandbox(c *gin.Context)
	KillSandbox(c *gin.Context)
}

type Server struct {
	server *http.Server
}

// NewServer accepts the Service interface instead of the concrete e2b type.
func NewServer(addr string, service Service) *Server {
	r := gin.Default()

	// Add OpenTelemetry instrumentation
	r.Use(otelgin.Middleware("kruise-agents"))

	r.POST("/sandboxes", service.CreateSandbox)
	r.GET("/sandboxes", service.ListSandboxes)
	r.GET("/sandboxes/:sandboxID", service.GetSandbox)
	r.POST("/sandboxes/:sandboxID/refreshes", service.RefreshSandbox)
	r.DELETE("/sandboxes/:sandboxID", service.KillSandbox)

	return &Server{
		server: &http.Server{
			Addr:    addr,
			Handler: r,
		},
	}
}

func (s *Server) Run() error {
	return s.server.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}
