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
	"sync/atomic"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// filteringSpanProcessor wraps another SpanProcessor and drops Spans that were
// marked as no-op via the AttrReconcileNoop attribute. It lets the controller
// always create a Reconcile Span (so child operation Spans have a valid
// parent) while still keeping empty, write-free Reconcile iterations out of the
// exported trace data. All non-marked Spans are forwarded to the wrapped
// processor unchanged.
type filteringSpanProcessor struct {
	next sdktrace.SpanProcessor
}

// NewFilteringSpanProcessor returns a SpanProcessor that discards Spans carrying
// AttrReconcileNoop=true and forwards everything else to next.
func NewFilteringSpanProcessor(next sdktrace.SpanProcessor) sdktrace.SpanProcessor {
	return &filteringSpanProcessor{next: next}
}

// OnStart forwards to the wrapped processor. Filtering can only be decided at
// OnEnd, once the no-op attribute has been set by the Reconcile logic.
func (p *filteringSpanProcessor) OnStart(parent context.Context, s sdktrace.ReadWriteSpan) {
	p.next.OnStart(parent, s)
}

// OnEnd drops the Span if it is marked no-op; otherwise forwards it.
func (p *filteringSpanProcessor) OnEnd(s sdktrace.ReadOnlySpan) {
	for _, attr := range s.Attributes() {
		if string(attr.Key) == AttrReconcileNoop && attr.Value.AsBool() {
			return
		}
	}
	p.next.OnEnd(s)
}

// Shutdown forwards to the wrapped processor.
func (p *filteringSpanProcessor) Shutdown(ctx context.Context) error {
	return p.next.Shutdown(ctx)
}

// ForceFlush forwards to the wrapped processor.
func (p *filteringSpanProcessor) ForceFlush(ctx context.Context) error {
	return p.next.ForceFlush(ctx)
}

// writeFlagKey is the context key for the per-Reconcile write flag.
type writeFlagKey struct{}

// writeFlag tracks whether any real write operation (e.g. CreatePod, DeletePod,
// status Patch, finalizer removal) or any failure occurred during a single
// Reconcile iteration. It is shared across the whole Reconcile call tree via
// context so that the Reconcile Span (and its EnsureSandbox* child Spans) can be
// marked as no-op and dropped by FilteringSpanProcessor only when nothing was
// written and nothing failed. Failed iterations are always retained so errors
// stay visible in trace UIs.
type writeFlag struct {
	written atomic.Bool
	failed  atomic.Bool
}

// withWriteFlag returns a context carrying a fresh write flag. It must be called
// once at the start of each Reconcile iteration (in StartReconcileSpan) so that
// downstream write operations can mark it via markWrite.
func withWriteFlag(ctx context.Context) context.Context {
	return context.WithValue(ctx, writeFlagKey{}, &writeFlag{})
}

// markWrite records that a real write operation happened in the current Reconcile.
// It is deliberately unexported: the only marking path is the write-tracking
// client (see NewWriteTrackingClient) intercepting Kubernetes write calls, so instrumentation
// authors never have to know about the flag — any write issued through the
// client is tracked automatically.
// It is a no-op if the context carries no write flag (e.g. tracing disabled or
// called outside a Reconcile). Safe for concurrent use.
func markWrite(ctx context.Context) {
	if f, ok := ctx.Value(writeFlagKey{}).(*writeFlag); ok {
		f.written.Store(true)
	}
}

// hasWrite reports whether markWrite was called for the current Reconcile.
// Returns false if the context carries no write flag.
func hasWrite(ctx context.Context) bool {
	if f, ok := ctx.Value(writeFlagKey{}).(*writeFlag); ok {
		return f.written.Load()
	}
	return false
}

// markFailed records that an operation failed in the current Reconcile. It is
// called by EndSpan when the ended Span carries a non-nil error, so the whole
// iteration (including the Reconcile Span, which is ended later with a nil
// error) is retained instead of being dropped as no-op. It is a no-op if the
// context carries no write flag (e.g. tracing disabled or called outside a
// Reconcile). Safe for concurrent use.
func markFailed(ctx context.Context) {
	if f, ok := ctx.Value(writeFlagKey{}).(*writeFlag); ok {
		f.failed.Store(true)
	}
}

// hasFailed reports whether any Span in the current Reconcile ended with an
// error. Returns false if the context carries no write flag.
func hasFailed(ctx context.Context) bool {
	if f, ok := ctx.Value(writeFlagKey{}).(*writeFlag); ok {
		return f.failed.Load()
	}
	return false
}

