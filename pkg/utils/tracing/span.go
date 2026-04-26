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
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

func StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span, func()) {
	tracer := GetTracer()
	spanCtx, span := tracer.Start(ctx, name)
	if len(attrs) > 0 {
		span.SetAttributes(attrs...)
	}
	return spanCtx, span, func() { span.End() }
}

func RecordSpanError(span trace.Span, err error, errorMessage string, errorCode string) {
	if span == nil || err == nil {
		return
	}
	span.RecordError(err)
	span.SetAttributes(
		attribute.String(AttrErrorMessage, errorMessage),
		attribute.String(AttrErrorCode, errorCode),
	)
	span.SetStatus(codes.Error, errorMessage)
}

func RecordSpanSuccess(span trace.Span) {
	if span == nil {
		return
	}
	span.SetStatus(codes.Ok, "success")
}

func RecordSpanEvent(span trace.Span, eventName string, attrs ...attribute.KeyValue) {
	if span == nil {
		return
	}
	span.AddEvent(eventName, trace.WithAttributes(attrs...))
}

func RecordSpanMetrics(span trace.Span, duration time.Duration, attrs ...attribute.KeyValue) {
	if span == nil {
		return
	}
	metricAttrs := append([]attribute.KeyValue{attribute.Int64("operation.duration_ms", duration.Milliseconds())}, attrs...)
	span.SetAttributes(metricAttrs...)
}

func EndSpan(span trace.Span, err error, message string) {
	if span == nil {
		return
	}
	if err != nil {
		RecordSpanError(span, err, message, "")
	} else {
		RecordSpanSuccess(span)
	}
	span.End()
}
