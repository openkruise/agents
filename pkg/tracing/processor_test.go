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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

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
// FilteringSpanProcessor wrapping a recordingSpanProcessor. It also flips the
// enabled flag on, since StartReconcileSpan only installs the write flag when
// tracing is enabled. Returns the recording processor and a cleanup function.
func setupTracerWithFilter(t *testing.T) (*recordingSpanProcessor, func()) {
	t.Helper()
	rec := &recordingSpanProcessor{}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(NewFilteringSpanProcessor(rec)),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	prevEnabled := enabledFlag.Load()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	enabledFlag.Store(true)
	return rec, func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
		enabledFlag.Store(prevEnabled)
	}
}

// TestStartReconcileSpan_DisabledSkipsWriteFlag verifies that with tracing
// disabled StartReconcileSpan does not install the write flag, so mode "none"
// pays not even the context allocation.
func TestStartReconcileSpan_DisabledSkipsWriteFlag(t *testing.T) {
	prevEnabled := enabledFlag.Load()
	enabledFlag.Store(false)
	t.Cleanup(func() { enabledFlag.Store(prevEnabled) })

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "disabled-test", Namespace: "default", UID: "disabled-uid",
		},
	}
	ctx, _ := StartReconcileSpan(context.Background(), box)
	assert.Nil(t, ctx.Value(writeFlagKey{}),
		"write flag must not be installed when tracing is disabled")

	// markWrite on such a context must be a safe no-op.
	markWrite(ctx)
	assert.False(t, hasWrite(ctx))
}

func TestEndSpan_NoWrite_MarksNoop(t *testing.T) {
	rec, cleanup := setupTracerWithFilter(t)
	defer cleanup()

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "noop-test", Namespace: "default", UID: "noop-uid",
		},
	}
	ctx, span := StartReconcileSpan(context.Background(), box)
	// No markWrite call — this Reconcile did no write operation.
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
	ctx, span := StartReconcileSpan(context.Background(), box)
	// Simulate a write operation (e.g., CreatePod was called).
	markWrite(ctx)
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

// TestStartControllerSpan_DoesNotMarkWrite verifies Span names are purely
// observational: even a span named CreatePod does not mark the write flag —
// retention is decided solely by the write-tracking client and failures.
func TestStartControllerSpan_DoesNotMarkWrite(t *testing.T) {
	rec, cleanup := setupTracerWithFilter(t)
	defer cleanup()

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "child-write-test", Namespace: "default", UID: "child-uid",
		},
	}
	ctx, reconcileSpan := StartReconcileSpan(context.Background(), box)

	// A CreatePod-named span alone must not mark the write flag.
	ctx, childSpan := StartControllerSpan(ctx, SpanControllerCreatePod)
	EndSpan(ctx, childSpan, nil)

	// The reconcile span ends without any client write: dropped as no-op.
	EndSpan(ctx, reconcileSpan, nil)

	assert.Equal(t, 0, rec.len(), "spans should be dropped when no client write occurred")
}

// TestWriteThroughClient_RetainsIteration is the bare-write regression: a
// Reconcile that performs a Kubernetes write directly through the
// write-tracking client — without wrapping it in any child span — must be
// retained instead of being dropped as no-op.
func TestWriteThroughClient_RetainsIteration(t *testing.T) {
	rec, cleanup := setupTracerWithFilter(t)
	defer cleanup()

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "bare-write-test", Namespace: "default", UID: "bare-write-uid",
		},
	}
	ctx, reconcileSpan := StartReconcileSpan(context.Background(), box)

	// Bare write: no child span at all, just a client call.
	cli := NewWriteTrackingClient(clientfake.NewClientBuilder().Build())
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"}}
	require.NoError(t, cli.Create(ctx, pod))

	EndSpan(ctx, reconcileSpan, nil)

	assert.Equal(t, 1, rec.len(), "iteration with a bare client write must be retained")
}

func TestEndSpan_Error_RetainsWholeReconcileTrace(t *testing.T) {
	rec, cleanup := setupTracerWithFilter(t)
	defer cleanup()

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "fail-test", Namespace: "default", UID: "fail-uid",
		},
	}
	ctx, reconcileSpan := StartReconcileSpan(context.Background(), box)

	// A child operation fails without any write: the whole iteration must be
	// retained so the failure stays visible in trace UIs.
	ctx, childSpan := StartControllerSpan(ctx, SpanControllerEnsureSandboxUpdated)
	EndSpan(ctx, childSpan, errors.New("update failed"))

	// The Reconcile span ends with a nil error but must still be forwarded
	// because a child span in this iteration failed.
	EndSpan(ctx, reconcileSpan, nil)

	require.Equal(t, 2, rec.len(), "failing iteration should retain the whole Reconcile trace")
	var recordedChild, recordedReconcile sdktrace.ReadOnlySpan
	for _, s := range rec.getSpans() {
		if s.Name() == SpanControllerReconcile {
			recordedReconcile = s
		} else {
			recordedChild = s
		}
	}
	require.NotNil(t, recordedChild, "failed child span should be forwarded")
	require.NotNil(t, recordedReconcile, "Reconcile span should be forwarded despite nil error")
	assert.Equal(t, codes.Error, recordedChild.Status().Code)
	for _, s := range []sdktrace.ReadOnlySpan{recordedChild, recordedReconcile} {
		for _, attr := range s.Attributes() {
			assert.False(t, string(attr.Key) == AttrReconcileNoop && attr.Value.AsBool(),
				"span %s must not be marked noop in a failing iteration", s.Name())
		}
	}
}

