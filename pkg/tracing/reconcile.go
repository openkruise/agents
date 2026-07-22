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

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Tracer scope names used by the Start* helpers below. Keeping them private
// to this package guarantees every span carries the right otel.scope.name in
// trace UIs, telling which component emitted it.
const (
	// managerTracerName scopes all sandbox-manager spans.
	managerTracerName = "sandbox-manager"
	// controllerTracerName scopes all sandbox-controller operation spans.
	controllerTracerName = "sandbox"
)

// traceIDKey is the context key for storing the trace ID extracted from
// the Reconcile span. Callers can use TraceIDFromContext to retrieve it and
// inject it into their logging framework (e.g., klog.FromContext).
type traceIDKey struct{}

// TraceIDFromContext returns the trace ID stored in ctx by StartReconcileSpan.
// Returns empty string if no trace ID is present (e.g., tracing disabled or
// StartReconcileSpan was not called).
func TraceIDFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(traceIDKey{}).(string); ok {
		return id
	}
	return ""
}

// StartReconcileSpan creates a Span for a controller-runtime Reconcile iteration.
// It extracts the trace context from the CR's annotations to establish a parent-child
// relationship with the sandbox-manager root Span. Multiple Reconcile iterations
// for the same user operation produce sibling Spans (same TraceID, different SpanID).
//
// If no trace-context annotation exists (e.g., kubectl-created sandbox), the Span
// starts a new root trace — still useful for manual search via sandbox UID attribute.
//
// This function is for controller Reconcile entry points only. To instrument
// an operation inside a Reconcile, use StartControllerSpan + EndSpan instead.
//
// IMPORTANT: The caller must invoke this AFTER all early-return paths that indicate
// "no work to do" (e.g., Sandbox not found, terminal state, expectation unsatisfied).
// This avoids creating noise Spans for no-op Reconciles.
func StartReconcileSpan(ctx context.Context, obj client.Object, controllerName string) (context.Context, trace.Span) {
	// Extract trace context from CR annotations.
	annotations := obj.GetAnnotations()
	ctx = ExtractTraceContext(ctx, annotations)

	// Attach a fresh write flag so downstream write operations can mark this
	// Reconcile as having performed real work. Reconcile and EnsureSandbox*
	// Spans with no write are dropped by FilteringSpanProcessor (see EndSpan).
	ctx = withWriteFlag(ctx)

	tracer := Tracer(controllerName)
	attrs := []attribute.KeyValue{
		attribute.String(AttrSandboxID, string(obj.GetUID())),
		attribute.String(AttrSandboxNamespace, obj.GetNamespace()),
		attribute.String(AttrSandboxName, obj.GetName()),
	}
	ctx, span := tracer.Start(ctx, SpanControllerReconcile,
		trace.WithAttributes(attrs...),
		trace.WithSpanKind(trace.SpanKindInternal),
	)
	// Store trace ID in context so callers can inject it into their logger
	// for unified trace-log correlation across manager and controller.
	if span.SpanContext().TraceID().IsValid() {
		ctx = context.WithValue(ctx, traceIDKey{}, span.SpanContext().TraceID().String())
	}
	return ctx, span
}

// StartControllerSpan starts a child Span for a specific controller-side
// operation (e.g. CreatePod, DeletePod) within the trace carried by ctx.
// Together with EndSpan it is the only pair of functions needed to instrument
// a piece of controller code:
//
//	ctx, span := tracing.StartControllerSpan(ctx, tracing.SpanControllerCreatePod,
//	    attribute.String(tracing.AttrPodName, pod.Name))
//	err := c.Create(ctx, pod)
//	tracing.EndSpan(ctx, span, err)
//
// Parameters:
//   - ctx: context carrying the parent Span, e.g. the one returned by
//     StartReconcileSpan or an enclosing StartControllerSpan. If it carries no
//     valid parent Span (tracing disabled, or called outside a traced entry
//     point), a no-op Span is returned so instrumentation stays zero-cost and
//     never creates orphan root Spans.
//   - name: Span name; use one of the SpanController* constants from spans.go.
//     Names registered in writeSpanNames additionally mark the enclosing
//     Reconcile as a write operation, so its Spans are retained instead of
//     being dropped as no-op.
//   - attrs: optional attributes built with attribute.String/Int/... and the
//     Attr* keys from spans.go.
//
// Returns the context carrying the new Span (pass it to the instrumented
// call) and the Span itself (pass it to EndSpan).
func StartControllerSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	// If no valid parent span exists in ctx, return noop to avoid orphan spans.
	if !trace.SpanFromContext(ctx).SpanContext().IsValid() {
		return ctx, trace.SpanFromContext(context.Background())
	}

	// A write-operation Span (e.g. CreatePod, DeletePod, updateSandboxStatus)
	// means this Reconcile did real work; mark it so the enclosing Reconcile and
	// EnsureSandbox* Spans are retained rather than filtered as no-op.
	if writeSpanNames[name] {
		MarkWrite(ctx)
	}

	tracer := Tracer(controllerTracerName)
	opts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindInternal),
	}
	if len(attrs) > 0 {
		opts = append(opts, trace.WithAttributes(attrs...))
	}
	return tracer.Start(ctx, name, opts...)
}

