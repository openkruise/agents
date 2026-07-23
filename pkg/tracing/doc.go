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

// Package tracing provides OpenTelemetry distributed tracing utilities for
// sandbox lifecycle management across sandbox-manager and sandbox-controller.
//
// The package contains:
//   - provider.go: TracerProvider initialization and OTLP gRPC exporter setup
//   - propagator.go: W3C Trace Context injection/extraction via CRD annotations
//   - processor.go: FilteringSpanProcessor dropping no-op Reconcile Spans
//   - reconcile.go: instrumentation API (StartReconcileSpan, StartControllerSpan,
//     StartManagerRootSpan, StartManagerSpan, EndSpan)
//   - spans.go: Span name and attribute key constants
package tracing
