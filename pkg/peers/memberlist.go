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
	"sync/atomic"
	"time"

	"github.com/hashicorp/memberlist"
	agentsapiv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/logs"
	"github.com/openkruise/agents/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
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
)

type MemberlistPeers struct {
	apiReader ctrlclient.Reader

	localName    string
	peerSelector string
	sysNs        string

	list    *memberlist.Memberlist
	config  *memberlist.Config
	localIP string

	started atomic.Bool
	stopCh  chan struct{}
}

// NewMemberlistPeers creates a new MemberlistPeers instance
func NewMemberlistPeers(apiReader ctrlclient.Reader, nodeName string, namespace, peerSelector string) *MemberlistPeers {
	return &MemberlistPeers{
		apiReader:    apiReader,
		sysNs:        namespace,
		peerSelector: peerSelector,
		localName:    nodeName,
		stopCh:       make(chan struct{}),
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
	apiReader := m.apiReader
	selector, err := labels.Parse(m.peerSelector)
	if err != nil {
		return fmt.Errorf("failed to parse peer selector: %w", err)
	}
	peerList := &corev1.PodList{}
	if err := apiReader.List(ctx, peerList, &ctrlclient.ListOptions{
		Namespace:     m.sysNs,
		LabelSelector: selector,
	}); err != nil {
		return fmt.Errorf("failed to list peer pods: %w", err)
	}
	log.Info("listed peers for memberlist join", "count", len(peerList.Items))

	// Build list of existing peer IPs for initial join
	existingPeers := make([]string, 0)
	for _, peer := range peerList.Items {
		// AnnotationMemberlistURL is generally not needed in production when all replicas use the same memberlist port.
		// In scenarios like unit tests or single-host multi-replica deployments where a fixed port cannot be guaranteed
		// for all replicas, this annotation can be used as a way to specify a custom address.
		memberlistURL := peer.Annotations[agentsapiv1alpha1.AnnotationMemberlistURL]
		if memberlistURL == "" {
			ip := peer.Status.PodIP
			if ip == "" {
				log.Info("ignoring peer with empty IP", "ip", ip, "peer", klog.KObj(&peer))
				continue
			}
			memberlistURL = fmt.Sprintf("%s:%d", ip, bindPort)
		}
		if memberlistURL == localURL {
			continue
		}
		// Memberlist uses the bind port for gossip
		existingPeers = append(existingPeers, memberlistURL)
		log.Info("adding peer for memberlist join", "memberlistURL", memberlistURL, "pod", klog.KObj(&peer))
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
	log.Info("memberlist started", "addr", bindAddr, "port", bindPort, "name", m.localName)

	return nil
}

// Stop gracefully leaves the cluster and shuts down
func (m *MemberlistPeers) Stop() error {
	if !m.started.Load() || m.list == nil {
		return nil
	}

	close(m.stopCh)

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
