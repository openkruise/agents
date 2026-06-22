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

package server

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/openkruise/agents/pkg/traffic-extension/framework/configstore"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func TestNewDefaultExtProcServerRunner_Defaults(t *testing.T) {
	r := NewDefaultExtProcServerRunner(9999, true)
	if r.GrpcPort != 9999 {
		t.Errorf("expected port 9999, got %d", r.GrpcPort)
	}
	if !r.SecureServing {
		t.Error("expected SecureServing=true by default")
	}
	if !r.Streaming {
		t.Error("expected Streaming=true to match arg")
	}
}

// TestAsRunnable_PlainServingStartsAndStops covers the non-TLS branch end to
// end (no cert generation cost) and the cancel-driven shutdown.
func TestAsRunnable_PlainServingStartsAndStops(t *testing.T) {
	r := &ExtProcServerRunner{
		GrpcPort:      freePort(t),
		SecureServing: false,
		Streaming:     false,
		ConfigStore:   configstore.NewStore(),
	}

	rn := r.AsRunnable(logr.Discard())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rn.Start(ctx) }()

	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expected nil on graceful shutdown, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runnable did not stop within 2s")
	}
}
