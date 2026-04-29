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
	"sync/atomic"
	"time"

	"github.com/hashicorp/memberlist"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/sandbox-manager/logs"
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
	// DefaultReconcileInterval is the interval between peer reconciliation checks
	DefaultReconcileInterval = 60 * time.Second
)

type MemberlistPeers struct {
	list      *memberlist.Memberlist
	config    *memberlist.Config
	localName string
	bindAddr  string
	bindPort  int

	started atomic.Bool
	stopCh  chan struct{}
}

// NewMemberlistPeers creates a new MemberlistPeers instance
func NewMemberlistPeers(nodeName string) *MemberlistPeers {
	return &MemberlistPeers{
		localName: nodeName,
		stopCh:    make(chan struct{}),
	}
}

// Start initializes and starts the memberlist
func (m *MemberlistPeers) Start(ctx context.Context, bindAddr string, bindPort int, existingPeers []string) error {
	log := klog.FromContext(ctx)

	if m.started.Load() {
		return fmt.Errorf("memberlist already started")
	}

	m.bindAddr = bindAddr
	m.bindPort = bindPort

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

// ReconcilePeers joins any peers not already known to the memberlist.
// It compares the provided peer addresses with current members and only
// attempts to join truly unknown nodes, preventing split-brain scenarios.
func (m *MemberlistPeers) ReconcilePeers(ctx context.Context, peers []string) error {
	if !m.started.Load() || m.list == nil {
		return fmt.Errorf("memberlist not started")
	}

	log := klog.FromContext(ctx)

	knownAddrs := make(map[string]struct{})
	for _, member := range m.list.Members() {
		addr := fmt.Sprintf("%s:%d", member.Addr.String(), member.Port)
		knownAddrs[addr] = struct{}{}
	}

	var unknownPeers []string
	for _, p := range peers {
		if _, known := knownAddrs[p]; !known {
			unknownPeers = append(unknownPeers, p)
		}
	}

	if len(unknownPeers) == 0 {
		return nil
	}

	log.Info("reconciling unknown peers", "count", len(unknownPeers), "peers", unknownPeers)
	joined, err := m.list.Join(unknownPeers)
	if err != nil {
		log.Error(err, "failed to join some peers during reconciliation", "joined", joined)
		return err
	}
	log.Info("successfully reconciled peers", "joined", joined)
	return nil
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
