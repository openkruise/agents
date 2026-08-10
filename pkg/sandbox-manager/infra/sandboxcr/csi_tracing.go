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

package sandboxcr

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/tracing"
	"github.com/openkruise/agents/pkg/utils/runtime"
	"github.com/openkruise/agents/pkg/utils/runtime/config"
)

// traceCSIMounts wraps runtime.ProcessCSIMounts in a manager child span that
// records the volume count and driver list, returning the mount duration and
// error so each caller (claim, clone) keeps its own metrics reporting and
// error wrapping. rtOpts is forwarded to the runtime transport so a
// TLS-capable sandbox mounts its volumes over HTTPS; empty keeps plaintext.
func traceCSIMounts(ctx context.Context, sbx *v1alpha1.Sandbox, opts config.CSIMountOptions, rtOpts ...runtime.Option) (time.Duration, error) {
	ctx, span := tracing.StartManagerSpan(ctx, tracing.SpanInfraProcessCSIMounts)
	// Project the mount configs onto the driver-name list accepted by
	// attribute.StringSlice (which copies the slice internally).
	drivers := make([]string, len(opts.MountOptionList))
	for i, m := range opts.MountOptionList {
		drivers[i] = m.Driver
	}
	span.SetAttributes(
		attribute.Int(tracing.AttrCSIVolumeCount, len(opts.MountOptionList)),
		attribute.StringSlice(tracing.AttrCSIDrivers, drivers),
	)
	duration, err := runtime.ProcessCSIMounts(ctx, sbx, opts, rtOpts...)
	tracing.EndSpan(ctx, span, err)
	return duration, err
}
