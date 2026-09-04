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
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/memberlist"
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func interceptList(list func(context.Context, ctrlclient.ObjectList, ...ctrlclient.ListOption) error) ctrlclient.Client {
	return interceptor.NewClient(fake.NewClientBuilder().Build(), interceptor.Funcs{
		List: func(ctx context.Context, _ ctrlclient.WithWatch, obj ctrlclient.ObjectList, opts ...ctrlclient.ListOption) error {
			return list(ctx, obj, opts...)
		},
	})
}

// fakeMemberlistHandle records every memberlist call in order; counts and
// ordering assertions both derive from recordedCalls.
type fakeMemberlistHandle struct {
	join     func([]string) (int, error)
	leaveErr error
	mu       sync.Mutex
	calls    []string
}

func (f *fakeMemberlistHandle) Join(seeds []string) (int, error) {
	f.record("join:" + strings.Join(seeds, ","))
	if f.join != nil {
		return f.join(seeds)
	}
	return 0, nil
}

func (f *fakeMemberlistHandle) Leave(time.Duration) error {
	f.record("leave")
	return f.leaveErr
}

func (f *fakeMemberlistHandle) Shutdown() error {
	f.record("shutdown")
	return nil
}

func (*fakeMemberlistHandle) Members() []*memberlist.Node { return nil }

func (*fakeMemberlistHandle) LocalNode() *memberlist.Node {
	return &memberlist.Node{Addr: net.ParseIP("127.0.0.1")}
}

func (f *fakeMemberlistHandle) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeMemberlistHandle) recordedCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeMemberlistHandle) callCount(call string) int {
	count := 0
	for _, recorded := range f.recordedCalls() {
		if recorded == call {
			count++
		}
	}
	return count
}

// startFakeLifecycle wires handle through the same startLifecycle tail that
// Start uses, so these tests exercise the production lifecycle wiring.
func startFakeLifecycle(ctx context.Context, reader ctrlclient.Reader, handle memberlistHandle, retry time.Duration) *MemberlistPeers {
	peer := NewMemberlistPeers(reader, "test-node", Namespace, LabelSelector)
	peer.retryInterval = retry
	peer.startLifecycle(ctx, handle, labels.SelectorFromSet(labels.Set{LabelSelectorKey: LabelSelectorValue}), 7946, "127.0.0.1:7946")
	return peer
}

// TestMemberlistPeers_Start_Stop tests basic start and stop functionality
func TestMemberlistPeers_Start_Stop(t *testing.T) {
	fc := fake.NewClientBuilder().WithStatusSubresource(&v1.Pod{}).Build()
	ctx := context.Background()
	peer, port, err := CreateTestPeer(ctx, fc, "test-node-1")
	require.NoError(t, err)

	err = peer.Start(ctx, "127.0.0.1", port)
	require.NoError(t, err)
	assert.NotNil(t, peer.list)

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

	err = peer.Stop(ctx)
	require.NoError(t, err)
}

func TestMemberlistPeers_StartDoesNotWaitForDiscovery(t *testing.T) {
	t.Setenv("POD_IP", "127.0.0.1")
	listStarted := make(chan struct{})
	reader := interceptList(func(ctx context.Context, _ ctrlclient.ObjectList, _ ...ctrlclient.ListOption) error {
		close(listStarted)
		<-ctx.Done()
		return ctx.Err()
	})
	port, err := getFreePort()
	require.NoError(t, err)
	parent, cancel := context.WithCancel(t.Context())
	peer := NewMemberlistPeers(reader, "non-blocking-node", Namespace, LabelSelector)

	require.NoError(t, peer.Start(parent, "", port))
	assert.Equal(t, "127.0.0.1", peer.LocalAddr().String())
	select {
	case <-listStarted:
	case <-time.After(time.Second):
		t.Fatal("background discovery did not start")
	}
	cancel()
	require.NoError(t, peer.Stop(t.Context()))
}

// TestMemberlistPeers_Stop_NotStarted tests that stopping when not started does not return an error
func TestMemberlistPeers_Stop_NotStarted(t *testing.T) {
	fc := fake.NewClientBuilder().WithStatusSubresource(&v1.Pod{}).Build()
	peer := NewMemberlistPeers(fc, "test-node-not-started", Namespace, LabelSelector)

	assert.NoError(t, peer.Stop(t.Context()))
}