// TestEndSpan_ReconcileOwnError_RetainsIteration covers failures that are not
// wrapped in any child span (e.g. a transient API error in the Recycling path
// with no status change): the Reconcile span ends with the final Reconcile
// error, so the iteration is marked failed and retained instead of being
// dropped as no-op.
func TestEndSpan_ReconcileOwnError_RetainsIteration(t *testing.T) {
	rec, cleanup := setupTracerWithFilter(t)
	defer cleanup()

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "recycle-fail", Namespace: "default", UID: "recycle-uid",
		},
	}
	ctx, reconcileSpan := StartReconcileSpan(context.Background(), box)
	// No child span and no write happened; the failure surfaces only through
	// the final Reconcile return value passed to EndSpan.
	EndSpan(ctx, reconcileSpan, errors.New("transient get error"))

	require.Equal(t, 1, rec.len(), "iteration failing without a child span must still be exported")
	assert.Equal(t, codes.Error, rec.getSpans()[0].Status().Code)
}

// TestSiblingSpans_ReadThenWrite_NoMissingParent verifies the sibling pattern
// used by call sites: a read-mostly span is started and ended on a LOCAL
// context, and the subsequent write span starts from the Reconcile context.
// The read span (ended before any write, hence dropped as no-op) must not be
// the parent of the retained write span, so the exported trace never
// references a dropped parent.
func TestSiblingSpans_ReadThenWrite_NoMissingParent(t *testing.T) {
	rec, cleanup := setupTracerWithFilter(t)
	defer cleanup()

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sibling-test", Namespace: "default", UID: "sibling-uid",
		},
	}
	ctx, reconcileSpan := StartReconcileSpan(context.Background(), box)

	// Read-mostly span on a local context; it ends before any write occurred
	// in this iteration and is therefore dropped as no-op.
	readCtx, readSpan := StartControllerSpan(ctx, SpanControllerAssumePodCheckpointed)
	EndSpan(readCtx, readSpan, nil)

	// The subsequent write span starts from the Reconcile context, staying a
	// sibling of the dropped read span instead of its child. The write itself
	// goes through the write-tracking client, which marks the iteration.
	writeCtx, writeSpan := StartControllerSpan(ctx, SpanControllerDeletePod)
	cli := NewWriteTrackingClient(clientfake.NewClientBuilder().Build())
	_ = cli.Delete(writeCtx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"}})
	EndSpan(writeCtx, writeSpan, nil)
	EndSpan(ctx, reconcileSpan, nil)

	require.Equal(t, 2, rec.len(), "Reconcile and DeletePod should be exported, the read span dropped")
	var deletePod, reconcile sdktrace.ReadOnlySpan
	for _, s := range rec.getSpans() {
		switch s.Name() {
		case SpanControllerDeletePod:
			deletePod = s
		case SpanControllerReconcile:
			reconcile = s
		}
	}
	require.NotNil(t, deletePod, "DeletePod span should be exported")
	require.NotNil(t, reconcile, "Reconcile span should be exported")
	assert.Equal(t, reconcile.SpanContext().SpanID(), deletePod.Parent().SpanID(),
		"the write span must parent under the retained Reconcile span, not the dropped read span")
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
			name:      "markWrite without write flag is a no-op",
			setup:     func(ctx context.Context) context.Context { return ctx },
			markWrite: true,
			wantWrite: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := tt.setup(context.Background())
			if tt.markWrite {
				markWrite(ctx)
			}
			assert.Equal(t, tt.wantWrite, hasWrite(ctx))
		})
	}
}

func TestFailedFlag(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(ctx context.Context) context.Context
		markFailed bool
		wantFailed bool
	}{
		{
			name:       "no write flag in context returns false",
			setup:      func(ctx context.Context) context.Context { return ctx },
			markFailed: false,
			wantFailed: false,
		},
		{
			name:       "write flag present but not failed returns false",
			setup:      func(ctx context.Context) context.Context { return withWriteFlag(ctx) },
			markFailed: false,
			wantFailed: false,
		},
		{
			name:       "write flag present and failed returns true",
			setup:      func(ctx context.Context) context.Context { return withWriteFlag(ctx) },
			markFailed: true,
			wantFailed: true,
		},
		{
			name:       "markFailed without write flag is a no-op",
			setup:      func(ctx context.Context) context.Context { return ctx },
			markFailed: true,
			wantFailed: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := tt.setup(context.Background())
			if tt.markFailed {
				markFailed(ctx)
			}
			assert.Equal(t, tt.wantFailed, hasFailed(ctx))
		})
	}
}

