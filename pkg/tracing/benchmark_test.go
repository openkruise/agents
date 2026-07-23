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

// This file benchmarks the overhead that the create-then-filter tracing
// architecture adds to controller Reconcile loops, especially the common
// no-op case where the span is created and then dropped. Benchmarks are not
// executed by regular `go test`; run them with:
//
//	go test -bench=. -benchmem ./pkg/tracing/

package tracing

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// setupBenchTracer creates a TracerProvider with FilteringSpanProcessor for
// benchmarking, matching the production configuration.
func setupBenchTracer(b *testing.B) func() {
	b.Helper()
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
	return func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	}
}

// BenchmarkReconcileSpan_NoWrite measures the overhead of creating and dropping
// a Reconcile span when no write operation occurred (the common no-op case).
func BenchmarkReconcileSpan_NoWrite(b *testing.B) {
	cleanup := setupBenchTracer(b)
	defer cleanup()

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "bench-sandbox", Namespace: "default", UID: "bench-uid",
		},
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ctx, span := StartReconcileSpan(context.Background(), box)
		// No MarkWrite — simulates an empty Reconcile with no write operations.
		EndSpan(ctx, span, nil)
	}
}

// BenchmarkReconcileSpan_WithWrite measures the overhead when a write operation
// occurred (span is retained and forwarded to the batch processor).
func BenchmarkReconcileSpan_WithWrite(b *testing.B) {
	cleanup := setupBenchTracer(b)
	defer cleanup()

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "bench-sandbox", Namespace: "default", UID: "bench-uid",
		},
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ctx, span := StartReconcileSpan(context.Background(), box)
		MarkWrite(ctx)
		EndSpan(ctx, span, nil)
	}
}

// BenchmarkReconcileSpan_WithChildWrite measures the full path: Reconcile span
// + a write-operation child span (e.g. CreatePod) + EndSpan.
func BenchmarkReconcileSpan_WithChildWrite(b *testing.B) {
	cleanup := setupBenchTracer(b)
	defer cleanup()

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "bench-sandbox", Namespace: "default", UID: "bench-uid",
		},
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ctx, reconcileSpan := StartReconcileSpan(context.Background(), box)
		ctx, childSpan := StartControllerSpan(ctx, SpanControllerCreatePod)
		EndSpan(ctx, childSpan, nil)
		EndSpan(ctx, reconcileSpan, nil)
	}
}

// BenchmarkWriteFlag measures the raw write-flag operations (MarkWrite + hasWrite)
// without any span overhead, to isolate the atomic operation cost.
func BenchmarkWriteFlag(b *testing.B) {
	ctx := withWriteFlag(context.Background())

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		MarkWrite(ctx)
		_ = hasWrite(ctx)
	}
}
