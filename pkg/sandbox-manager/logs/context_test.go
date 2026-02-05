package logs

import (
	"context"
	"errors"
	"testing"
	"time"

	"k8s.io/klog/v2"
)

func TestNewContext(t *testing.T) {
	ctx := NewContext("key1", "value1", "key2", "value2")

	if ctx == nil {
		t.Fatal("NewContext returned nil")
	}

	// Verify logger is attached
	logger := klog.FromContext(ctx)
	if logger == (klog.Logger{}) {
		t.Error("Expected logger to be attached to context")
	}

	// Verify context is not canceled
	select {
	case <-ctx.Done():
		t.Error("Context should not be canceled")
	default:
		// Expected: context is not done
	}
}

func TestNewContext_WithoutKeyValues(t *testing.T) {
	ctx := NewContext()

	if ctx == nil {
		t.Fatal("NewContext returned nil")
	}

	// Verify logger is attached
	logger := klog.FromContext(ctx)
	if logger == (klog.Logger{}) {
		t.Error("Expected logger to be attached to context")
	}
}

func TestNewContextFrom(t *testing.T) {
	parent := context.Background()
	ctx := NewContextFrom(parent, "key1", "value1")

	if ctx == nil {
		t.Fatal("NewContextFrom returned nil")
	}

	// Verify logger is attached
	logger := klog.FromContext(ctx)
	if logger == (klog.Logger{}) {
		t.Error("Expected logger to be attached to context")
	}
}

func TestNewContextFrom_InheritsCancellation(t *testing.T) {
	// Create a cancelable parent context
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Derive a new context
	ctx := NewContextFrom(parent, "key", "value")

	// Verify context is not canceled initially
	select {
	case <-ctx.Done():
		t.Error("Context should not be canceled initially")
	default:
		// Expected
	}

	// Cancel parent context
	cancel()

	// Verify derived context is also canceled
	select {
	case <-ctx.Done():
		// Expected: context is canceled
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Errorf("Expected context.Canceled error, got %v", ctx.Err())
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("Derived context should be canceled when parent is canceled")
	}
}

func TestNewContextFrom_WithTimeout(t *testing.T) {
	// Create a parent context with timeout
	parent, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// Derive a new context
	ctx := NewContextFrom(parent, "key", "value")

	// Wait for timeout
	select {
	case <-ctx.Done():
		// Expected: context times out
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Errorf("Expected context.DeadlineExceeded error, got %v", ctx.Err())
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("Context should have timed out")
	}
}

func TestNewContextFrom_WithoutKeyValues(t *testing.T) {
	parent := context.Background()
	ctx := NewContextFrom(parent)

	if ctx == nil {
		t.Fatal("NewContextFrom returned nil")
	}

	// Verify logger is attached
	logger := klog.FromContext(ctx)
	if logger == (klog.Logger{}) {
		t.Error("Expected logger to be attached to context")
	}
}

func TestNewContext_DoesNotInheritCancellation(t *testing.T) {
	// This test verifies that NewContext creates an independent context
	// that does NOT inherit cancellation from any parent

	// Create a cancelable context (simulating HTTP request context)
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Call NewContext (which uses context.Background internally)
	ctx := NewContext("key", "value")

	// Cancel the parent
	cancel()

	// Verify NewContext's context is NOT affected
	select {
	case <-ctx.Done():
		t.Error("NewContext should create independent context, not inherit cancellation")
	case <-time.After(50 * time.Millisecond):
		// Expected: context remains active
	}
}