func TestMarkWrite_Idempotent(t *testing.T) {
	ctx := withWriteFlag(context.Background())
	assert.False(t, hasWrite(ctx), "should be false before markWrite")

	markWrite(ctx)
	assert.True(t, hasWrite(ctx), "should be true after first markWrite")

	markWrite(ctx)
	assert.True(t, hasWrite(ctx), "should remain true after second markWrite")
}

func TestWriteFlag_IndependentFlags(t *testing.T) {
	ctx1 := withWriteFlag(context.Background())
	ctx2 := withWriteFlag(context.Background())

	markWrite(ctx1)
	assert.True(t, hasWrite(ctx1), "ctx1 should be marked")
	assert.False(t, hasWrite(ctx2), "ctx2 should not be affected by marking ctx1")
}

// withTracingEnabled flips the enabled flag for the duration of a test.
func withTracingEnabled(t *testing.T, enabled bool) {
	t.Helper()
	prev := enabledFlag.Load()
	enabledFlag.Store(enabled)
	t.Cleanup(func() { enabledFlag.Store(prev) })
}

func TestNewWriteTrackingClient_DisabledReturnsOriginal(t *testing.T) {
	withTracingEnabled(t, false)
	base := clientfake.NewClientBuilder().Build()
	assert.Same(t, base, NewWriteTrackingClient(base),
		"with tracing disabled the original client must be returned unwrapped")
}

func TestNewWriteTrackingClient_EnabledWraps(t *testing.T) {
	withTracingEnabled(t, true)
	base := clientfake.NewClientBuilder().Build()
	assert.NotSame(t, client.Client(base), NewWriteTrackingClient(base),
		"with tracing enabled the client must be wrapped")
}

// TestWriteTrackingClient_Verbs verifies every write verb marks the
// per-Reconcile write flag (regardless of the call outcome) and read verbs
// never do.
func TestWriteTrackingClient_Verbs(t *testing.T) {
	withTracingEnabled(t, true)

	pod := func() *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"}}
	}

	tests := []struct {
		name      string
		call      func(ctx context.Context, c client.Client)
		wantWrite bool
	}{
		{
			name:      "Create marks write",
			call:      func(ctx context.Context, c client.Client) { _ = c.Create(ctx, pod()) },
			wantWrite: true,
		},
		{
			name:      "Update marks write even on error",
			call:      func(ctx context.Context, c client.Client) { _ = c.Update(ctx, pod()) },
			wantWrite: true,
		},
		{
			name: "Patch marks write even on error",
			call: func(ctx context.Context, c client.Client) {
				_ = c.Patch(ctx, pod(), client.MergeFrom(pod()))
			},
			wantWrite: true,
		},
		{
			name:      "Delete marks write even when object is absent",
			call:      func(ctx context.Context, c client.Client) { _ = c.Delete(ctx, pod()) },
			wantWrite: true,
		},
		{
			name:      "DeleteAllOf marks write",
			call:      func(ctx context.Context, c client.Client) { _ = c.DeleteAllOf(ctx, pod()) },
			wantWrite: true,
		},
		{
			name: "Status Update marks write even on error",
			call: func(ctx context.Context, c client.Client) {
				_ = c.Status().Update(ctx, pod())
			},
			wantWrite: true,
		},
		{
			name: "Status Patch marks write even on error",
			call: func(ctx context.Context, c client.Client) {
				_ = c.Status().Patch(ctx, pod(), client.MergeFrom(pod()))
			},
			wantWrite: true,
		},
		{
			name: "SubResource Update marks write even on error",
			call: func(ctx context.Context, c client.Client) {
				_ = c.SubResource("status").Update(ctx, pod())
			},
			wantWrite: true,
		},
		{
			name: "Get does not mark write",
			call: func(ctx context.Context, c client.Client) {
				_ = c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "p"}, pod())
			},
			wantWrite: false,
		},
		{
			name: "List does not mark write",
			call: func(ctx context.Context, c client.Client) {
				_ = c.List(ctx, &corev1.PodList{})
			},
			wantWrite: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cli := NewWriteTrackingClient(clientfake.NewClientBuilder().Build())
			ctx := withWriteFlag(context.Background())
			tt.call(ctx, cli)
			assert.Equal(t, tt.wantWrite, hasWrite(ctx))
		})
	}
}

// TestWriteTrackingClient_NoFlagContext verifies writes outside a Reconcile
// (no write flag in ctx) are safe no-ops for tracking.
func TestWriteTrackingClient_NoFlagContext(t *testing.T) {
	withTracingEnabled(t, true)
	cli := NewWriteTrackingClient(clientfake.NewClientBuilder().Build())
	ctx := context.Background()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"}}
	assert.NoError(t, cli.Create(ctx, pod))
	assert.False(t, hasWrite(ctx))
}
