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
	"testing"

	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestStartReconcileSpan_WithTraceContext(t *testing.T) {
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	defer func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	}()

	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	defer func() { _ = tp.Shutdown(context.Background()) }()

	// Create a parent span and inject trace context into annotations.
	tracer := tp.Tracer("sandbox-manager")
	parentCtx, parentSpan := tracer.Start(context.Background(), "manager.CreateSandbox")
	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			UID:       "test-uid-12345",
		},
	}
	annotations := map[string]string{}
	annotations = InjectTraceContext(parentCtx, annotations)
	box.SetAnnotations(annotations)

	// Start Reconcile span.
	reconcileCtx, reconcileSpan := StartReconcileSpan(context.Background(), box)
	defer reconcileSpan.End()

	assert.True(t, reconcileSpan.SpanContext().IsValid(), "reconcile span should be valid")
	assert.Equal(t, parentSpan.SpanContext().TraceID(), reconcileSpan.SpanContext().TraceID(),
		"reconcile span should share parent trace ID")
	assert.NotEqual(t, parentSpan.SpanContext().SpanID(), reconcileSpan.SpanContext().SpanID(),
		"reconcile span should have different span ID from parent")

	parentSpan.End()

	// Start a second Reconcile span (sibling) from the same annotations.
	reconcileCtx2, reconcileSpan2 := StartReconcileSpan(context.Background(), box)
	defer reconcileSpan2.End()

	assert.Equal(t, reconcileSpan.SpanContext().TraceID(), reconcileSpan2.SpanContext().TraceID(),
		"sibling reconcile spans should share the same trace ID")
	assert.NotEqual(t, reconcileSpan.SpanContext().SpanID(), reconcileSpan2.SpanContext().SpanID(),
		"sibling reconcile spans should have different span IDs")

	_ = reconcileCtx
	_ = reconcileCtx2
}

func TestStartReconcileSpan_WithoutAnnotations(t *testing.T) {
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	defer func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	}()

	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	defer func() { _ = tp.Shutdown(context.Background()) }()

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kubectl-created-sandbox",
			Namespace: "default",
			UID:       "kubectl-uid-67890",
		},
	}
	// No annotations — kubectl-created sandbox has no trace context.

	ctx, span := StartReconcileSpan(context.Background(), box)
	defer span.End()

	// Without trace-context annotation, StartReconcileSpan starts a new root trace.
	// This is useful for manual search via sandbox UID attribute.
	// Noise control is handled by the caller (controller places StartReconcileSpan
	// after all early-return paths), not by StartReconcileSpan itself.
	assert.True(t, span.SpanContext().IsValid(),
		"root span should be valid even without trace-context annotation")
	_ = ctx
}

func TestStartReconcileSpan_NoopTracer(t *testing.T) {
	cleanup := initNoopTracer()
	defer cleanup()

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "noop-test-sandbox",
			Namespace: "default",
		},
	}

	ctx, span := StartReconcileSpan(context.Background(), box)
	defer span.End()

	assert.False(t, span.SpanContext().IsValid(),
		"noop tracer should produce invalid span context")
	_ = ctx
}

func TestStartControllerSpan_WithinReconcile(t *testing.T) {
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	defer func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	}()

	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	defer func() { _ = tp.Shutdown(context.Background()) }()

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "child-span-test",
			Namespace: "default",
			UID:       "child-uid-11111",
		},
	}

	// Inject trace context so StartReconcileSpan creates a real span.
	parentTracer := tp.Tracer("sandbox-manager")
	parentCtx, parentSpan := parentTracer.Start(context.Background(), "manager.CreateSandbox")
	defer parentSpan.End()
	box.SetAnnotations(InjectTraceContext(parentCtx, box.GetAnnotations()))

	// Start Reconcile span first.
	reconcileCtx, reconcileSpan := StartReconcileSpan(context.Background(), box)
	defer reconcileSpan.End()

	// Start a child span within the Reconcile context.
	_, childSpan := StartControllerSpan(reconcileCtx, SpanControllerCreatePod,
		attribute.String(AttrPodName, "test-pod"),
	)
	defer childSpan.End()

	assert.True(t, childSpan.SpanContext().IsValid(), "child span should be valid")
	assert.Equal(t, reconcileSpan.SpanContext().TraceID(), childSpan.SpanContext().TraceID(),
		"child span should share reconcile trace ID")
	assert.NotEqual(t, reconcileSpan.SpanContext().SpanID(), childSpan.SpanContext().SpanID(),
		"child span should have a different span ID from reconcile span")
}

