package agent_runtime

import (
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestSandboxRuntimeServer_Run(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := NewServer(&ServerConfig{
		Port: 9998,
	})
	done := make(chan bool, 1)
	go func() {
		err := server.Run()
		assert.NoError(t, err, "Run should not return an error")
		done <- true
	}()
	time.Sleep(100 * time.Millisecond)
	// mock os send signal
	p, _ := os.FindProcess(os.Getpid())
	p.Signal(syscall.SIGTERM)

	select {
	case <-done:
		t.Log("Server shut down gracefully")
	case <-time.After(5 * time.Second):
		t.Fatal("Test timed out waiting for server to shut down")
	}
}

func TestSandboxRuntimeServer_RunWithImmediateShutdown(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := NewServer(&ServerConfig{
		Port: 9999,
	})
	done := make(chan error, 1)
	go func() {
		done <- server.Run()
	}()

	time.Sleep(50 * time.Millisecond)

	p, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatalf("Failed to find process: %v", err)
	}
	err = p.Signal(syscall.SIGTERM)
	if err != nil {
		t.Fatalf("Failed to send signal: %v", err)
	}
	select {
	case err := <-done:
		assert.NoError(t, err, "Server should shutdown gracefully without error")
	case <-time.After(5 * time.Second):
		t.Fatal("Test timed out waiting for server to shut down")
	}
}

func NewServer(config *ServerConfig) *Server {
	return &Server{
		config:    *config,
		engine:    gin.New(),
		startTime: time.Now(),
	}
}
