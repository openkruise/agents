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

package runnable

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// freePort returns an OS-assigned free TCP port. The listener is closed
// immediately, leaving a brief race window — acceptable for tests.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// TestGRPCServer_StartsAndStopsViaContext starts the runnable in a goroutine
// and cancels the context to drive a graceful shutdown.
func TestGRPCServer_StartsAndStopsViaContext(t *testing.T) {
	srv := grpc.NewServer()
	r := GRPCServer("test", srv, freePort(t))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	// Give the server time to bind, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expected nil from runnable on graceful stop, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runnable did not exit within 2s after cancel")
	}
}

// TestGRPCServer_ListenError exercises the listener-failure branch by
// pre-binding a port on all interfaces (matching the runnable's wildcard
// listen) and asking the runnable to bind the same one.
func TestGRPCServer_ListenError(t *testing.T) {
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("pre-bind failed: %v", err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port

	r := GRPCServer("conflict", grpc.NewServer(), port)
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { done <- r.Start(ctx) }()

	select {
	case err := <-done:
		if err == nil {
			t.Error("expected listen error when the port is already bound")
		}
	case <-ctx.Done():
		t.Fatal("runnable did not return within 2s on bind conflict")
	}
}

func TestLeaderElectionWrappers(t *testing.T) {
	noop := manager.RunnableFunc(func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	})

	if le, ok := RequireLeaderElection(noop).(*leaderElection); !ok || !le.NeedLeaderElection() {
		t.Errorf("RequireLeaderElection should produce a runnable that requires election")
	}
	if le, ok := NoLeaderElection(noop).(*leaderElection); !ok || le.NeedLeaderElection() {
		t.Errorf("NoLeaderElection should produce a runnable that does NOT require election")
	}
}
