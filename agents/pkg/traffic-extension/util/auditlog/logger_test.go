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

package auditlog

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/apimachinery/pkg/types"
)

// recorderSink is a tiny logr.LogSink that captures Info calls so tests can
// assert on the emitted message and key-value pairs without depending on a
// concrete logging backend.
type recorderSink struct {
	mu      sync.Mutex
	records []recorded
}

type recorded struct {
	msg string
	kvs []any
}

func (r *recorderSink) Init(logr.RuntimeInfo)             {}
func (r *recorderSink) Enabled(int) bool                  { return true }
func (r *recorderSink) Error(error, string, ...any)       {}
func (r *recorderSink) WithName(string) logr.LogSink      { return r }
func (r *recorderSink) WithValues(...any) logr.LogSink    { return r }
func (r *recorderSink) Info(_ int, msg string, kvs ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]any, len(kvs))
	copy(cp, kvs)
	r.records = append(r.records, recorded{msg: msg, kvs: cp})
}

func (r *recorderSink) snapshot() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recorded, len(r.records))
	copy(out, r.records)
	return out
}

func newRecorderLogger() (logr.Logger, *recorderSink) {
	r := &recorderSink{}
	return logr.New(r), r
}

// waitFor polls fn at 5ms intervals until it returns true or timeout fires.
// Used to give the worker goroutine time to drain the channel without
// resorting to ad-hoc sleeps in every test.
func waitFor(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waitFor: condition not met within %s", timeout)
}

func sampleEntry(suffix string) Entry {
	return Entry{
		Pod:      types.NamespacedName{Namespace: "default", Name: "pod-" + suffix},
		Method:   "POST",
		Host:     "api.openai.com",
		Path:     "/v1/chat/completions",
		Profiles: 1,
		Outcome:  "mutated",
		Actions:  []string{"mutator:default/p/r"},
		Skipped:  map[string]int{},
	}
}

func TestNewBufferedLogger_DefaultsBuffer(t *testing.T) {
	logger, _ := newRecorderLogger()
	l := NewBufferedLogger(logger, 0)
	if got := cap(l.ch); got != DefaultBufferSize {
		t.Errorf("expected default buffer cap %d, got %d", DefaultBufferSize, got)
	}
	l2 := NewBufferedLogger(logger, -10)
	if got := cap(l2.ch); got != DefaultBufferSize {
		t.Errorf("expected negative buffer to fall back to %d, got %d", DefaultBufferSize, got)
	}
	l3 := NewBufferedLogger(logger, 16)
	if got := cap(l3.ch); got != 16 {
		t.Errorf("expected explicit buffer cap 16, got %d", got)
	}
}

func TestNop_SubmitIsNoop(t *testing.T) {
	// Submitting to Nop should never panic and should never touch the drop
	// counter.
	before := counterValue(t, AuditLogDroppedTotal)
	Nop().Submit(sampleEntry("nop"))
	if after := counterValue(t, AuditLogDroppedTotal); after != before {
		t.Errorf("Nop.Submit must not touch drop counter; before=%v after=%v", before, after)
	}
}

func TestBufferedLogger_NilReceiverSubmitIsNoop(t *testing.T) {
	var l *BufferedLogger
	// Must not panic, must not increment counter.
	before := counterValue(t, AuditLogDroppedTotal)
	l.Submit(sampleEntry("x"))
	if after := counterValue(t, AuditLogDroppedTotal); after != before {
		t.Errorf("nil receiver Submit must not touch drop counter; before=%v after=%v", before, after)
	}
}

func TestBufferedLogger_SubmitAndEmit(t *testing.T) {
	logger, sink := newRecorderLogger()
	l := NewBufferedLogger(logger, 8)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = l.Start(ctx) }()

	l.Submit(sampleEntry("a"))
	l.Submit(Entry{
		Pod:     types.NamespacedName{Namespace: "ns", Name: "pod-err"},
		Method:  "GET",
		Host:    "h",
		Path:    "/p",
		Outcome: "error",
		Skipped: map[string]int{"mutator": 1},
		Error:   "boom",
	})

	waitFor(t, time.Second, func() bool { return len(sink.snapshot()) == 2 })

	rec := sink.snapshot()
	if rec[0].msg != "egress request handled" {
		t.Errorf("unexpected log message: %q", rec[0].msg)
	}

	first := kvMap(rec[0].kvs)
	if first["pod"] != "default/pod-a" {
		t.Errorf("pod field = %v, want default/pod-a", first["pod"])
	}
	if first["outcome"] != "mutated" {
		t.Errorf("outcome field = %v, want mutated", first["outcome"])
	}
	if _, ok := first["error"]; ok {
		t.Errorf("happy-path entry should not carry an error key, got %v", first["error"])
	}

	second := kvMap(rec[1].kvs)
	if second["error"] != "boom" {
		t.Errorf("error field = %v, want boom", second["error"])
	}
	if got, ok := second["skipped"].(map[string]int); !ok || got["mutator"] != 1 {
		t.Errorf("skipped field = %#v, want {mutator:1}", second["skipped"])
	}
}

// TestBufferedLogger_SubmitDropsWhenFull fills the buffer past its capacity
// with the worker idle, then asserts that AuditLogDroppedTotal grew by the
// number of overflow attempts.
func TestBufferedLogger_SubmitDropsWhenFull(t *testing.T) {
	logger, _ := newRecorderLogger()
	const cap = 4
	l := NewBufferedLogger(logger, cap)

	// Do NOT start the worker — Submit must accept exactly cap entries and
	// drop the rest non-blocking.
	before := counterValue(t, AuditLogDroppedTotal)
	for i := 0; i < cap; i++ {
		l.Submit(sampleEntry("acc"))
	}
	// These must drop.
	const overflow = 3
	for i := 0; i < overflow; i++ {
		l.Submit(sampleEntry("drop"))
	}
	if got := counterValue(t, AuditLogDroppedTotal); got-before != overflow {
		t.Errorf("expected drop delta %d, got %v (before=%v)", overflow, got-before, before)
	}
}

// TestBufferedLogger_ContextCancelDrainsAndExits verifies that cancelling
// the context flushes in-flight entries and unblocks Start.
func TestBufferedLogger_ContextCancelDrainsAndExits(t *testing.T) {
	logger, sink := newRecorderLogger()
	l := NewBufferedLogger(logger, 16)

	// Preload entries before starting the worker; once started, the worker
	// will consume them.
	for i := 0; i < 5; i++ {
		l.Submit(sampleEntry("preload"))
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Start(ctx) }()

	// Give the worker a chance to consume at least one entry through the
	// normal path so we actually exercise both branches of the select.
	waitFor(t, time.Second, func() bool { return len(sink.snapshot()) >= 1 })

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not exit after ctx cancel")
	}

	if got := len(sink.snapshot()); got != 5 {
		t.Errorf("expected 5 entries flushed (preload), got %d", got)
	}
}

// kvMap collects a logr-style flat key/value list into a map for easy
// assertion access.
func kvMap(kvs []any) map[string]any {
	out := make(map[string]any, len(kvs)/2)
	for i := 0; i+1 < len(kvs); i += 2 {
		k, ok := kvs[i].(string)
		if !ok {
			continue
		}
		out[k] = kvs[i+1]
	}
	return out
}

// counterValue reads the current value of a Prometheus collector via the
// official testutil helper so we can build pre/post deltas.
func counterValue(t *testing.T, c prometheus.Collector) float64 {
	t.Helper()
	return testutil.ToFloat64(c)
}
