/*
Copyright 2025.

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

package tracing

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

func StartClaimSandboxSpan(ctx context.Context, sandboxID, templateID, user string, timeout time.Duration) (context.Context, trace.Span) {
	tracer := GetTracer()
	attrs := []attribute.KeyValue{
		attribute.String(AttrSandboxID, sandboxID),
		attribute.String(AttrTemplateID, templateID),
		attribute.String(AttrUserID, user),
	}
	if timeout > 0 {
		attrs = append(attrs, attribute.Int64(AttrClaimTimeout, int64(timeout.Milliseconds())))
	}
	spanCtx, span := tracer.Start(ctx, SpanClaimSandbox)
	span.SetAttributes(attrs...)
	return spanCtx, span
}

func StartCloneSandboxSpan(ctx context.Context, sandboxID, checkpointID, user string, timeout time.Duration) (context.Context, trace.Span) {
	tracer := GetTracer()
	attrs := []attribute.KeyValue{
		attribute.String(AttrSandboxID, sandboxID),
		attribute.String(AttrCheckpointID, checkpointID),
		attribute.String(AttrUserID, user),
	}
	if timeout > 0 {
		attrs = append(attrs, attribute.Int64(AttrCloneTimeout, int64(timeout.Milliseconds())))
	}
	spanCtx, span := tracer.Start(ctx, SpanCloneSandbox)
	span.SetAttributes(attrs...)
	return spanCtx, span
}

func StartPauseSandboxSpan(ctx context.Context, sandboxID, user string, sbx infra.Sandbox) (context.Context, trace.Span) {
	tracer := GetTracer()
	attrs := []attribute.KeyValue{
		attribute.String(AttrSandboxID, sandboxID),
		attribute.String(AttrUserID, user),
	}
	if sbx != nil {
		if state, _ := sbx.GetState(); state != "" {
			attrs = append(attrs, attribute.String(AttrSandboxState, string(state)))
		}
	}
	spanCtx, span := tracer.Start(ctx, SpanPauseSandbox)
	span.SetAttributes(attrs...)
	return spanCtx, span
}

func RecordClaimMetrics(span trace.Span, metrics infra.ClaimMetrics) {
	if span == nil {
		return
	}
	span.SetAttributes(
		attribute.Int64("claim.total_ms", int64(metrics.Total.Milliseconds())),
		attribute.Int64("claim.wait_ms", int64(metrics.Wait.Milliseconds())),
	)
	if metrics.PickAndLock > 0 {
		span.SetAttributes(attribute.Int64("claim.pick_and_lock_ms", int64(metrics.PickAndLock.Milliseconds())))
	}
}

func RecordCloneMetrics(span trace.Span, metrics infra.CloneMetrics) {
	if span == nil {
		return
	}
	span.SetAttributes(attribute.Int64("clone.total_ms", int64(metrics.Total.Milliseconds())))
	if metrics.Wait > 0 {
		span.SetAttributes(attribute.Int64("clone.wait_ms", int64(metrics.Wait.Milliseconds())))
	}
}

func AddSandboxDetails(span trace.Span, sbx infra.Sandbox) {
	if span == nil || sbx == nil {
		return
	}
	state, reason := sbx.GetState()
	if state != "" {
		span.SetAttributes(attribute.String(AttrSandboxState, string(state)))
	}
	if reason != "" {
		span.SetAttributes(attribute.String("sandbox.state_reason", reason))
	}
}
