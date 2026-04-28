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

package peers

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stretchr/testify/assert"
)

// mockPeers implements Peers interface for testing.
type mockPeers struct {
	reconcileFunc func(ctx context.Context, peers []string) error
}

func (m *mockPeers) Start(_ context.Context, _ string, _ int, _ []string) error {
	return nil
}

func (m *mockPeers) Stop() error { return nil }

func (m *mockPeers) GetPeers() []Peer { return nil }

func (m *mockPeers) GetAllMembers() []Peer { return nil }

func (m *mockPeers) WaitForPeers(_ context.Context, _ int) error { return nil }

func (m *mockPeers) LocalAddr() net.IP { return nil }

func (m *mockPeers) LocalPort() int { return 0 }

func (m *mockPeers) ReconcilePeers(ctx context.Context, peers []string) error {
	if m.reconcileFunc != nil {
		return m.reconcileFunc(ctx, peers)
	}
	return nil
}

// TestRunPeerReconciliation_EmptyNamespace verifies that an empty namespace causes
// an immediate return without starting the ticker loop.
func TestRunPeerReconciliation_EmptyNamespace(t *testing.T) {
	RunPeerReconciliation(context.Background(), nil, &mockPeers{}, "", "x=y", "10.0.0.1", 7946)
	// If we reach here without hanging, the early return works.
}

// TestRunPeerReconciliation_EmptyLabelSelector verifies that an empty labelSelector
// causes an immediate return.
func TestRunPeerReconciliation_EmptyLabelSelector(t *testing.T) {
	RunPeerReconciliation(context.Background(), nil, &mockPeers{}, "ns", "", "10.0.0.1", 7946)
}

// TestRunPeerReconciliation_CancelledContext verifies that a cancelled context
// causes an immediate return from the select loop.
func TestRunPeerReconciliation_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	RunPeerReconciliation(ctx, nil, &mockPeers{}, "ns", "x=y", "10.0.0.1", 7946)
}

// TestRunPeerReconciliation_FullFlow verifies the reconciliation flow:
// - Self IP, loopback IP, and empty PodIP are filtered out
// - Valid peer IPs are formatted as ip:port and passed to ReconcilePeers
// - A short test interval is used to avoid waiting 60s
func TestRunPeerReconciliation_FullFlow(t *testing.T) {
	// Override the reconcile interval for fast testing
	oldInterval := reconcileInterval
	reconcileInterval = 10 * time.Millisecond
	t.Cleanup(func() { reconcileInterval = oldInterval })

	localIP := "10.0.0.1"
	bindPort := 7946

	// Set up fake K8s client with a mix of pods.
	// Pods must carry the label that matches the selector used in RunPeerReconciliation.
	labels := map[string]string{"app": "peer"}
	fakeClient := fake.NewSimpleClientset()
	pods := []*corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "self", Labels: labels}, Status: corev1.PodStatus{PodIP: localIP}},
		{ObjectMeta: metav1.ObjectMeta{Name: "peer-a", Labels: labels}, Status: corev1.PodStatus{PodIP: "10.0.0.2"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "peer-b", Labels: labels}, Status: corev1.PodStatus{PodIP: "10.0.0.3"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "loopback", Labels: labels}, Status: corev1.PodStatus{PodIP: "127.0.0.1"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "no-ip", Labels: labels}, Status: corev1.PodStatus{PodIP: ""}},
	}
	for _, pod := range pods {
		_, err := fakeClient.CoreV1().Pods("ns").Create(context.Background(), pod, metav1.CreateOptions{})
		assert.NoError(t, err)
	}

	// Track what ReconcilePeers receives
	var (
		mu           sync.Mutex
		gotAddresses []string
	)
	reconciled := make(chan struct{})

	mock := &mockPeers{
		reconcileFunc: func(_ context.Context, peers []string) error {
			mu.Lock()
			gotAddresses = peers
			mu.Unlock()
			// Signal first reconciliation via channel (safe to close once)
			select {
			case <-reconciled:
			default:
				close(reconciled)
			}
			return nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go RunPeerReconciliation(ctx, fakeClient, mock, "ns", "app=peer", localIP, bindPort)

	// Wait for first tick to fire (10ms ticker), with timeout guard
	select {
	case <-reconciled:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for first reconciliation")
	}

	mu.Lock()
	defer mu.Unlock()

	// Should not include self
	assert.NotContains(t, gotAddresses, "10.0.0.1:7946")
	// Should not include loopback
	assert.NotContains(t, gotAddresses, "127.0.0.1:7946")
	// Should not include empty-ip pod
	assert.NotContains(t, gotAddresses, ":7946")
	// Should include valid peers
	assert.Contains(t, gotAddresses, "10.0.0.2:7946")
	assert.Contains(t, gotAddresses, "10.0.0.3:7946")
	// Should have exactly 2 valid peers
	assert.Len(t, gotAddresses, 2)
}