// TestMemberlistPeers_Start_Twice tests that a second Start is rejected
// instead of replacing the running memberlist.
func TestMemberlistPeers_Start_Twice(t *testing.T) {
	fc := fake.NewClientBuilder().WithStatusSubresource(&v1.Pod{}).Build()
	ctx := t.Context()
	peer, port, err := CreateTestPeer(ctx, fc, "test-node-2")
	require.NoError(t, err)

	require.NoError(t, peer.Start(ctx, "127.0.0.1", port))
	first := peer.list

	assert.ErrorContains(t, peer.Start(ctx, "127.0.0.1", port), "already started")
	assert.Same(t, first, peer.list)
	require.NoError(t, peer.Stop(ctx))
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
	err = peer1.Start(ctx, "127.0.0.1", port1)
	require.NoError(t, err)
	defer func() { _ = peer1.Stop(ctx) }()

	// Start second node, join first
	err = peer2.Start(ctx, "127.0.0.1", port2)
	require.NoError(t, err)
	defer func() { _ = peer2.Stop(ctx) }()

	// Start third node, join first two
	err = peer3.Start(ctx, "127.0.0.1", port3)
	require.NoError(t, err)
	defer func() { _ = peer3.Stop(ctx) }()

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

// TestMemberlistPeers_NodeLeave tests that node is removed from peers list after leaving
func TestMemberlistPeers_NodeLeave(t *testing.T) {
	fc := fake.NewClientBuilder().WithStatusSubresource(&v1.Pod{}).Build()
	ctx := t.Context()

	peer1, port1, err := CreateTestPeer(ctx, fc, "leave-node-1")
	require.NoError(t, err)
	peer2, port2, err := CreateTestPeer(ctx, fc, "leave-node-2")
	require.NoError(t, err)

	// Start two nodes
	err = peer1.Start(ctx, "127.0.0.1", port1)
	require.NoError(t, err)
	defer func() { _ = peer1.Stop(ctx) }()

	err = peer2.Start(ctx, "127.0.0.1", port2)
	require.NoError(t, err)

	// Wait for peer2 to be discovered
	assert.Eventually(t, func() bool {
		return len(peer1.GetPeers()) == 1
	}, 5*time.Second, 100*time.Millisecond)

	// Gracefully stop peer2
	err = peer2.Stop(ctx)
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

// TestMemberlistPeers_GettersRaceWithStart fails under -race when a getter
// reads list without waiting on the started barrier.
func TestMemberlistPeers_GettersRaceWithStart(t *testing.T) {
	fc := fake.NewClientBuilder().WithStatusSubresource(&v1.Pod{}).Build()
	peer, port, err := CreateTestPeer(t.Context(), fc, "race-node")
	require.NoError(t, err)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = peer.GetPeers()
					_ = peer.GetAllMembers()
					_ = peer.LocalAddr()
					_ = peer.LocalPort()
				}
			}
		}()
	}

	require.NoError(t, peer.Start(t.Context(), "127.0.0.1", port))
	close(stop)
	wg.Wait()
	require.NoError(t, peer.Stop(t.Context()))
}

// TestMemberlistPeers_Join_PartialFailure tests that partial join failure does not affect startup
func TestMemberlistPeers_Join_PartialFailure(t *testing.T) {
	fc := fake.NewClientBuilder().WithStatusSubresource(&v1.Pod{}).Build()
	ctx := t.Context()
	peer, port, err := CreateTestPeer(ctx, fc, "partial-node")
	require.NoError(t, err)

	// Try to join a non-existent node and seed node
	err = peer.Start(ctx, "127.0.0.1", port)
	require.NoError(t, err) // Should not fail because single node operation is allowed
	defer func() { _ = peer.Stop(ctx) }()

	assert.NotNil(t, peer.list)
}

