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

// Package tracing provides reusable trace instrumentation for sandbox controllers.
// It wraps Pod lifecycle operations (create, delete, pause, resume, patch, runtime-init)
// with timing and error classification, delegating output to a pluggable TraceEmitter.
//
// Community builds use an OTEL SDK emitter; internal builds use a JSONL file emitter.
// Both share the same TraceOperation wrapper and TraceLogEntry format.
package tracing

import (
	"context"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
)

// TraceOperation runs op() and emits a structured trace entry.
// module identifies the caller component (e.g., "sandbox-controller").
// phase identifies the lifecycle operation (use Phase* constants from types.go).
// obj must be a *corev1.Pod or *agentsv1alpha1.Sandbox for UID extraction.
// The returned error is the original error from op().
func TraceOperation(ctx context.Context, module, phase string, obj interface{}, op func() error) error {
	startTime := time.Now()
	err := op()
	if !defaultEmitter.Enabled() {
		return err
	}

	uid, creationTime := extractPodInfo(obj)
	if uid == "" {
		return err
	}

	entry := &TraceLogEntry{
		TraceID:      strings.ReplaceAll(uid, "-", ""),
		SpanID:       generateSpanID(),
		Name:         "HandleResource",
		Kind:         "INTERNAL",
		Phase:        phase,
		Module:       module,
		StartTime:    startTime,
		EndTime:      time.Now(),
		Success:      err == nil,
		ErrorCode:    classifyError(err),
		ResourceUID:  uid,
		CreationTime: creationTime,
	}
	defaultEmitter.Emit(ctx, entry)
	return err
}

// TraceOperationTreatNotFoundAsSuccess is like TraceOperation but treats
// NotFound errors as success in the trace output. The original error
// (including NotFound) is still returned to the caller.
// Use this for Delete operations where the resource already being gone is acceptable.
func TraceOperationTreatNotFoundAsSuccess(ctx context.Context, module, phase string, obj interface{}, op func() error) error {
	startTime := time.Now()
	err := op()
	if !defaultEmitter.Enabled() {
		return err
	}

	uid, creationTime := extractPodInfo(obj)
	if uid == "" {
		return err
	}

	success := err == nil || errors.IsNotFound(err)
	errCode := ""
	if err != nil && !errors.IsNotFound(err) {
		errCode = classifyError(err)
	}

	entry := &TraceLogEntry{
		TraceID:      strings.ReplaceAll(uid, "-", ""),
		SpanID:       generateSpanID(),
		Name:         "HandleResource",
		Kind:         "INTERNAL",
		Phase:        phase,
		Module:       module,
		StartTime:    startTime,
		EndTime:      time.Now(),
		Success:      success,
		ErrorCode:    errCode,
		ResourceUID:  uid,
		CreationTime: creationTime,
	}
	defaultEmitter.Emit(ctx, entry)
	return err
}
