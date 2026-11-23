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

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/openkruise/agents/pkg/peers"
	"github.com/openkruise/agents/pkg/sandboxroute"
	"github.com/openkruise/agents/pkg/sandboxroute/refresh"
)

const (
	controllerPeerNamespace = "sandbox-system"
	controllerPeerBindPort  = 7946
)

// RouteSync manages peer discovery and publishes route refreshes without serving them.
// ready publishes the read-only outbound client after startup succeeds.
type RouteSync struct {
	cache    ctrlcache.Cache
	peers    *peers.MemberlistPeers
	bindPort int
	ready    chan struct{}
	outbound *refresh.PeerOutbound
}

// SetupRouteSync registers a dedicated peer cache and the plaintext sender lifecycle.
// An empty selector disables sync without starting any peer listeners.
func SetupRouteSync(mgr manager.Manager, peerSelector string) (*RouteSync, error) {
	if peerSelector == "" {
		return nil, nil
	}
	selector, err := labels.Parse(peerSelector)
	if err != nil {
		return nil, fmt.Errorf("parse peer selector: %w", err)
	}
	nodeName := os.Getenv("POD_NAME")
	if nodeName == "" {
		nodeName, err = os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("get controller peer name: %w", err)
		}
	}

	// The main Pod cache may exclude manager/gateway Pods; peer discovery uses a separate filter scope.
	peerCache, err := ctrlcache.New(mgr.GetConfig(), ctrlcache.Options{
		Scheme:                      mgr.GetScheme(),
		Mapper:                      mgr.GetRESTMapper(),
		ReaderFailOnMissingInformer: true,
		ByObject: map[ctrlclient.Object]ctrlcache.ByObject{
			&corev1.Pod{}: {
				Namespaces: map[string]ctrlcache.Config{controllerPeerNamespace: {}},
				Label:      selector,
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create peer cache: %w", err)
	}
	if _, err := peerCache.GetInformer(context.Background(), &corev1.Pod{}, ctrlcache.BlockUntilSynced(false)); err != nil {
		return nil, fmt.Errorf("register peer informer: %w", err)
	}
	sender := &RouteSync{
		cache:    peerCache,
		peers:    peers.NewMemberlistPeers(peerCache, peers.NodePrefixSandboxController+nodeName, controllerPeerNamespace, peerSelector),
		bindPort: controllerPeerBindPort,
		ready:    make(chan struct{}),
	}
	if err := mgr.Add(sender); err != nil {
		return nil, fmt.Errorf("register controller route sync: %w", err)
	}
	return sender, nil
}

// GetCache lets the manager start and synchronize the peer cache before starting controllers.
func (s *RouteSync) GetCache() ctrlcache.Cache { return s.cache }

// Start discovers peers from the synchronized Pod cache, joins memberlist, and waits for manager shutdown.
func (s *RouteSync) Start(ctx context.Context) (err error) {
	ctx, cancel := context.WithCancel(ctx)
	cacheDone := make(chan error, 1)
	go func() {
		cacheErr := s.cache.Start(ctx)
		cancel()
		cacheDone <- cacheErr
	}()
	defer func() {
		cancel()
		err = errors.Join(err, <-cacheDone)
	}()
	if !s.cache.WaitForCacheSync(ctx) {
		return fmt.Errorf("synchronize controller peer cache: %w", ctx.Err())
	}
	s.outbound = refresh.NewPeerOutbound(nil)
	defer s.outbound.CloseIdleConnections()
	if err := s.peers.Start(ctx, "", s.bindPort); err != nil {
		return fmt.Errorf("start controller peers: %w", err)
	}
	close(s.ready)
	<-ctx.Done()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), peers.DefaultLeaveTimeout+10*time.Second)
	defer stopCancel()
	return s.peers.Stop(stopCtx)
}

// SyncRouteWithPeers waits for startup publication, then uses the shared sender implementation.
func (s *RouteSync) SyncRouteWithPeers(ctx context.Context, route sandboxroute.Route) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ready:
		return refresh.SyncRouteWithPeers(ctx, s.peers, route, s.outbound)
	}
}
