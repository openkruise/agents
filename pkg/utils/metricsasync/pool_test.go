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
	"testing"
	"time"

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
