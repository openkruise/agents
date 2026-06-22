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

package core

import (
	"context"

	"github.com/openkruise/agents/pkg/utils/tracing"
)

const sandboxControllerModule = "sandbox-controller"

func (r *commonControl) traceOperation(ctx context.Context, phase string, obj interface{}, op func() error) error {
	return tracing.TraceOperation(ctx, sandboxControllerModule, phase, obj, op)
}

func (r *commonControl) traceOperationTreatNotFoundAsSuccess(ctx context.Context, phase string, obj interface{}, op func() error) error {
	return tracing.TraceOperationTreatNotFoundAsSuccess(ctx, sandboxControllerModule, phase, obj, op)
}
