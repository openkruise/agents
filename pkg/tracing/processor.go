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
)

// filteringSpanProcessor wraps another SpanProcessor and drops Spans that were
// marked as no-op via the AttrReconcileNoop attribute. It lets the controller
// always create a Reconcile Span (so child write-operation Spans have a valid
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
// status Patch, finalizer removal) occurred during a single Reconcile iteration.
// It is shared across the whole Reconcile call tree via context so that the
// Reconcile Span (and its EnsureSandbox* child Spans) can be marked as no-op and
// dropped by FilteringSpanProcessor when nothing was actually written.
type writeFlag struct {
	written atomic.Bool
}

// withWriteFlag returns a context carrying a fresh write flag. It must be called
// once at the start of each Reconcile iteration (in StartReconcileSpan) so that
// downstream write operations can mark it via MarkWrite.
func withWriteFlag(ctx context.Context) context.Context {
	return context.WithValue(ctx, writeFlagKey{}, &writeFlag{})
}

// MarkWrite records that a real write operation happened in the current Reconcile.
// Write-operation Spans created via StartControllerSpan (see writeSpanNames) call it
// automatically; call it directly only when a write happens inside a helper whose
// individual operations are not wrapped in their own Spans (e.g. a deep function
// reporting success via a done bool). It is a no-op if the context carries no
// write flag (e.g. tracing disabled or called outside a Reconcile).
// Safe for concurrent use.
func MarkWrite(ctx context.Context) {
	if f, ok := ctx.Value(writeFlagKey{}).(*writeFlag); ok {
		f.written.Store(true)
	}
}

// hasWrite reports whether MarkWrite was called for the current Reconcile.
// Returns false if the context carries no write flag.
func hasWrite(ctx context.Context) bool {
	if f, ok := ctx.Value(writeFlagKey{}).(*writeFlag); ok {
		return f.written.Load()
	}
	return false
}
