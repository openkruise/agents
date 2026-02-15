package logs

import (
	"context"

	"github.com/google/uuid"
	"k8s.io/klog/v2"
)

func NewContext(keysAndValues ...any) context.Context {
	logger := klog.LoggerWithValues(klog.Background(), "contextID", uuid.NewString())
	return klog.NewContext(context.Background(), logger.WithValues(keysAndValues...))
}

// NewContextFrom derives a new context from parent context with additional key-value pairs.
// The derived context inherits cancellation from the parent context.
func NewContextFrom(parent context.Context, keysAndValues ...any) context.Context {
	logger := klog.LoggerWithValues(klog.Background(), "contextID", uuid.NewString())
	return klog.NewContext(parent, logger.WithValues(keysAndValues...))
}

func Extend(ctx context.Context, keysAndValues ...any) context.Context {
	logger := klog.FromContext(ctx)
	return klog.NewContext(ctx, logger.WithValues(keysAndValues...))
}