func TestStartControllerSpan_WithNoopTracer(t *testing.T) {
	cleanup := initNoopTracer()
	defer cleanup()

	ctx, span := StartControllerSpan(context.Background(), "test-child-span")
	defer span.End()

	assert.False(t, span.SpanContext().IsValid(),
		"noop tracer should produce invalid span context")
	_ = ctx
}

func TestStartManagerRootSpan_CreatesRootWithRequestIDTraceID(t *testing.T) {
	prevTP := otel.GetTracerProvider()
	defer otel.SetTracerProvider(prevTP)

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithIDGenerator(&RequestIDGenerator{}),
	)
	otel.SetTracerProvider(tp)
	defer func() { _ = tp.Shutdown(context.Background()) }()

	requestID := "0123456789abcdef0123456789abcdef"
	ctx, span := StartManagerRootSpan(context.Background(), "POST /sandboxes", requestID)
	defer span.End()

	assert.True(t, span.SpanContext().IsValid(), "root span should be valid")
	assert.False(t, span.SpanContext().IsRemote(), "root span should not have a remote parent")
	assert.Equal(t, requestID, span.SpanContext().TraceID().String(),
		"TraceID should equal the request ID for trace-log correlation")
	_ = ctx
}

func TestStartManagerSpan_ChildAndRootBehavior(t *testing.T) {
	prevTP := otel.GetTracerProvider()
	defer otel.SetTracerProvider(prevTP)

	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	otel.SetTracerProvider(tp)
	defer func() { _ = tp.Shutdown(context.Background()) }()

	// With a parent in ctx it creates a child sharing the trace ID.
	parentCtx, parentSpan := Tracer(managerTracerName).Start(context.Background(), "parent")
	defer parentSpan.End()
	_, childSpan := StartManagerSpan(parentCtx, SpanManagerClaimSandbox)
	defer childSpan.End()
	assert.Equal(t, parentSpan.SpanContext().TraceID(), childSpan.SpanContext().TraceID(),
		"child span should share the parent trace ID")

	// Without a parent it starts a new root trace instead of a noop span,
	// because the manager originates traces (unlike StartControllerSpan).
	_, rootSpan := StartManagerSpan(context.Background(), SpanManagerClaimSandbox)
	defer rootSpan.End()
	assert.True(t, rootSpan.SpanContext().IsValid(),
		"manager span without a parent should start a new root trace")
}

func TestTraceIDFromContext(t *testing.T) {
	tests := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{
			name: "no trace ID in context returns empty string",
			ctx:  context.Background(),
			want: "",
		},
		{
			name: "trace ID in context returns the ID",
			ctx:  context.WithValue(context.Background(), traceIDKey{}, "test-trace-id-123"),
			want: "test-trace-id-123",
		},
		{
			name: "wrong type value returns empty string",
			ctx:  context.WithValue(context.Background(), traceIDKey{}, 12345),
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TraceIDFromContext(tt.ctx)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestStartReconcileSpan_StoresTraceIDInContext(t *testing.T) {
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	defer func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	}()

	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	defer func() { _ = tp.Shutdown(context.Background()) }()

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "trace-id-test",
			Namespace: "default",
			UID:       "trace-id-uid",
		},
	}

	ctx, span := StartReconcileSpan(context.Background(), box)
	defer span.End()

	traceID := TraceIDFromContext(ctx)
	assert.NotEmpty(t, traceID, "trace ID should be stored in context after StartReconcileSpan")
	assert.Equal(t, span.SpanContext().TraceID().String(), traceID,
		"stored trace ID should match span's trace ID")
}
