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
	"fmt"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/openkruise/agents/pkg/features"
	"github.com/openkruise/agents/pkg/peers"
	"github.com/openkruise/agents/pkg/peersecurity"
	"github.com/openkruise/agents/pkg/sandboxroute"
	"github.com/openkruise/agents/pkg/sandboxroute/refresh"
	"github.com/openkruise/agents/pkg/utils"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
)

// RouteSync manages peer discovery and publishes route refreshes without serving them.
// ready publishes the read-only outbound client after startup succeeds.
type RouteSync struct {
	peerReader            ctrlclient.Reader
	peers                 *peers.MemberlistPeers
	bindAddress           string
	bindPort              int
	ready                 chan struct{}
	outbound              *refresh.PeerOutbound
	lastNoPeersLog        atomic.Int64
	security              peersecurity.Inputs
	securityRetryInterval time.Duration
}

// SetupRouteSync reuses the manager/gateway peer client, security configuration, and sender.
// A disabled gate or empty selector skips validation, material loading, and listener startup.
// An empty bindAddress uses peers.FindPodIP; a non-positive bindPort uses 7946.
func SetupRouteSync(mgr manager.Manager, peerSelector, bindAddress string, bindPort int, security peersecurity.Inputs) (*RouteSync, error) {
	if !utilfeature.DefaultFeatureGate.Enabled(features.SandboxRouteSyncGate) || peerSelector == "" {
		return nil, nil
	}
	if bindPort <= 0 {
		bindPort = 7946
	}
	if bindPort > 65535 {
		return nil, fmt.Errorf("--memberlist-bind-port must be in the range 1-65535, got %d", bindPort)
	}
	if _, err := labels.Parse(peerSelector); err != nil {
		return nil, fmt.Errorf("parse peer selector: %w", err)
	}
	if err := security.Validate(); err != nil {
		return nil, fmt.Errorf("validate controller peer security: %w", err)
	}
	nodeName := peers.NewPeerNodeName(true)

	// Like manager/gateway, build the peer client from the process rest.Config.
	// The shared path only lists seed Pods and gets referenced Secrets, without adding informers.
	peerClient, err := ctrlclient.New(mgr.GetConfig(), ctrlclient.Options{Scheme: mgr.GetScheme(), Mapper: mgr.GetRESTMapper()})
	if err != nil {
		return nil, fmt.Errorf("create peer client: %w", err)
	}
	sender := &RouteSync{
		security:              security,
		securityRetryInterval: peers.DefaultJoinRetryInterval,
		peerReader:            peerClient,
		peers:                 peers.NewMemberlistPeers(peerClient, peers.NodePrefixSandboxController+nodeName, utils.DefaultSandboxDeployNamespace, peerSelector),
		bindAddress:           bindAddress,
		bindPort:              bindPort,
		ready:                 make(chan struct{}),
	}
	if err := mgr.Add(sender); err != nil {
		return nil, fmt.Errorf("register controller route sync: %w", err)
	}
	return sender, nil
}

// NeedLeaderElection lets all replicas maintain membership without blocking other controllers on security material.
func (s *RouteSync) NeedLeaderElection() bool { return false }

// Start waits for peer Secrets through the shared loader, then starts the shared memberlist lifecycle.
func (s *RouteSync) Start(ctx context.Context) error {
	for attempt := 1; ; attempt++ {
		loadCtx, loadCancel := context.WithTimeout(ctx, peersecurity.LoadTimeout)
		key, _, clientTLS, loadErr := peersecurity.Load(loadCtx, s.peerReader, s.security)
		loadCancel()
		if loadErr == nil {
			s.peers.SetSecretKey(key)
			s.outbound = refresh.NewPeerOutbound(clientTLS)
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		logger := logf.FromContext(ctx)
		if attempt > 1 {
			logger = logger.V(2)
		}
		logger.Error(loadErr, "controller route sync waiting for valid security material", "retryInterval", s.securityRetryInterval)
		retry := time.NewTimer(s.securityRetryInterval)
		select {
		case <-ctx.Done():
			retry.Stop()
			return ctx.Err()
		case <-retry.C:
		}
	}
	defer s.outbound.CloseIdleConnections()
	if err := s.peers.Start(ctx, s.bindAddress, s.bindPort); err != nil {
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
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	peerList := s.peers.GetPeers()
	if err := refresh.SyncRouteWithPeerList(ctx, peerList, route, s.outbound); err != nil {
		return err
	}
	logger := klog.FromContext(ctx).WithValues("sandbox", klog.KRef(route.Namespace, route.Name))
	if len(peerList) == 0 {
		// Skip when no receivers are discovered; limit logging across concurrent reconciles to once per minute.
		now := time.Now().UnixNano()
		previous := s.lastNoPeersLog.Load()
		if (previous == 0 || now-previous >= int64(time.Minute)) && s.lastNoPeersLog.CompareAndSwap(previous, now) {
			logger.Info("route sync skipped: no peers discovered", "peerCount", 0)
		}
		return nil
	}
	logger.Info("synced Sandbox route with peers", "peerCount", len(peerList),
		"canRouteTraffic", route.CanRouteTraffic(), "resourceVersion", route.ResourceVersion)
	return nil
}
