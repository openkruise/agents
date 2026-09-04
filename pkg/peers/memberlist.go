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
	"fmt"
	"net"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/memberlist"
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/logs"
	"github.com/openkruise/agents/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// DefaultProbeInterval is the interval between gossip probes
	DefaultProbeInterval = 500 * time.Millisecond
	// DefaultProbeTimeout is the timeout for gossip probes
	DefaultProbeTimeout = 200 * time.Millisecond
	// DefaultGossipInterval is the interval between gossip messages
	DefaultGossipInterval = 500 * time.Millisecond
	// DefaultGossipNodes is the number of nodes to gossip to
	DefaultGossipNodes = 3
	// DefaultSuspicionMult is the multiplier for determining the time to wait before considering a node suspect
	DefaultSuspicionMult = 4
	// DefaultRetransmitMult is the multiplier for the number of retransmissions
	DefaultRetransmitMult = 4
	// DefaultReconcileInterval keeps Kubernetes discovery as a low-frequency recovery backstop.
	DefaultReconcileInterval = 60 * time.Second
	// reconcileJitterFactor spreads Kubernetes list calls from concurrently started replicas.
	reconcileJitterFactor = 0.1
)

type MemberlistPeers struct {
	apiReader ctrlclient.Reader

	localName    string
	peerSelector string
	sysNs        string

	list    *memberlist.Memberlist
	config  *memberlist.Config
	localIP string

	started  atomic.Bool
	stopCh   chan struct{}
	stopOnce sync.Once

	reconcileInterval time.Duration
	reconcileWG       sync.WaitGroup
}

// NewMemberlistPeers creates a new MemberlistPeers instance
func NewMemberlistPeers(apiReader ctrlclient.Reader, nodeName string, namespace, peerSelector string) *MemberlistPeers {
	return &MemberlistPeers{
		apiReader:         apiReader,
		sysNs:             namespace,
		peerSelector:      peerSelector,
		localName:         nodeName,
		stopCh:            make(chan struct{}),
		reconcileInterval: DefaultReconcileInterval,
	}
}

func FindPodIP() (string, error) {
	podIP := os.Getenv("POD_IP")
	if podIP == "" {
		podIP = utils.GetFirstNonLoopbackIP()
	}
	if podIP == "" {
		return "", fmt.Errorf("failed to determine local IP for memberlist")
	}
	return podIP, nil
}

// Start initializes and starts the memberlist
func (m *MemberlistPeers) Start(ctx context.Context, bindPort int) error {
	log := klog.FromContext(ctx)
	var err error
	if m.started.Load() {
		return fmt.Errorf("memberlist already started")
	}

	// Get pod IP for memberlist binding
	localIP := m.localIP
	if localIP == "" {
		localIP, err = FindPodIP()
		if err != nil {
			return fmt.Errorf("failed to determine local IP for memberlist: %w", err)
		}
	}
	localURL := fmt.Sprintf("%s:%d", localIP, bindPort)

	// Get existing peers from Kubernetes API for initial join
	log.Info("discovering existing peers for memberlist join", "localURL", localURL)
	existingPeers, err := m.discoverPeerAddresses(ctx, bindPort, localURL)
	if err != nil {
		return err
	}
	log.Info("found existing peers for memberlist join", "count", len(existingPeers))

	bindAddr := localIP

	// Create memberlist config
	config := memberlist.DefaultLANConfig()
	config.Name = m.localName
	config.BindAddr = bindAddr
	config.BindPort = bindPort
	config.AdvertisePort = bindPort

	// Tuning for faster failure detection and convergence
	config.ProbeInterval = DefaultProbeInterval
	config.ProbeTimeout = DefaultProbeTimeout
	config.GossipInterval = DefaultGossipInterval
	config.GossipNodes = DefaultGossipNodes
	config.SuspicionMult = DefaultSuspicionMult
	config.RetransmitMult = DefaultRetransmitMult

	// Set up event delegate to track membership changes
	config.Events = &eventDelegate{
		parent: m,
		logCtx: logs.NewContext(),
	}

	// Disable logging from memberlist itself (we use klog)
	config.LogOutput = nil
	config.Logger = nil

	m.config = config

	// Create the memberlist
	list, err := memberlist.Create(config)
	if err != nil {
		return fmt.Errorf("failed to create memberlist: %w", err)
	}
	m.list = list

	// Join existing peers if provided
	if len(existingPeers) > 0 {
		log.Info("attempting to join existing peers", "peers", existingPeers)
		joined, err := list.Join(existingPeers)
		if err != nil {
			log.Error(err, "failed to join some peers", "joined", joined)
			// Don't return error - we can still operate as a single node
		} else {
			log.Info("successfully joined peers", "count", joined)
		}
	}

	m.started.Store(true)
	m.reconcileWG.Add(1)
	go func() {
		defer m.reconcileWG.Done()
		m.runPeerReconciliation(ctx, bindPort, localURL)
	}()
	log.Info("memberlist started", "addr", bindAddr, "port", bindPort, "name", m.localName)

	return nil
}

