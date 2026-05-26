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

package metricsasync

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
)

func TestOptions_applyDefaults(t *testing.T) {
	tests := []struct {
		name string
		in   Options
		want Options
	}{
		{
			name: "zero values get defaults",
			in:   Options{},
			want: Options{
				Workers:      8,
				DrainTimeout: 0,
				QueueCap:     0,
				Name:         "metrics_async",
			},
		},
		{
			name: "explicit values preserved",
			in:   Options{Workers: 16, DrainTimeout: time.Second, QueueCap: 100, Name: "custom"},
			want: Options{Workers: 16, DrainTimeout: time.Second, QueueCap: 100, Name: "custom"},
		},
		{
			name: "negative workers clamped to 1",
			in:   Options{Workers: -3, DrainTimeout: 2 * time.Second},
			want: Options{Workers: 1, DrainTimeout: 2 * time.Second, Name: "metrics_async"},
		},
		{
			name: "negative drain treated as no-wait",
			in:   Options{Workers: 2, DrainTimeout: -1},
			want: Options{Workers: 2, DrainTimeout: 0, Name: "metrics_async"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.in.applyDefaults()
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNewCollectors_namesAndLabels(t *testing.T) {
	c := newCollectors("metrics_async_test1")
	// Each Vec is non-nil and accepts the documented label set.
	c.queueDepth.WithLabelValues("sandbox").Set(0)
	c.processedTotal.WithLabelValues("sandbox", "ok").Inc()
	c.duration.WithLabelValues("sandbox").Observe(0.001)
	c.latency.WithLabelValues("sandbox").Observe(0.001)
	c.dropped.WithLabelValues("sandbox", "queue_full").Inc()
}

func TestPool_RegisterKind(t *testing.T) {
	tests := []struct {
		name        string
		setup       func(p *Pool) error
		register    func(p *Pool) error
		expectError string
	}{
		{
			name:     "register single kind succeeds",
			register: func(p *Pool) error { return p.RegisterKind("sandbox", func(string, string) {}) },
		},
		{
			name: "register duplicate kind fails",
			setup: func(p *Pool) error {
				return p.RegisterKind("sandbox", func(string, string) {})
			},
			register:    func(p *Pool) error { return p.RegisterKind("sandbox", func(string, string) {}) },
			expectError: "already registered",
		},
		{
			name:        "empty kind rejected",
			register:    func(p *Pool) error { return p.RegisterKind("", func(string, string) {}) },
			expectError: "empty kind",
		},
		{
			name:        "nil func rejected",
			register:    func(p *Pool) error { return p.RegisterKind("sandbox", nil) },
			expectError: "nil CleanupFunc",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewPool(Options{Name: "metrics_async_test_register_" + tt.name})
			if tt.setup != nil {
				assert.NoError(t, tt.setup(p))
			}
			err := tt.register(p)
			if tt.expectError == "" {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			}
		})
	}
}

func TestPool_Enqueue(t *testing.T) {
	tests := []struct {
		name       string
		opts       Options
		register   bool
		enqueues   []Key
		wantQueued int
		wantDrop   map[string]float64 // reason -> count
	}{
		{
			name:       "single enqueue queues one",
			opts:       Options{Name: "metrics_async_test_enqueue_single"},
			register:   true,
			enqueues:   []Key{{Kind: "sandbox", Namespace: "ns", Name: "a"}},
			wantQueued: 1,
		},
		{
			name:     "duplicate keys collapse",
			opts:     Options{Name: "metrics_async_test_enqueue_dedup"},
			register: true,
			enqueues: []Key{
				{Kind: "sandbox", Namespace: "ns", Name: "a"},
				{Kind: "sandbox", Namespace: "ns", Name: "a"},
				{Kind: "sandbox", Namespace: "ns", Name: "a"},
			},
			wantQueued: 1,
		},
		{
			name:       "unregistered kind dropped",
			opts:       Options{Name: "metrics_async_test_enqueue_unreg"},
			register:   false,
			enqueues:   []Key{{Kind: "sandbox", Namespace: "ns", Name: "a"}},
			wantQueued: 0,
			wantDrop:   map[string]float64{"unregistered": 1},
		},
		{
			name:     "queue_full drops past cap",
			opts:     Options{Name: "metrics_async_test_enqueue_cap", QueueCap: 1},
			register: true,
			enqueues: []Key{
				{Kind: "sandbox", Namespace: "ns", Name: "a"},
				{Kind: "sandbox", Namespace: "ns", Name: "b"},
				{Kind: "sandbox", Namespace: "ns", Name: "c"},
			},
			wantQueued: 1,
			wantDrop:   map[string]float64{"queue_full": 2},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewPool(tt.opts)
			if tt.register {
				assert.NoError(t, p.RegisterKind("sandbox", func(string, string) {}))
			}
			for _, k := range tt.enqueues {
				p.Enqueue(k.Kind, k.Namespace, k.Name)
			}
			assert.Equal(t, tt.wantQueued, p.queue.Len())
			for reason, want := range tt.wantDrop {
				got := testCounter(t, p.col.dropped, "sandbox", reason)
				assert.Equal(t, want, got, "drop reason %s", reason)
			}
		})
	}
}

func TestPool_SameKey_NeverConcurrent(t *testing.T) {
	p := NewPool(Options{Name: "metrics_async_test_serial", Workers: 4, DrainTimeout: time.Second})

	const distinctKeys = 8
	const enqueuePerKey = 50
	var inflight [distinctKeys]int32
	var maxObserved int32
	fn := func(_, name string) {
		idx := int(name[1] - '0')
		now := atomic.AddInt32(&inflight[idx], 1)
		// snapshot peak per key
		for {
			cur := atomic.LoadInt32(&maxObserved)
			if now <= cur || atomic.CompareAndSwapInt32(&maxObserved, cur, now) {
				break
			}
		}
		// Hold long enough that a concurrent processor would race in.
		time.Sleep(2 * time.Millisecond)
		atomic.AddInt32(&inflight[idx], -1)
	}
	assert.NoError(t, p.RegisterKind("sandbox", fn))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Start(ctx) }()

	for i := 0; i < distinctKeys; i++ {
		for j := 0; j < enqueuePerKey; j++ {
			p.Enqueue("sandbox", "ns", fmt.Sprintf("n%d", i))
		}
	}

	// Each distinct key should be processed at least once; dedup means total <= distinctKeys * enqueuePerKey.
	assert.Eventually(t, func() bool {
		return testCounter(t, p.col.processedTotal, "sandbox", "ok") >= distinctKeys
	}, 3*time.Second, 5*time.Millisecond)

	cancel()
	assert.NoError(t, <-done)

	assert.LessOrEqual(t, atomic.LoadInt32(&maxObserved), int32(1),
		"workqueue must serialize processing of identical keys")
}

// testCounter reads a CounterVec child value via Write to a dto.Metric.
func testCounter(t *testing.T, vec *prometheus.CounterVec, lvs ...string) float64 {
	t.Helper()
	m, err := vec.GetMetricWithLabelValues(lvs...)
	assert.NoError(t, err)
	var d dto.Metric
	assert.NoError(t, m.(prometheus.Metric).Write(&d))
	return d.GetCounter().GetValue()
}

func TestPool_Start_DrainTimeout(t *testing.T) {
	p := NewPool(Options{Name: "metrics_async_test_drain", Workers: 1, DrainTimeout: 50 * time.Millisecond})

	block := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(block)
		}
	}()
	fn := func(string, string) {
		<-block // never returns until released
	}
	assert.NoError(t, p.RegisterKind("sandbox", fn))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Start(ctx) }()

	// Enqueue two items; the first will block the only worker, the
	// second remains in the queue at shutdown.
	p.Enqueue("sandbox", "ns", "blocker")
	p.Enqueue("sandbox", "ns", "queued")

	// Give the worker a moment to pick up the first.
	time.Sleep(20 * time.Millisecond)

	cancel()
	startWait := time.Now()
	assert.NoError(t, <-done)
	elapsed := time.Since(startWait)
	assert.GreaterOrEqual(t, elapsed, 50*time.Millisecond)
	assert.Less(t, elapsed, 500*time.Millisecond, "Start must return shortly after DrainTimeout")

	// The queued item is reported as drained-out.
	assert.GreaterOrEqual(t, testCounter(t, p.col.dropped, "", "drain_timeout"), float64(1))

	close(block)
	released = true
}

