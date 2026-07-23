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

package tracing

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// TraceContextAnnotationKey is the annotation key used to propagate
// W3C Trace Context across components via Kubernetes CRD annotations.
const TraceContextAnnotationKey = "agents.kruise.io/trace-context"

// traceParentKey is the standard W3C Trace Context header key used by the
// OTel propagator (https://www.w3.org/TR/trace-context/#traceparent-header).
const traceParentKey = "traceparent"

// annotationCarrier implements propagation.TextMapCarrier over a map[string]string.
type annotationCarrier struct {
	annotations map[string]string
}

// Get returns the value for the given OTel propagator key.
// The standard W3C "traceparent" key is mapped to TraceContextAnnotationKey
// so that the annotation key follows Kubernetes naming conventions.
func (c *annotationCarrier) Get(key string) string {
	if key == traceParentKey {
		return c.annotations[TraceContextAnnotationKey]
	}
	return c.annotations[key]
}

// Set stores the value for the given OTel propagator key.
// The standard W3C "traceparent" key is mapped to TraceContextAnnotationKey
// so that the annotation key follows Kubernetes naming conventions.
func (c *annotationCarrier) Set(key, value string) {
	if key == traceParentKey {
		c.annotations[TraceContextAnnotationKey] = value
		return
	}
	c.annotations[key] = value
}

func (c *annotationCarrier) Keys() []string {
	keys := make([]string, 0, len(c.annotations))
	for k := range c.annotations {
		keys = append(keys, k)
	}
	return keys
}

// rootSpanContextKey is the context key for storing the root span context.
// This allows InjectTraceContext to use the root span's SpanID as the
// traceparent's parent, so that controller Reconcile spans become direct
// children of the root span (per the tracing proposal design).
type rootSpanContextKey struct{}

// WithRootSpanContext captures the current span context from ctx and stores
// it as the "root span context" in the returned context. Subsequent calls to
// InjectTraceContext will use this root span context instead of the current
// (innermost) span, ensuring that traceparent carries the root span's SpanID.
//
// This must be called at the API layer entry point, BEFORE creating any
// child spans (e.g., manager.ClaimSandbox). At that point, the only span in
// ctx is the HTTP middleware root span.
func WithRootSpanContext(ctx context.Context) context.Context {
	spanCtx := trace.SpanFromContext(ctx).SpanContext()
	if !spanCtx.IsValid() {
		return ctx
	}
	return context.WithValue(ctx, rootSpanContextKey{}, spanCtx)
}

// InjectTraceContext injects the trace context from ctx into annotations.
// If a root span context was stored via WithRootSpanContext, it is used
// instead of the current span, so that the annotation's traceparent carries
// the root span's SpanID. This makes controller Reconcile spans direct
// children of the root span.
//
// If annotations is nil, initializes a new map.
// If tracing is disabled or no active span exists, returns annotations without
// injecting anything (except it may allocate an empty map when annotations is nil).
func InjectTraceContext(ctx context.Context, annotations map[string]string) map[string]string {
	if annotations == nil {
		annotations = make(map[string]string)
	}

	// Prefer root span context if available, so that the traceparent carries
	// the root span's SpanID rather than the innermost child span's SpanID.
	if rootSpanCtx, ok := ctx.Value(rootSpanContextKey{}).(trace.SpanContext); ok && rootSpanCtx.IsValid() {
		ctx = trace.ContextWithSpanContext(ctx, rootSpanCtx)
	}

	carrier := &annotationCarrier{annotations: annotations}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	return annotations
}

// ExtractTraceContext extracts trace context from annotations and returns a context
// carrying the extracted span context. If the annotation doesn't exist or is invalid,
// returns ctx unchanged.
func ExtractTraceContext(ctx context.Context, annotations map[string]string) context.Context {
	if annotations == nil {
		return ctx
	}

	carrier := &annotationCarrier{annotations: annotations}
	return otel.GetTextMapPropagator().Extract(ctx, carrier)
}