// Stop gracefully leaves the cluster and shuts down
func (m *MemberlistPeers) Stop() error {
	if !m.started.Load() || m.list == nil {
		return nil
	}

	m.stopOnce.Do(func() {
		close(m.stopCh)
	})
	m.reconcileWG.Wait()

	// Gracefully leave the cluster
	if err := m.list.Leave(5 * time.Second); err != nil {
		return fmt.Errorf("failed to leave memberlist: %w", err)
	}

	if err := m.list.Shutdown(); err != nil {
		return fmt.Errorf("failed to shutdown memberlist: %w", err)
	}

	m.started.Store(false)
	return nil
}

// discoverPeerAddresses lists the desired memberlist peers from Kubernetes.
// Initial discovery and periodic reconciliation share this method so they use
// identical namespace, selector, annotation, and address resolution rules.
func (m *MemberlistPeers) discoverPeerAddresses(ctx context.Context, bindPort int, localURL string) ([]string, error) {
	selector, err := labels.Parse(m.peerSelector)
	if err != nil {
		return nil, fmt.Errorf("failed to parse peer selector: %w", err)
	}

	peerList := &corev1.PodList{}
	if err := m.apiReader.List(ctx, peerList, &ctrlclient.ListOptions{
		Namespace:     m.sysNs,
		LabelSelector: selector,
	}); err != nil {
		return nil, fmt.Errorf("failed to list peer pods: %w", err)
	}

	log := klog.FromContext(ctx)
	addressSet := make(map[string]struct{}, len(peerList.Items))
	for i := range peerList.Items {
		peer := &peerList.Items[i]

		// AnnotationMemberlistURL is generally not needed in production when all replicas use the same memberlist port.
		// In scenarios like unit tests or single-host multi-replica deployments where a fixed port cannot be guaranteed
		// for all replicas, this annotation can be used as a way to specify a custom address.
		memberlistURL := peer.Annotations[agentsv1alpha1.AnnotationMemberlistURL]
		if memberlistURL == "" {
			if peer.Status.PodIP == "" {
				log.V(4).Info("ignoring peer with empty IP", "peer", klog.KObj(peer))
				continue
			}
			memberlistURL = fmt.Sprintf("%s:%d", peer.Status.PodIP, bindPort)
		}
		if memberlistURL == localURL {
			continue
		}
		addressSet[memberlistURL] = struct{}{}
	}

	addresses := make([]string, 0, len(addressSet))
	for address := range addressSet {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	return addresses, nil
}

// runPeerReconciliation periodically rediscovers peer addresses so a failed
// initial join or a healed network partition does not leave permanent split-brain clusters.
func (m *MemberlistPeers) runPeerReconciliation(ctx context.Context, bindPort int, localURL string) {
	interval := m.reconcileInterval
	if interval <= 0 {
		interval = DefaultReconcileInterval
	}
	timer := time.NewTimer(wait.Jitter(interval, reconcileJitterFactor))
	defer timer.Stop()

	log := klog.FromContext(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopCh:
			return
		case <-timer.C:
			if err := m.reconcilePeers(ctx, bindPort, localURL); err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Error(err, "failed to reconcile memberlist peers")
			}
			timer.Reset(wait.Jitter(interval, reconcileJitterFactor))
		}
	}
}

