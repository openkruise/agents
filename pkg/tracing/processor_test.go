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
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// recordingSpanProcessor captures all spans that reach OnEnd for test assertions.
type recordingSpanProcessor struct {
	mu    sync.Mutex
	spans []sdktrace.ReadOnlySpan
}

func (p *recordingSpanProcessor) OnStart(_ context.Context, _ sdktrace.ReadWriteSpan) {}

func (p *recordingSpanProcessor) OnEnd(s sdktrace.ReadOnlySpan) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.spans = append(p.spans, s)
}

func (p *recordingSpanProcessor) Shutdown(_ context.Context) error   { return nil }
func (p *recordingSpanProcessor) ForceFlush(_ context.Context) error { return nil }

func (p *recordingSpanProcessor) len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.spans)
}

func (p *recordingSpanProcessor) getSpans() []sdktrace.ReadOnlySpan {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]sdktrace.ReadOnlySpan(nil), p.spans...)
}

// setupTracerWithFilter creates a TracerProvider whose spans pass through a
// FilteringSpanProcessor wrapping a recordingSpanProcessor. Returns the recording
// processor and a cleanup function.
func setupTracerWithFilter(t *testing.T) (*recordingSpanProcessor, func()) {
	t.Helper()
	rec := &recordingSpanProcessor{}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(NewFilteringSpanProcessor(rec)),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return rec, func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	}
}

func TestEndSpan_NoWrite_MarksNoop(t *testing.T) {
	rec, cleanup := setupTracerWithFilter(t)
	defer cleanup()

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "noop-test", Namespace: "default", UID: "noop-uid",
		},
	}
	ctx, span := StartReconcileSpan(context.Background(), box, "sandbox-controller")
	// No MarkWrite call — this Reconcile did no write operation.
	EndSpan(ctx, span, nil)

	// The span should have AttrReconcileNoop=true and be dropped by the filter.
	assert.Equal(t, 0, rec.len(), "noop span should be dropped by FilteringSpanProcessor")
}

func TestEndSpan_WithWrite_RetainsSpan(t *testing.T) {
	rec, cleanup := setupTracerWithFilter(t)
	defer cleanup()

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "write-test", Namespace: "default", UID: "write-uid",
		},
	}
	ctx, span := StartReconcileSpan(context.Background(), box, "sandbox-controller")
	// Simulate a write operation (e.g., CreatePod was called).
	MarkWrite(ctx)
	EndSpan(ctx, span, nil)

	// The span should NOT have AttrReconcileNoop and should be forwarded.
	assert.Equal(t, 1, rec.len(), "span with write should be forwarded")
	recorded := rec.getSpans()[0]
	hasNoop := false
	for _, attr := range recorded.Attributes() {
		if string(attr.Key) == AttrReconcileNoop && attr.Value.AsBool() {
			hasNoop = true
		}
	}
	assert.False(t, hasNoop, "span with write should not have noop attribute")
}

func TestStartControllerSpan_WriteOperation_MarksWriteFlag(t *testing.T) {
	rec, cleanup := setupTracerWithFilter(t)
	defer cleanup()

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "child-write-test", Namespace: "default", UID: "child-uid",
		},
	}
	ctx, reconcileSpan := StartReconcileSpan(context.Background(), box, "sandbox-controller")

	// StartControllerSpan for a write-operation name should auto-mark the write flag.
	ctx, childSpan := StartControllerSpan(ctx, SpanControllerCreatePod)
	EndSpan(ctx, childSpan, nil)

	// Now end the reconcile span — it should be retained because CreatePod marked write.
	EndSpan(ctx, reconcileSpan, nil)

	// Both spans should be forwarded (Reconcile + CreatePod).
	assert.Equal(t, 2, rec.len(), "both Reconcile and CreatePod spans should be forwarded")
}

func TestStartControllerSpan_NonWriteOperation_DoesNotMarkWriteFlag(t *testing.T) {
	rec, cleanup := setupTracerWithFilter(t)
	defer cleanup()

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "child-nowrite-test", Namespace: "default", UID: "child-nowrite-uid",
		},
	}
	ctx, reconcileSpan := StartReconcileSpan(context.Background(), box, "sandbox-controller")

	// StartControllerSpan for a non-write-operation name should NOT mark the write flag.
	ctx, childSpan := StartControllerSpan(ctx, SpanControllerEnsureSandboxUpdated)
	EndSpan(ctx, childSpan, nil)

	// Now end the reconcile span — it should be dropped because no write occurred.
	EndSpan(ctx, reconcileSpan, nil)

	// Both spans should be dropped (Reconcile + EnsureSandboxUpdated).
	assert.Equal(t, 0, rec.len(), "both spans should be dropped when no write occurred")
}

