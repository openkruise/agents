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

package sandbox_manager

import (
	"context"
	"fmt"
	"os"

	"github.com/google/uuid"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infracache "github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/peers"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
)

type GetInfraBuilderFunc func() (infra.Builder, error)

type NewPeerArgs struct {
	apiReader client.Reader
}
type GetPeersFunc func(args NewPeerArgs) (peers.Peers, error)

type SandboxManagerBuilder struct {
	instance       *SandboxManager
	opts           config.SandboxManagerOptions
	buildInfraFunc GetInfraBuilderFunc
	getPeersFunc   GetPeersFunc
	requestAdapter proxy.RequestAdapter
}

func NewSandboxManagerBuilder(opts config.SandboxManagerOptions) *SandboxManagerBuilder {
	opts = config.InitOptions(opts)
	return &SandboxManagerBuilder{
		instance: &SandboxManager{
			proxy:              proxy.NewServer(opts),
			memberlistBindPort: opts.MemberlistBindPort,
		},
		opts: opts,
	}
}

func (b *SandboxManagerBuilder) WithSandboxInfra() *SandboxManagerBuilder {
	b.buildInfraFunc = func() (infra.Builder, error) {
		mgr, err := infracache.NewControllerManager(b.opts.RestConfig, b.opts)
		if err != nil {
			return nil, err
		}
		cache, err := infracache.NewCache(mgr)
		if err != nil {
			return nil, err
		}
		return sandboxcr.NewInfraBuilder(b.opts).
			WithCache(cache).
			WithProxy(b.instance.proxy).
			WithAPIReader(mgr.GetAPIReader()), nil
	}
	return b
}

func (b *SandboxManagerBuilder) WithCustomInfra(builderFunc GetInfraBuilderFunc) *SandboxManagerBuilder {
	b.buildInfraFunc = builderFunc
	return b
}

func (b *SandboxManagerBuilder) WithMemberlistPeers() *SandboxManagerBuilder {
	b.getPeersFunc = func(args NewPeerArgs) (peers.Peers, error) {
		if b.opts.PeerSelector == "" {
			return nil, fmt.Errorf("peer selector is empty")
		}
		// build node name of sandbox-manager
		nodeName := os.Getenv("HOSTNAME")
		if nodeName == "" {
			nodeName = os.Getenv("POD_NAME")
		}
		if nodeName == "" {
			nodeName = uuid.NewString()[:8]
		}
		peersManager := peers.NewMemberlistPeers(
			args.apiReader,
			peers.NodePrefixSandboxManager+nodeName,
			b.opts.SystemNamespace,
			b.opts.PeerSelector)
		return peersManager, nil
	}

	return b
}

func (b *SandboxManagerBuilder) WithRequestAdapter(adapter proxy.RequestAdapter) *SandboxManagerBuilder {
	b.requestAdapter = adapter
	return b
}

func (b *SandboxManagerBuilder) Build() (*SandboxManager, error) {
	// Build infra
	if b.buildInfraFunc == nil {
		return nil, errors.NewError(errors.ErrorInternal, "infra builder is not configured: call WithSandboxInfra or WithCustomInfra before Build")
	}
	builder, err := b.buildInfraFunc()
	if err != nil {
		return nil, errors.NewError(errors.ErrorInternal, "failed to get infra builder: %v", err)
	}
	b.instance.infra = builder.Build()
	reader := b.instance.infra.GetCache().GetAPIReader()

	// Build peers manager
	if b.getPeersFunc != nil {
		peersManager, err := b.getPeersFunc(NewPeerArgs{apiReader: reader})
		if err != nil {
			return nil, errors.NewError(errors.ErrorInternal, "failed to get peers manager: %v", err)
		}
		b.instance.peersManager = peersManager
		b.instance.proxy.SetPeersManager(peersManager)
	}

	// Wire request adapter onto the proxy if provided
	if b.requestAdapter != nil {
		b.instance.proxy.SetRequestAdapter(b.requestAdapter)
	}

	return b.instance, nil
}

type SandboxManager struct {
	peersManager       peers.Peers
	memberlistBindPort int

	infra infra.Infrastructure
	proxy *proxy.Server
}

func (m *SandboxManager) Run(ctx context.Context) error {
	log := klog.FromContext(ctx)

	go func() {
		klog.InfoS("starting proxy")
		err := m.proxy.Run()
		if err != nil {
			klog.Error(err, "proxy stopped")
		}
	}()

	// Start peers (optional - only if configured)
	if m.peersManager != nil {
		if err := m.peersManager.Start(ctx, m.memberlistBindPort); err != nil {
			return fmt.Errorf("failed to start memberlist: %w", err)
		}
		log.Info("memberlist started successfully")
	} else {
		log.Info("peers manager not configured, skip starting memberlist")
	}

	if err := m.infra.Run(ctx); err != nil {
		return err
	}
	return nil
}

func (m *SandboxManager) Stop(ctx context.Context) {
	log := klog.FromContext(ctx)
	m.proxy.Stop(ctx)
	m.infra.Stop(ctx)
	if m.peersManager != nil {
		if err := m.peersManager.Stop(); err != nil {
			log.Error(err, "failed to stop peers manager")
		}
	}
}

func (m *SandboxManager) GetInfra() infra.Infrastructure {
	return m.infra
}
