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
)

// Peer represents a discovered peer in the cluster
type Peer struct {
	IP   string
	Name string
}

// Peers defines the interface for peer discovery and management
// It abstracts the underlying implementation (memberlist) from the consumers
type Peers interface {
	// Start initializes and starts the peer discovery mechanism
	Start(ctx context.Context, bindAddr string, bindPort int, existingPeers []string) error

	// Stop gracefully shuts down the peer discovery
	Stop() error

	// GetPeers returns the current list of alive peers (excluding self)
	GetPeers() []Peer

	// GetAllMembers returns all members including self
	GetAllMembers() []Peer

	// WaitForPeers blocks until at least minPeers are discovered or context is cancelled
	WaitForPeers(ctx context.Context, minPeers int) error

	// LocalAddr returns the local node's address
	LocalAddr() net.IP

	// LocalPort returns the local node's port
	LocalPort() int
}
