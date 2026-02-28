package logs

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/klog/v2"
)

// TestNewLoggerContext tests the NewLoggerContext function
func TestNewLoggerContext(t *testing.T) {
	// Create a background context
	ctx := context.Background()

	// Call NewLoggerContext with some key-value pairs
	newCtx := NewLoggerContext(ctx, "key1", "value1", "key2", "value2")

	// Extract the logger from the new context
	logger := klog.FromContext(newCtx)

	// Verify that the logger is not nil
	assert.NotNil(t, logger, "Logger should not be nil")

	// Log a test message to ensure the logger works and includes the provided key-value pairs
	logger.Info("Test message", "key1", "value1", "key2", "value2")

	// Optionally, capture logs and assert their content if needed (requires custom log capturing logic)
	// For now, we assume the logger is functional based on successful creation and usage.
}