// StartManagerRootSpan starts the root Span of a sandbox-manager trace. It is
// meant for request entry points only (one call per HTTP request, in the web
// framework); everything below the entry point uses StartManagerSpan.
//
//	ctx, rootSpan := tracing.StartManagerRootSpan(ctx, "POST /sandboxes", requestID)
//	defer func() { tracing.EndSpan(ctx, rootSpan, err) }()
//
// Parameters:
//   - ctx: the incoming request context. It must NOT already carry a Span:
//     the manager originates traces, so this call always creates a root.
//   - name: Span name describing the request, conventionally "METHOD /path".
//   - requestID: the normalized request ID (32 hex chars, no hyphens). It is
//     stored in ctx so the custom IDGenerator makes TraceID == requestID
//     (unified trace-log correlation), and recorded as the request.id
//     attribute.
//
// Returns the context carrying the root Span (pass it to middlewares and the
// handler) and the Span itself (pass it to EndSpan).
func StartManagerRootSpan(ctx context.Context, name, requestID string) (context.Context, trace.Span) {
	// Store the request ID so the custom IDGenerator uses it as TraceID.
	ctx = WithRequestID(ctx, requestID)
	return Tracer(managerTracerName).Start(ctx, name,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(attribute.String(AttrRequestID, requestID)),
	)
}

// StartManagerSpan starts a child Span for a specific sandbox-manager
// operation (e.g. ClaimSandbox, CreateSnapshot) within the trace carried by
// ctx. Together with EndSpan it is the only pair of functions needed to
// instrument a piece of manager code:
//
//	func (m *SandboxManager) ClaimSandbox(ctx context.Context, ...) (sandbox infra.Sandbox, err error) {
//	    ctx, span := tracing.StartManagerSpan(ctx, tracing.SpanManagerClaimSandbox)
//	    defer func() { tracing.EndSpan(ctx, span, err) }()
//	    ...
//
// Unlike StartControllerSpan it has no no-op guard: the manager originates
// traces, so a call without a parent Span (e.g. a background task) simply
// starts a new root trace instead of being silently dropped.
//
// Parameters:
//   - ctx: context carrying the parent Span, e.g. the one installed by
//     StartManagerRootSpan or an enclosing StartManagerSpan.
//   - name: Span name; use one of the SpanManager*/SpanInfra*/SpanProxy*
//     constants from spans.go.
//   - attrs: optional attributes built with attribute.String/Int/... and the
//     Attr* keys from spans.go.
//
// Returns the context carrying the new Span (pass it to the instrumented
// call) and the Span itself (pass it to EndSpan).
func StartManagerSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	opts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindInternal),
	}
	if len(attrs) > 0 {
		opts = append(opts, trace.WithAttributes(attrs...))
	}
	return Tracer(managerTracerName).Start(ctx, name, opts...)
}

// EndSpan ends a Span and records the outcome of the instrumented operation
// in a single call. It is the single closing function for Spans created by
// any of the Start* helpers above (StartReconcileSpan, StartControllerSpan,
// StartManagerRootSpan, StartManagerSpan):
//
//	tracing.EndSpan(ctx, span, err)
//
// Parameters:
//   - ctx: the context returned by the Start call that created the Span.
//   - span: the Span to end.
//   - err: the error returned by the instrumented operation, nil on success.
//     The Span status is set to codes.Error with err's message when err is
//     non-nil, and to codes.Ok otherwise, so failed operations stand out in
//     trace UIs such as Jaeger.
//
// In addition, a Reconcile-scoped Span (one whose context carries the write
// flag installed by StartReconcileSpan) is marked no-op and dropped by the
// FilteringSpanProcessor when the Reconcile performed no write operation,
// keeping empty Reconcile iterations out of exported traces. Spans outside a
// Reconcile (e.g. sandbox-manager request handling) carry no write flag and
// are always exported.
func EndSpan(ctx context.Context, span trace.Span, err error) {
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
	} else {
		span.SetStatus(codes.Ok, "")
	}
	if _, scoped := ctx.Value(writeFlagKey{}).(*writeFlag); scoped && !hasWrite(ctx) {
		span.SetAttributes(attribute.Bool(AttrReconcileNoop, true))
	}
	span.End()
}