func TestTrustedSeedAddresses(t *testing.T) {
	pod := func(ip, annotation string, phase v1.PodPhase) v1.Pod {
		annotations := map[string]string{}
		if annotation != "" {
			annotations[agentsv1alpha1.AnnotationMemberlistURL] = annotation
		}
		return v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: annotations},
			Status:     v1.PodStatus{PodIP: ip, Phase: phase},
		}
	}
	tests := []struct {
		name string
		pods []v1.Pod
		want []string
	}{
		{name: "pod ip with the bind port", pods: []v1.Pod{pod("10.0.0.3", "", v1.PodRunning)}, want: []string{"10.0.0.3:7946"}},
		{name: "annotation overrides only the port", pods: []v1.Pod{pod("10.0.0.2", "10.0.0.2:9000", v1.PodRunning)}, want: []string{"10.0.0.2:9000"}},
		{name: "annotation redirecting to another ip is dropped", pods: []v1.Pod{pod("10.0.0.4", "10.0.0.99:9000", v1.PodRunning)}, want: []string{}},
		{name: "annotation with a hostname is dropped", pods: []v1.Pod{pod("10.0.0.6", "evil.example:9000", v1.PodRunning)}, want: []string{}},
		{name: "annotation with an invalid port is dropped", pods: []v1.Pod{pod("10.0.0.5", "10.0.0.5:70000", v1.PodRunning)}, want: []string{}},
		{name: "invalid pod ip is skipped", pods: []v1.Pod{pod("not-an-ip", "", v1.PodRunning)}, want: []string{}},
		{name: "local address is excluded", pods: []v1.Pod{pod("127.0.0.1", "127.0.0.1:7946", v1.PodRunning)}, want: []string{}},
		{name: "pending and unready pods stay eligible", pods: []v1.Pod{pod("10.0.0.7", "", v1.PodPending), pod("10.0.0.8", "", "")}, want: []string{"10.0.0.7:7946", "10.0.0.8:7946"}},
		{name: "terminal pods are excluded", pods: []v1.Pod{pod("10.0.0.9", "", v1.PodSucceeded), pod("10.0.0.10", "", v1.PodFailed)}, want: []string{}},
		{name: "duplicates collapse", pods: []v1.Pod{pod("10.0.0.3", "", v1.PodRunning), pod("10.0.0.3", "", v1.PodRunning)}, want: []string{"10.0.0.3:7946"}},
		{name: "seeds are sorted", pods: []v1.Pod{pod("10.0.0.3", "", v1.PodRunning), pod("10.0.0.2", "10.0.0.2:9000", v1.PodRunning)}, want: []string{"10.0.0.2:9000", "10.0.0.3:7946"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, trustedSeedAddresses(tt.pods, "127.0.0.1:7946", 7946))
		})
	}
}

// The fake client itself filters by namespace and label selector, so the
// joined seeds prove the discovery list carries both.
func TestMemberlistPeers_SeedListUsesNamespaceAndSelector(t *testing.T) {
	matching := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "matching", Namespace: Namespace, Labels: map[string]string{LabelSelectorKey: LabelSelectorValue}},
		Status:     v1.PodStatus{PodIP: "10.0.0.2"},
	}
	otherNamespace := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "other-ns", Namespace: "other", Labels: map[string]string{LabelSelectorKey: LabelSelectorValue}},
		Status:     v1.PodStatus{PodIP: "10.0.0.3"},
	}
	otherLabel := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "other-label", Namespace: Namespace, Labels: map[string]string{"app": "other"}},
		Status:     v1.PodStatus{PodIP: "10.0.0.4"},
	}
	fc := fake.NewClientBuilder().WithStatusSubresource(&v1.Pod{}).WithObjects(matching, otherNamespace, otherLabel).Build()
	handle := &fakeMemberlistHandle{join: func([]string) (int, error) { return 1, nil }}
	peer := startFakeLifecycle(t.Context(), fc, handle, time.Hour)

	require.Eventually(t, func() bool {
		return len(handle.recordedCalls()) > 0
	}, time.Second, time.Millisecond)
	require.NoError(t, peer.Stop(t.Context()))
	assert.Equal(t, []string{"join:10.0.0.2:7946", "leave", "shutdown"}, handle.recordedCalls())
}

func TestMemberlistPeers_RetriesUntilJoinSucceeds(t *testing.T) {
	var listCalls atomic.Int32
	reader := interceptList(func(_ context.Context, list ctrlclient.ObjectList, _ ...ctrlclient.ListOption) error {
		call := listCalls.Add(1)
		if call == 1 {
			return assert.AnError
		}
		pods := list.(*v1.PodList)
		if call > 2 {
			pods.Items = []v1.Pod{{Status: v1.PodStatus{PodIP: "10.0.0.2"}}}
		}
		return nil
	})
	var joinCalls atomic.Int32
	handle := &fakeMemberlistHandle{join: func([]string) (int, error) {
		if joinCalls.Add(1) == 1 {
			return 0, assert.AnError
		}
		return 1, nil
	}}
	peer := startFakeLifecycle(t.Context(), reader, handle, 5*time.Millisecond)

	require.Eventually(t, func() bool { return listCalls.Load() >= 4 }, time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return joinCalls.Load() == 2 }, time.Second, time.Millisecond)
	listCount := listCalls.Load()
	assert.Never(t, func() bool { return listCalls.Load() != listCount }, 20*time.Millisecond, time.Millisecond,
		"successful join must stop Kubernetes discovery")
	require.NoError(t, peer.Stop(t.Context()))
	assert.Equal(t, []string{"join:10.0.0.2:7946", "join:10.0.0.2:7946", "leave", "shutdown"}, handle.recordedCalls())
}

