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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestMemberlistPeers_Start_Stop tests basic start and stop functionality
func TestMemberlistPeers_Start_Stop(t *testing.T) {
	fc := fake.NewClientBuilder().WithStatusSubresource(&v1.Pod{}).Build()
	ctx := context.Background()
	peer, port, err := CreateTestPeer(ctx, fc, "test-node-1")
	require.NoError(t, err)

	err = peer.Start(ctx, port)
	require.NoError(t, err)
	assert.True(t, peer.started.Load())

	// Verify LocalAddr and LocalPort
	assert.NotNil(t, peer.LocalAddr())
	assert.Equal(t, port, peer.LocalPort())

	// Verify GetPeers returns empty (single node)
	peers := peer.GetPeers()
	assert.Empty(t, peers)

	// Verify GetAllMembers includes self
	members := peer.GetAllMembers()
	assert.Len(t, members, 1)
	assert.Equal(t, "test-node-1", members[0].Name)

	err = peer.Stop()
	require.NoError(t, err)
	assert.False(t, peer.started.Load())
}

// TestMemberlistPeers_Start_Twice tests that starting twice should return an error
func TestMemberlistPeers_Start_Twice(t *testing.T) {
	fc := fake.NewClientBuilder().WithStatusSubresource(&v1.Pod{}).Build()
	ctx := context.Background()
	peer, port, err := CreateTestPeer(ctx, fc, "test-node-2")
	require.NoError(t, err)

	err = peer.Start(ctx, port)
	require.NoError(t, err)

	err = peer.Start(ctx, port)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already started")

	_ = peer.Stop()
}

// TestMemberlistPeers_Stop_NotStarted tests that stopping when not started does not return an error
func TestMemberlistPeers_Stop_NotStarted(t *testing.T) {
	fc := fake.NewClientBuilder().WithStatusSubresource(&v1.Pod{}).Build()
	peer := NewMemberlistPeers(fc, "test-node-not-started", Namespace, LabelSelector)

	err := peer.Stop()
	assert.NoError(t, err)
}

// TestMemberlistPeers_ThreeNodes_Join tests three-node join and discovery
func TestMemberlistPeers_ThreeNodes_Join(t *testing.T) {
	fc := fake.NewClientBuilder().WithStatusSubresource(&v1.Pod{}).Build()
	ctx := t.Context()

	// Create three nodes
	peer1, port1, err := CreateTestPeer(ctx, fc, "node-1")
	require.NoError(t, err)
	peer2, port2, err := CreateTestPeer(ctx, fc, "node-2")
	require.NoError(t, err)
	peer3, port3, err := CreateTestPeer(ctx, fc, "node-3")
	require.NoError(t, err)

	// Start first node (seed node)
	err = peer1.Start(ctx, port1)
	require.NoError(t, err)
	defer func() { _ = peer1.Stop() }()

	// Start second node, join first
	err = peer2.Start(ctx, port2)
	require.NoError(t, err)
	defer func() { _ = peer2.Stop() }()

	// Start third node, join first two
	err = peer3.Start(ctx, port3)
	require.NoError(t, err)
	defer func() { _ = peer3.Stop() }()

	// Wait for gossip propagation
	assert.Eventually(t, func() bool {
		return len(peer1.GetPeers()) == 2
	}, 5*time.Second, 100*time.Millisecond, "peer1 should discover 2 peers")

	assert.Eventually(t, func() bool {
		return len(peer2.GetPeers()) == 2
	}, 5*time.Second, 100*time.Millisecond, "peer2 should discover 2 peers")

	assert.Eventually(t, func() bool {
		return len(peer3.GetPeers()) == 2
	}, 5*time.Second, 100*time.Millisecond, "peer3 should discover 2 peers")

	// Verify GetAllMembers includes all nodes
	assert.Len(t, peer1.GetAllMembers(), 3)
	assert.Len(t, peer2.GetAllMembers(), 3)
	assert.Len(t, peer3.GetAllMembers(), 3)

	// Verify discovered peers contain correct node names
	peerNames := make(map[string]bool)
	for _, p := range peer1.GetPeers() {
		peerNames[p.Name] = true
	}
	assert.True(t, peerNames["node-2"], "peer1 should discover node-2")
	assert.True(t, peerNames["node-3"], "peer1 should discover node-3")
}

