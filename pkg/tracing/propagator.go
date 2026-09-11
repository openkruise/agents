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
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/trace"
)

// TraceContextAnnotationKey is the annotation key used to propagate
// W3C Trace Context across components via Kubernetes CRD annotations.
const TraceContextAnnotationKey = "agents.kruise.io/trace-context"

// TraceBaggageAnnotationKey is the annotation key used to propagate
// W3C Baggage across components via Kubernetes CRD annotations. It carries
// trace-scoped metadata such as the user operation that started the trace.
const TraceBaggageAnnotationKey = "agents.kruise.io/trace-baggage"

// traceParentKey is the standard W3C Trace Context header key used by the
// OTel propagator (https://www.w3.org/TR/trace-context/#traceparent-header).
const traceParentKey = "traceparent"

// baggageKey is the standard W3C Baggage header key used by the OTel
// propagator (https://www.w3.org/TR/baggage/#baggage-http-header-format).
const baggageKey = "baggage"

// operationBaggageMember is the baggage member name carrying the user
// operation (HTTP method + route pattern) that started the trace.
const operationBaggageMember = "operation"

// TraceOperationLogKey is the structured-logging key for the user operation
// that started the trace (e.g. "POST /sandboxes/{sandboxID}/pause").
const TraceOperationLogKey = "traceOperation"

// annotationCarrier implements propagation.TextMapCarrier over a map[string]string.
type annotationCarrier struct {
	annotations map[string]string
}

// Get returns the value for the given OTel propagator key.
// The standard W3C "traceparent" and "baggage" keys are mapped to
// Kubernetes-convention annotation keys.
func (c *annotationCarrier) Get(key string) string {
	switch key {
	case traceParentKey:
		return c.annotations[TraceContextAnnotationKey]
	case baggageKey:
		return c.annotations[TraceBaggageAnnotationKey]
	}
	return c.annotations[key]
}

// Set stores the value for the given OTel propagator key.
// The standard W3C "traceparent" and "baggage" keys are mapped to
// Kubernetes-convention annotation keys.
func (c *annotationCarrier) Set(key, value string) {
	switch key {
	case traceParentKey:
		c.annotations[TraceContextAnnotationKey] = value
		return
	case baggageKey:
		c.annotations[TraceBaggageAnnotationKey] = value
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

// HasInjectableTraceContext reports whether ctx carries a valid span context
// that InjectTraceContext would propagate into annotations. Callers can use
// it as a cheap guard to skip work that only exists for trace propagation
// (e.g. a DeepCopy + Patch before deletion) when tracing is disabled or no
// span is active.
func HasInjectableTraceContext(ctx context.Context) bool {
	if rootSpanCtx, ok := ctx.Value(rootSpanContextKey{}).(trace.SpanContext); ok && rootSpanCtx.IsValid() {
		return true
	}
	return trace.SpanContextFromContext(ctx).IsValid()
}

// InjectTraceContext injects the trace context from ctx into annotations.
// If a root span context was stored via WithRootSpanContext, it is used
// instead of the current span, so that the annotation's traceparent carries
// the root span's SpanID. This makes controller Reconcile spans direct
// children of the root span.
//
// When there is nothing to inject (tracing disabled or no valid span in
// ctx), the input is returned untouched — nil stays nil and no map is
// allocated — so callers can cheaply detect "nothing injected" and skip
// otherwise useless API writes (e.g. a Patch whose only purpose is trace
// propagation).
func InjectTraceContext(ctx context.Context, annotations map[string]string) map[string]string {
	// Fast path: the W3C propagator injects nothing without a valid span
	// context, so return the input as-is instead of allocating a map.
	if !HasInjectableTraceContext(ctx) {
		return annotations
	}

	// Prefer root span context if available, so that the traceparent carries
	// the root span's SpanID rather than the innermost child span's SpanID.
	if rootSpanCtx, ok := ctx.Value(rootSpanContextKey{}).(trace.SpanContext); ok && rootSpanCtx.IsValid() {
		ctx = trace.ContextWithSpanContext(ctx, rootSpanCtx)
	}

	if annotations == nil {
		annotations = make(map[string]string, 1)
	}
	// Empty baggage does not trigger a carrier write. Clear the previous value
	// before injection so the new trace context cannot inherit a stale operation.
	delete(annotations, TraceBaggageAnnotationKey)
	carrier := &annotationCarrier{annotations: annotations}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	return annotations
}

// WithTraceOperation records the user operation (e.g. the HTTP method and
// route pattern) that started the trace as an OTel Baggage member, so it
// propagates across components together with the trace context (both via
// HTTP headers and via CR annotations through InjectTraceContext). A raw
// member is used so values may contain spaces; serialization percent-encodes
// them. An invalid operation value leaves ctx unchanged.
func WithTraceOperation(ctx context.Context, operation string) context.Context {
	if operation == "" {
		return ctx
	}
	member, err := baggage.NewMemberRaw(operationBaggageMember, operation)
	if err != nil {
		return ctx
	}
	bag, err := baggage.FromContext(ctx).SetMember(member)
	if err != nil {
		return ctx
	}
	return baggage.ContextWithBaggage(ctx, bag)
}

// TraceOperationFromContext returns the user operation stored by
// WithTraceOperation, or extracted from propagated baggage (HTTP headers or
// CR annotations via ExtractTraceContext). Returns "" when absent.
func TraceOperationFromContext(ctx context.Context) string {
	return baggage.FromContext(ctx).Member(operationBaggageMember).Value()
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