func TestEndSpan_WithoutWriteFlag_AlwaysExports(t *testing.T) {
	rec, cleanup := setupTracerWithFilter(t)
	defer cleanup()

	// Spans outside a Reconcile (no write flag in ctx, e.g. sandbox-manager
	// request handling) must always be exported, even without any write.
	tracer := otel.GetTracerProvider().Tracer("test")
	ctx, span := tracer.Start(context.Background(), "manager.DoSomething")
	EndSpan(ctx, span, nil)

	assert.Equal(t, 1, rec.len(), "span without write flag should be exported")
}

func TestEndSpan_SetsStatus(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus codes.Code
	}{
		{name: "success sets Ok", err: nil, wantStatus: codes.Ok},
		{name: "error sets Error", err: errors.New("boom"), wantStatus: codes.Error},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, cleanup := setupTracerWithFilter(t)
			defer cleanup()

			tracer := otel.GetTracerProvider().Tracer("test")
			ctx, span := tracer.Start(context.Background(), "op")
			EndSpan(ctx, span, tt.err)

			require.Equal(t, 1, rec.len(), "span should be exported")
			recorded := rec.getSpans()[0]
			assert.Equal(t, tt.wantStatus, recorded.Status().Code)
			if tt.err != nil {
				assert.Contains(t, recorded.Status().Description, tt.err.Error())
			}
		})
	}
}

func TestFilteringSpanProcessor_ForwardsNonNoopSpan(t *testing.T) {
	rec, cleanup := setupTracerWithFilter(t)
	defer cleanup()

	// Create a span without noop attribute — should be forwarded.
	tracer := otel.GetTracerProvider().Tracer("test")
	_, span := tracer.Start(context.Background(), "test-span")
	span.End()

	assert.Equal(t, 1, rec.len(), "non-noop span should be forwarded")
}

func TestFilteringSpanProcessor_DropsNoopSpan(t *testing.T) {
	rec, cleanup := setupTracerWithFilter(t)
	defer cleanup()

	// Create a span with noop attribute — should be dropped.
	tracer := otel.GetTracerProvider().Tracer("test")
	_, span := tracer.Start(context.Background(), "test-span")
	span.SetAttributes(attribute.Bool(AttrReconcileNoop, true))
	span.End()

	assert.Equal(t, 0, rec.len(), "noop span should be dropped")
}

func TestFilteringSpanProcessor_ForwardsNoopFalse(t *testing.T) {
	rec, cleanup := setupTracerWithFilter(t)
	defer cleanup()

	// Create a span with AttrReconcileNoop=false — should be forwarded
	// (only true is dropped).
	tracer := otel.GetTracerProvider().Tracer("test")
	_, span := tracer.Start(context.Background(), "test-span")
	span.SetAttributes(attribute.Bool(AttrReconcileNoop, false))
	span.End()

	assert.Equal(t, 1, rec.len(), "span with noop=false should be forwarded")
}

func TestFilteringSpanProcessor_ShutdownAndForceFlush(t *testing.T) {
	rec := &recordingSpanProcessor{}
	fp := NewFilteringSpanProcessor(rec)

	err := fp.ForceFlush(context.Background())
	assert.NoError(t, err, "ForceFlush should forward to wrapped processor")

	err = fp.Shutdown(context.Background())
	assert.NoError(t, err, "Shutdown should forward to wrapped processor")
}

func TestWriteFlag(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(ctx context.Context) context.Context
		markWrite bool
		wantWrite bool
	}{
		{
			name:      "no write flag in context returns false",
			setup:     func(ctx context.Context) context.Context { return ctx },
			markWrite: false,
			wantWrite: false,
		},
		{
			name:      "write flag present but not marked returns false",
			setup:     func(ctx context.Context) context.Context { return withWriteFlag(ctx) },
			markWrite: false,
			wantWrite: false,
		},
		{
			name:      "write flag present and marked returns true",
			setup:     func(ctx context.Context) context.Context { return withWriteFlag(ctx) },
			markWrite: true,
			wantWrite: true,
		},
		{
			name:      "MarkWrite without write flag is a no-op",
			setup:     func(ctx context.Context) context.Context { return ctx },
			markWrite: true,
			wantWrite: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := tt.setup(context.Background())
			if tt.markWrite {
				MarkWrite(ctx)
			}
			assert.Equal(t, tt.wantWrite, hasWrite(ctx))
		})
	}
}

func TestMarkWrite_Idempotent(t *testing.T) {
	ctx := withWriteFlag(context.Background())
	assert.False(t, hasWrite(ctx), "should be false before MarkWrite")

	MarkWrite(ctx)
	assert.True(t, hasWrite(ctx), "should be true after first MarkWrite")

	MarkWrite(ctx)
	assert.True(t, hasWrite(ctx), "should remain true after second MarkWrite")
}

func TestWriteFlag_IndependentFlags(t *testing.T) {
	ctx1 := withWriteFlag(context.Background())
	ctx2 := withWriteFlag(context.Background())

	MarkWrite(ctx1)
	assert.True(t, hasWrite(ctx1), "ctx1 should be marked")
	assert.False(t, hasWrite(ctx2), "ctx2 should not be affected by marking ctx1")
}
