package logs

import (
	"context"

	"github.com/google/uuid"
	"k8s.io/klog/v2"
)

func NewLoggerContext(ctx context.Context, keysAndValues ...any) context.Context {
	log := klog.FromContext(ctx)
	if log.GetSink() == nil {
		log = klog.Background()
	}
	logger := klog.LoggerWithValues(log, "runtimeContextID", uuid.NewString())
	return klog.NewContext(ctx, logger.WithValues(keysAndValues...))
}