func TestPool_Start_processesAndRecovers(t *testing.T) {
	tests := []struct {
		name      string
		fn        CleanupFunc
		enqueues  int
		distinct  int
		wantOK    float64
		wantPanic float64
	}{
		{
			name:     "ok path counts processed",
			fn:       func(string, string) {},
			enqueues: 5,
			distinct: 5,
			wantOK:   5,
		},
		{
			name:      "panic recorded as panic, worker survives",
			fn:        func(string, string) { panic("boom") },
			enqueues:  3,
			distinct:  3,
			wantPanic: 3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewPool(Options{Name: "metrics_async_test_start_" + tt.name, Workers: 2, DrainTimeout: time.Second})
			assert.NoError(t, p.RegisterKind("sandbox", tt.fn))

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- p.Start(ctx) }()

			for i := 0; i < tt.enqueues; i++ {
				p.Enqueue("sandbox", "ns", fmt.Sprintf("n%d", i))
			}
			// Wait until processed.
			assert.Eventually(t, func() bool {
				return testCounter(t, p.col.processedTotal, "sandbox", "ok")+
					testCounter(t, p.col.processedTotal, "sandbox", "panic") >= float64(tt.distinct)
			}, 2*time.Second, 5*time.Millisecond)

			cancel()
			assert.NoError(t, <-done)

			if tt.wantOK > 0 {
				assert.Equal(t, tt.wantOK, testCounter(t, p.col.processedTotal, "sandbox", "ok"))
			}
			if tt.wantPanic > 0 {
				assert.Equal(t, tt.wantPanic, testCounter(t, p.col.processedTotal, "sandbox", "panic"))
			}
		})
	}
}