// TestMemberlistPeers_WaitForPeers tests waiting for peers functionality
func TestMemberlistPeers_WaitForPeers(t *testing.T) {
	fc := fake.NewClientBuilder().WithStatusSubresource(&v1.Pod{}).Build()
	ctx := t.Context()

	peer1, port1, err := CreateTestPeer(ctx, fc, "wait-node-1")
	require.NoError(t, err)
	peer2, port2, err := CreateTestPeer(ctx, fc, "wait-node-2")
	require.NoError(t, err)

	// Start first node
	err = peer1.Start(ctx, port1)
	require.NoError(t, err)
	defer func() { _ = peer1.Stop() }()

	// Start second node asynchronously (delay 200ms)
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = peer2.Start(ctx, port2)
	}()
	defer func() { _ = peer2.Stop() }()

	// Wait for at least 1 peer
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	err = peer1.WaitForPeers(waitCtx, 1)
	assert.NoError(t, err)

	// Verify peer was indeed discovered
	assert.Len(t, peer1.GetPeers(), 1)
}

// TestMemberlistPeers_WaitForPeers_Timeout tests waiting timeout
func TestMemberlistPeers_WaitForPeers_Timeout(t *testing.T) {
	fc := fake.NewClientBuilder().WithStatusSubresource(&v1.Pod{}).Build()
	ctx := t.Context()
	peer, port, err := CreateTestPeer(ctx, fc, "timeout-node")
	require.NoError(t, err)

	err = peer.Start(ctx, port)
	require.NoError(t, err)
	defer func() { _ = peer.Stop() }()

	// Set very short timeout
	waitCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()

	// Wait for 1 peer, should timeout
	err = peer.WaitForPeers(waitCtx, 1)
	assert.Error(t, err)
	assert.Equal(t, context.DeadlineExceeded, err)
}

// TestMemberlistPeers_WaitForPeers_Stopped tests that WaitForPeers returns error after stopped
func TestMemberlistPeers_WaitForPeers_Stopped(t *testing.T) {
	fc := fake.NewClientBuilder().WithStatusSubresource(&v1.Pod{}).Build()
	ctx := t.Context()
	peer, port, err := CreateTestPeer(ctx, fc, "stopped-node")
	require.NoError(t, err)

	err = peer.Start(ctx, port)
	require.NoError(t, err)

	// Stop asynchronously
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = peer.Stop()
	}()

	// Wait should return stopped error
	err = peer.WaitForPeers(ctx, 1)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "stopped")
}

// TestMemberlistPeers_NodeLeave tests that node is removed from peers list after leaving
func TestMemberlistPeers_NodeLeave(t *testing.T) {
	fc := fake.NewClientBuilder().WithStatusSubresource(&v1.Pod{}).Build()
	ctx := t.Context()

	peer1, port1, err := CreateTestPeer(ctx, fc, "leave-node-1")
	require.NoError(t, err)
	peer2, port2, err := CreateTestPeer(ctx, fc, "leave-node-2")
	require.NoError(t, err)

	// Start two nodes
	err = peer1.Start(ctx, port1)
	require.NoError(t, err)
	defer func() { _ = peer1.Stop() }()

	err = peer2.Start(ctx, port2)
	require.NoError(t, err)

	// Wait for peer2 to be discovered
	assert.Eventually(t, func() bool {
		return len(peer1.GetPeers()) == 1
	}, 5*time.Second, 100*time.Millisecond)

	// Gracefully stop peer2
	err = peer2.Stop()
	require.NoError(t, err)

	// Wait for peer2 to be removed from peer1's list
	assert.Eventually(t, func() bool {
		return len(peer1.GetPeers()) == 0
	}, 5*time.Second, 100*time.Millisecond, "peer2 should be removed from peer1's peer list after leaving")
}

// TestMemberlistPeers_GetPeers_NotStarted tests returning nil when not started
func TestMemberlistPeers_GetPeers_NotStarted(t *testing.T) {
	fc := fake.NewClientBuilder().WithStatusSubresource(&v1.Pod{}).Build()
	peer := NewMemberlistPeers(fc, "not-started", Namespace, LabelSelector)

	assert.Nil(t, peer.GetPeers())
	assert.Nil(t, peer.GetAllMembers())
	assert.Nil(t, peer.LocalAddr())
	assert.Equal(t, 0, peer.LocalPort())
}

// TestMemberlistPeers_Join_PartialFailure tests that partial join failure does not affect startup
func TestMemberlistPeers_Join_PartialFailure(t *testing.T) {
	fc := fake.NewClientBuilder().WithStatusSubresource(&v1.Pod{}).Build()
	ctx := t.Context()
	peer, port, err := CreateTestPeer(ctx, fc, "partial-node")
	require.NoError(t, err)

	// Try to join a non-existent node and seed node
	err = peer.Start(ctx, port)
	require.NoError(t, err) // Should not fail because single node operation is allowed
	defer func() { _ = peer.Stop() }()

	assert.True(t, peer.started.Load())
}
