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
