/*
Copyright 2026.

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