// reconcilePeers rejoins desired Kubernetes peers that are not currently alive.
func (m *MemberlistPeers) reconcilePeers(ctx context.Context, bindPort int, localURL string) error {
	if !m.started.Load() || m.list == nil {
		return fmt.Errorf("memberlist not started")
	}

	desiredPeers, err := m.discoverPeerAddresses(ctx, bindPort, localURL)
	if err != nil {
		return err
	}

	alivePeers := make(map[string]struct{})
	for _, member := range m.list.Members() {
		if member.State != memberlist.StateAlive {
			continue
		}
		alivePeers[fmt.Sprintf("%s:%d", member.Addr.String(), member.Port)] = struct{}{}
	}

	missingPeers := make([]string, 0, len(desiredPeers))
	for _, address := range desiredPeers {
		if _, alive := alivePeers[address]; !alive {
			missingPeers = append(missingPeers, address)
		}
	}
	if len(missingPeers) == 0 {
		return nil
	}

	log := klog.FromContext(ctx)
	log.Info("reconciling memberlist peers", "count", len(missingPeers), "peers", missingPeers)
	joined, err := m.list.Join(missingPeers)
	if err != nil {
		return fmt.Errorf("failed to join discovered memberlist peers after joining %d: %w", joined, err)
	}
	log.Info("reconciled memberlist peers", "joined", joined)
	return nil
}

// GetPeers returns the current list of alive peers (excluding self)
func (m *MemberlistPeers) GetPeers() []Peer {
	if !m.started.Load() || m.list == nil {
		return nil
	}

	peers := make([]Peer, 0, len(m.list.Members()))
	for _, member := range m.list.Members() {
		if member.Name == m.localName {
			continue
		}
		if member.State == memberlist.StateAlive {
			peers = append(peers, Peer{
				IP:   member.Addr.String(),
				Name: member.Name,
			})
		}
	}
	return peers
}

// GetAllMembers returns all members including self
func (m *MemberlistPeers) GetAllMembers() []Peer {
	if !m.started.Load() || m.list == nil {
		return nil
	}

	members := make([]Peer, 0, len(m.list.Members()))
	for _, member := range m.list.Members() {
		members = append(members, Peer{
			IP:   member.Addr.String(),
			Name: member.Name,
		})
	}
	return members
}

// WaitForPeers blocks until at least minPeers are discovered or context is canceled
func (m *MemberlistPeers) WaitForPeers(ctx context.Context, minPeers int) error {
	log := klog.FromContext(ctx)
	log.Info("waiting for peers", "minPeers", minPeers)

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-m.stopCh:
			return fmt.Errorf("memberlist stopped")
		case <-ticker.C:
			peers := m.GetPeers()
			if len(peers) >= minPeers {
				log.Info("minimum peers reached", "count", len(peers))
				return nil
			}
			log.V(4).Info("waiting for more peers", "current", len(peers), "min", minPeers)
		}
	}
}

// LocalAddr returns the local node's address
func (m *MemberlistPeers) LocalAddr() net.IP {
	if !m.started.Load() || m.list == nil {
		return nil
	}
	return m.list.LocalNode().Addr
}

// LocalPort returns the local node's port
func (m *MemberlistPeers) LocalPort() int {
	if !m.started.Load() || m.list == nil {
		return 0
	}
	return int(m.list.LocalNode().Port)
}

// LocalName returns the local node's memberlist name.
func (m *MemberlistPeers) LocalName() string {
	return m.localName
}

// eventDelegate handles memberlist membership change events
type eventDelegate struct {
	parent *MemberlistPeers
	logCtx context.Context
}

func (e *eventDelegate) NotifyJoin(node *memberlist.Node) {
	if node.Name == e.parent.localName {
		return
	}
	klog.FromContext(e.logCtx).Info("peer joined", "name", node.Name, "ip", node.Addr.String())
}

func (e *eventDelegate) NotifyLeave(node *memberlist.Node) {
	if node.Name == e.parent.localName {
		return
	}
	klog.FromContext(e.logCtx).Info("peer left", "name", node.Name, "ip", node.Addr.String())
}

func (e *eventDelegate) NotifyUpdate(*memberlist.Node) {
	// Handle metadata updates if needed in the future
}

// Ensure MemberlistPeers implements Peers
var _ Peers = (*MemberlistPeers)(nil)