// NewWriteTrackingClient wraps c so that every Kubernetes write issued through
// it (Create, Update, Patch, Delete, DeleteAllOf, and subresource writes such
// as Status().Patch) marks the current Reconcile iteration as having performed
// real work. This is the ONLY mechanism that marks a Reconcile as a write:
// instrumentation authors never deal with write marking — any write that goes
// through the client is tracked automatically, and Spans (StartControllerSpan
// + EndSpan) are purely observational.
//
// A write is counted when the write method is invoked, regardless of the
// result: the request reached the API server, so the iteration did real work
// worth retaining (a failed write additionally retains the iteration via the
// failed flag when its error ends a Span or the Reconcile).
//
// When tracing is disabled the original client is returned unwrapped, so the
// call path is structurally identical to a build without tracing — the same
// zero-overhead philosophy as the no-op filter and sampler, which are not
// installed at all in mode "none". This requires InitTracerProvider to have
// run before controllers are assembled, which is its documented contract.
//
// Reads (Get, List, Watch) are forwarded without interception.
func NewWriteTrackingClient(c client.Client) client.Client {
	if !Enabled() {
		return c
	}
	return &writeTrackingClient{Client: c}
}

// writeTrackingClient decorates client.Client, marking the per-Reconcile write
// flag on every write-verb call. markWrite is a no-op when the context carries
// no write flag (e.g. outside a Reconcile), so sharing the wrapped client with
// non-Reconcile code paths is safe.
type writeTrackingClient struct {
	client.Client
}

func (c *writeTrackingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	markWrite(ctx)
	return c.Client.Create(ctx, obj, opts...)
}

func (c *writeTrackingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	markWrite(ctx)
	return c.Client.Update(ctx, obj, opts...)
}

func (c *writeTrackingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	markWrite(ctx)
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c *writeTrackingClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	markWrite(ctx)
	return c.Client.Delete(ctx, obj, opts...)
}

func (c *writeTrackingClient) DeleteAllOf(ctx context.Context, obj client.Object, opts ...client.DeleteAllOfOption) error {
	markWrite(ctx)
	return c.Client.DeleteAllOf(ctx, obj, opts...)
}

func (c *writeTrackingClient) Apply(ctx context.Context, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
	markWrite(ctx)
	return c.Client.Apply(ctx, obj, opts...)
}

func (c *writeTrackingClient) Status() client.SubResourceWriter {
	return &writeTrackingSubResourceWriter{writer: c.Client.Status()}
}

func (c *writeTrackingClient) SubResource(subResource string) client.SubResourceClient {
	return &writeTrackingSubResourceClient{SubResourceClient: c.Client.SubResource(subResource)}
}

// writeTrackingSubResourceWriter marks writes issued via Status().
type writeTrackingSubResourceWriter struct {
	writer client.SubResourceWriter
}

func (w *writeTrackingSubResourceWriter) Create(ctx context.Context, obj client.Object, subResource client.Object, opts ...client.SubResourceCreateOption) error {
	markWrite(ctx)
	return w.writer.Create(ctx, obj, subResource, opts...)
}

func (w *writeTrackingSubResourceWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	markWrite(ctx)
	return w.writer.Update(ctx, obj, opts...)
}

func (w *writeTrackingSubResourceWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	markWrite(ctx)
	return w.writer.Patch(ctx, obj, patch, opts...)
}

func (w *writeTrackingSubResourceWriter) Apply(ctx context.Context, obj runtime.ApplyConfiguration, opts ...client.SubResourceApplyOption) error {
	markWrite(ctx)
	return w.writer.Apply(ctx, obj, opts...)
}

// writeTrackingSubResourceClient marks writes issued via SubResource(...);
// its read method (Get) is forwarded by the embedded client untouched.
type writeTrackingSubResourceClient struct {
	client.SubResourceClient
}

func (c *writeTrackingSubResourceClient) Create(ctx context.Context, obj client.Object, subResource client.Object, opts ...client.SubResourceCreateOption) error {
	markWrite(ctx)
	return c.SubResourceClient.Create(ctx, obj, subResource, opts...)
}

func (c *writeTrackingSubResourceClient) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	markWrite(ctx)
	return c.SubResourceClient.Update(ctx, obj, opts...)
}

func (c *writeTrackingSubResourceClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	markWrite(ctx)
	return c.SubResourceClient.Patch(ctx, obj, patch, opts...)
}

func (c *writeTrackingSubResourceClient) Apply(ctx context.Context, obj runtime.ApplyConfiguration, opts ...client.SubResourceApplyOption) error {
	markWrite(ctx)
	return c.SubResourceClient.Apply(ctx, obj, opts...)
}
