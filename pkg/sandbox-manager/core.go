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
	"github.com/redis/go-redis/v9"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infracache "github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/peers"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/sandbox-manager/quota"
	quotaspec "github.com/openkruise/agents/pkg/sandbox-manager/quota/spec"
	"github.com/openkruise/agents/pkg/sandboxid"
	"github.com/openkruise/agents/pkg/sandboxroute"
)

// QuotaEnforcer is the minimal surface sandbox-manager needs for admission, delete release, and cleanup.
// InitQuota wires the production implementation.
type QuotaEnforcer interface {
	Acquire(ctx context.Context, req quota.AcquireRequest) error
	Release(ctx context.Context, req quota.ReleaseRequest) error
	Cleanup(ctx context.Context, user string) error
}

type RedisClient interface {
	Close() error
}

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
			routeProjector:     newManagerRouteProjector(),
			routeNamespace:     opts.SandboxNamespace,
			memberlistBindPort: opts.MemberlistBindPort,
			enableShortID:      opts.EnableShortSandboxID,
			primary:            &primaryState{},
		},
		opts: opts,
	}
}

func (b *SandboxManagerBuilder) WithSandboxInfra() *SandboxManagerBuilder {
	b.buildInfraFunc = func() (infra.Builder, error) {
		mgr, health, err := infracache.NewControllerManagerWithHealth(b.opts.RestConfig, b.opts)
		if err != nil {
			return nil, err
		}
		cache, err := infracache.NewCacheWithOptions(mgr, infracache.Options{
			Health:            health,
			SandboxIDResolver: sandboxid.Resolve,
		})
		if err != nil {
			return nil, err
		}
		return sandboxcr.NewInfraBuilder(b.opts).
			WithCache(cache).
			WithRouteVersionReader(b.instance.proxy).
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

// WithQuotaEnforcer injects a quota enforcer before Build.
// InitQuota overwrites this value; tests using this helper must skip InitQuota.
func (b *SandboxManagerBuilder) WithQuotaEnforcer(qe QuotaEnforcer) *SandboxManagerBuilder {
	b.instance.quota = qe
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
	routeSelector, err := labels.Parse(b.opts.SandboxLabelSelector)
	if err != nil {
		return nil, errors.NewError(errors.ErrorInternal, "invalid sandbox route label selector: %v", err)
	}
	b.instance.routeSelector = routeSelector
	routeSource := b.instance.infra.GetRouteSandboxSource()
	if routeSource == nil {
		return nil, errors.NewError(errors.ErrorInternal, "route sandbox source is not configured")
	}
	routeRepairer, err := sandboxroute.NewRepairer(
		b.instance.proxy.Store(),
		b.instance.observeRoute(routeSource),
		sandboxroute.RepairerOptions{},
	)
	if err != nil {
		return nil, errors.NewError(errors.ErrorInternal, "failed to initialize manager route repairer: %v", err)
	}
	b.instance.routeRepairer = routeRepairer
	b.instance.proxy.SetRepairEnqueuer(routeRepairer.Enqueue)
	if err := routeSource.RegisterEventHandler(b.instance.reconcileSandboxRoute); err != nil {
		return nil, errors.NewError(errors.ErrorInternal, "failed to register manager route feeder: %v", err)
	}

	// Build peers manager
	if b.getPeersFunc != nil {
		reader := b.instance.infra.GetCache().GetAPIReader()
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

	if b.opts.RestConfig != nil {
		elector, err := newPrimaryElector(b.opts, b.instance.primary)
		if err != nil {
			return nil, errors.NewError(errors.ErrorInternal, "failed to create primary elector: %v", err)
		}
		b.instance.elector = elector
	} else {
		b.instance.primary.set(true)
	}

	return b.instance, nil
}

type SandboxManager struct {
	peersManager       peers.Peers
	memberlistBindPort int

	infra infra.Infrastructure
	proxy *proxy.Server

	routeProjector *sandboxroute.Projector
	routeRepairer  *sandboxroute.Repairer
	routeNamespace string
	routeSelector  labels.Selector

	enableShortID bool

	primary *primaryState
	elector *primaryElector

	quota            QuotaEnforcer          // nil until InitQuota or builder injection
	quotaAntiDrift   *quota.AntiDriftDriver // nil when Redis is not configured
	quotaRedisClient RedisClient            // nil when Redis is not configured
}

// InitQuota initializes the quota subsystem. Call after Build() so that m.infra is available.
// When opts.RedisAddr is empty, a no-op backend is used (limited keys are accepted but unenforced).
// subjects may be nil when key storage is disabled.
func (m *SandboxManager) InitQuota(ctx context.Context, opts config.QuotaOptions, subjects quotaspec.SubjectLister) error {
	log := klog.FromContext(ctx)
	if opts.RedisAddr == "" {
		m.quota = quota.NewManager(quota.NoopBackend{})
		log.Info("api-key quota Redis is not configured; limited keys are accepted but unenforced")
		return nil
	}
	if m.infra == nil || m.infra.GetCache() == nil {
		return fmt.Errorf("api-key quota Redis is configured but cache is not available")
	}
	provider, ok := m.infra.(infra.QuotaSandboxSourceProvider)
	if !ok {
		return fmt.Errorf("api-key quota Redis is configured but quota sandbox source is not available")
	}

	// Apply defensive defaults for programmatic callers that skip InitOptions.
	if opts.OperationTimeout <= 0 {
		opts.OperationTimeout = consts.DefaultQuotaRedisOperationTimeout
	}
	if opts.BreakerN <= 0 {
		opts.BreakerN = consts.DefaultQuotaRedisBreakerN
	}
	if opts.BreakerD <= 0 {
		opts.BreakerD = consts.DefaultQuotaRedisBreakerD
	}
	if opts.AntiDriftInterval <= 0 {
		opts.AntiDriftInterval = consts.DefaultQuotaAntiDriftInterval
	}
	if opts.AntiDriftGrace <= 0 {
		opts.AntiDriftGrace = consts.DefaultQuotaAntiDriftGrace
	}

	redisClient := redis.NewClient(&redis.Options{
		Addr:     opts.RedisAddr,
		Username: opts.RedisUsername,
		Password: opts.RedisPassword,
		DB:       opts.RedisDB,
	})
	redisBackend := quota.NewRedisBackend(redisClient, opts.OperationTimeout)
	hotBackend := quota.NewBreakerBackend(redisBackend, opts.BreakerN, opts.BreakerD)
	// Request admission and anti-drift events share this breaker so Redis release
	// failures trip request-path fail-open behavior instead of drifting silently.
	source := provider.GetQuotaSandboxSource()
	driver := quota.NewAntiDriftDriver(quota.AntiDriftConfig{
		Interval: opts.AntiDriftInterval,
		Grace:    opts.AntiDriftGrace,
	}, m, subjects, source, hotBackend)
	subscription, err := source.Subscribe(ctx, driver.QuotaEventHandler())
	if err != nil {
		_ = redisClient.Close()
		return err
	}
	driver.SetSubscription(subscription)
	m.quota = quota.NewManager(hotBackend)
	m.quotaAntiDrift = driver
	m.quotaRedisClient = redisClient
	log.Info("api-key quota Redis configured; Redis transport errors fail open", "addr", opts.RedisAddr)
	return nil
}

// CleanupQuota removes quota state for the given user (e.g. after API-key deletion).
// Safe to call when quota is not initialized.
func (m *SandboxManager) CleanupQuota(ctx context.Context, user string) error {
	if m == nil || m.quota == nil || user == "" {
		return nil
	}
	return m.quota.Cleanup(ctx, user)
}

func (m *SandboxManager) Run(ctx context.Context) error {
	log := klog.FromContext(ctx)

	if m.elector != nil {
		go m.elector.Run(ctx)
	} else {
		m.primary.set(true)
	}

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
	if m.routeRepairer != nil {
		go func() {
			if err := m.routeRepairer.Start(ctx); err != nil {
				log.Error(err, "manager route repairer stopped")
			}
		}()
	}
	if m.quotaAntiDrift != nil {
		m.quotaAntiDrift.Run(ctx)
	}
	return nil
}

func (m *SandboxManager) Stop(ctx context.Context) {
	log := klog.FromContext(ctx)
	if m.elector != nil {
		m.elector.Stop(ctx)
	}
	m.proxy.Stop(ctx)
	m.infra.Stop(ctx)
	if m.peersManager != nil {
		if err := m.peersManager.Stop(); err != nil {
			log.Error(err, "failed to stop peers manager")
		}
	}
	// Stop quota anti-drift before closing the Redis client
	if m.quotaAntiDrift != nil {
		m.quotaAntiDrift.Stop()
	}
	if m.quotaRedisClient != nil {
		if err := m.quotaRedisClient.Close(); err != nil {
			log.Error(err, "failed to close quota Redis client")
		}
	}
}

func (m *SandboxManager) GetInfra() infra.Infrastructure {
	return m.infra
}

func (m *SandboxManager) IsPrimary() bool {
	if m == nil || m.primary == nil {
		return true
	}
	return m.primary.IsPrimary()
}

func (m *SandboxManager) WaitPrimary(ctx context.Context) error {
	if m == nil || m.primary == nil {
		return nil
	}
	return m.primary.WaitPrimary(ctx)
}

func (m *SandboxManager) PrimaryChanged() <-chan struct{} {
	if m == nil || m.primary == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return m.primary.PrimaryChanged()
}
