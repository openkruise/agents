package logs

import (
	"context"

	"github.com/google/uuid"
	"k8s.io/klog/v2"
)

func NewContext() context.Context {
	return NewContextWithID(uuid.NewString())
}

func NewContextWithID(contextID string) context.Context {
	return klog.NewContext(context.Background(), klog.LoggerWithValues(klog.Background(), "contextID", contextID))
}