func TestMemberlistPeers_JoinsSeedsIndividuallyInStableOrder(t *testing.T) {
	reader := interceptList(func(_ context.Context, list ctrlclient.ObjectList, _ ...ctrlclient.ListOption) error {
		list.(*v1.PodList).Items = []v1.Pod{
			{Status: v1.PodStatus{PodIP: "10.0.0.3"}},
			{Status: v1.PodStatus{PodIP: "10.0.0.2"}},
		}
		return nil
	})
	var joinCalls atomic.Int32
	handle := &fakeMemberlistHandle{join: func(seeds []string) (int, error) {
		if len(seeds) != 1 {
			t.Errorf("Join seeds = %v, want exactly one seed", seeds)
			return 0, assert.AnError
		}
		if joinCalls.Add(1) == 1 {
			return 0, assert.AnError
		}
		return 1, nil
	}}
	peer := startFakeLifecycle(t.Context(), reader, handle, time.Hour)

	require.Eventually(t, func() bool { return joinCalls.Load() == 2 }, time.Second, time.Millisecond)
	require.NoError(t, peer.Stop(t.Context()))
	assert.Equal(t, []string{"join:10.0.0.2:7946", "join:10.0.0.3:7946", "leave", "shutdown"}, handle.recordedCalls())
}

// startBlockedJoin starts a peer whose first Join call blocks until the
// returned release function runs.
func startBlockedJoin(t *testing.T) (*MemberlistPeers, *fakeMemberlistHandle, func()) {
	t.Helper()
	joinStarted := make(chan struct{})
	releaseJoin := make(chan struct{})
	reader := interceptList(func(_ context.Context, list ctrlclient.ObjectList, _ ...ctrlclient.ListOption) error {
		list.(*v1.PodList).Items = []v1.Pod{{Status: v1.PodStatus{PodIP: "10.0.0.2"}}}
		return nil
	})
	handle := &fakeMemberlistHandle{join: func([]string) (int, error) {
		close(joinStarted)
		<-releaseJoin
		return 0, nil
	}}
	peer := startFakeLifecycle(t.Context(), reader, handle, time.Hour)
	<-joinStarted
	return peer, handle, func() { close(releaseJoin) }
}

func TestMemberlistPeers_StopWaitsForActiveJoin(t *testing.T) {
	peer, handle, release := startBlockedJoin(t)

	stopResult := make(chan error, 1)
	go func() { stopResult <- peer.Stop(t.Context()) }()
	assert.Never(t, func() bool { return handle.callCount("leave") != 0 }, 20*time.Millisecond, time.Millisecond)
	release()
	require.NoError(t, <-stopResult)
	assert.Equal(t, []string{"join:10.0.0.2:7946", "leave", "shutdown"}, handle.recordedCalls())
}

func TestMemberlistPeers_StopDeadlineDoesNotTakeCleanupOwnership(t *testing.T) {
	peer, handle, release := startBlockedJoin(t)

	waitCtx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	assert.ErrorIs(t, peer.Stop(waitCtx), context.DeadlineExceeded)
	assert.Zero(t, handle.callCount("leave"))
	release()
	select {
	case err := <-peer.lifecycleDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("lifecycle cleanup did not finish")
	}
	assert.Equal(t, []string{"join:10.0.0.2:7946", "leave", "shutdown"}, handle.recordedCalls())
}

func TestMemberlistPeers_StopReturnsCleanupError(t *testing.T) {
	leaveErr := errors.New("leave failed")
	reader := interceptList(func(context.Context, ctrlclient.ObjectList, ...ctrlclient.ListOption) error {
		return nil
	})
	handle := &fakeMemberlistHandle{leaveErr: leaveErr}
	peer := startFakeLifecycle(t.Context(), reader, handle, time.Hour)

	err := peer.Stop(t.Context())
	assert.ErrorIs(t, err, leaveErr)
	assert.Equal(t, []string{"leave", "shutdown"}, handle.recordedCalls(), "Leave failure must not skip Shutdown")
}

func TestMemberlistPeers_ParentCancellationOwnsCleanup(t *testing.T) {
	parent, cancel := context.WithCancel(t.Context())
	reader := interceptList(func(context.Context, ctrlclient.ObjectList, ...ctrlclient.ListOption) error {
		return nil
	})
	handle := &fakeMemberlistHandle{}
	peer := startFakeLifecycle(parent, reader, handle, time.Hour)
	cancel()

	require.Eventually(t, func() bool {
		return handle.callCount("leave") == 1
	}, time.Second, time.Millisecond, "parent cancellation must trigger cleanup")
	require.NoError(t, peer.Stop(t.Context()))
	assert.Equal(t, []string{"leave", "shutdown"}, handle.recordedCalls())
}
