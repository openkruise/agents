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
	"k8s.io/apimachinery/pkg/api/validate/content"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infracache "github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/identity"
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
	"github.com/openkruise/agents/pkg/utils/runtime"
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
	// runtimeTLSBundle is the client TLS bundle for reaching TLS-capable
	// agent-runtimes. It is carried on the builder rather than on
	// config.SandboxManagerOptions (which the cache layer imports) and handed to
	// the sandbox infra so claim/clone post-processing can resolve the
	// per-sandbox runtime transport. Nil disables runtime TLS.
	runtimeTLSBundle *runtime.TLSBundle
}

func NewSandboxManagerBuilder(opts config.SandboxManagerOptions) *SandboxManagerBuilder {
	opts = config.InitOptions(opts)
	return &SandboxManagerBuilder{
		instance: &SandboxManager{
			proxy:                    proxy.NewServer(opts),
			memberlistBindPort:       opts.MemberlistBindPort,
			systemNamespace:          opts.SystemNamespace,
			enableShortID:            opts.EnableShortSandboxID,
			shortIDPrefix:            opts.ShortSandboxIDPrefix,
			primary:                  &primaryState{},
			trafficTokenOptions:      identity.TokenOptions{RequestedValidity: opts.TrafficAccessToken.Validity},
			trafficTokenSingleflight: newTrafficTokenSingleflight(),
			trafficTokenIssues:       trafficTokenIssueLifecycle{timeout: defaultTrafficTokenIssueTimeout},
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
		cache, err := infracache.NewCacheWithHealth(mgr, health)
		if err != nil {
			return nil, err
		}
		return sandboxcr.NewInfraBuilder(b.opts).
			WithCache(cache).
			WithRouteReader(b.instance.proxy).
			WithAPIReader(mgr.GetAPIReader()).
			WithRuntimeTLSBundle(b.runtimeTLSBundle), nil
	}
	return b
}

// WithRuntimeTLSBundle supplies the client TLS bundle used to reach TLS-capable
// agent-runtimes. It may be called in any order relative to WithSandboxInfra
// because the infra builder is materialized lazily at Build time. A nil bundle
// (the default) keeps every runtime call on the legacy plaintext paths.
func (b *SandboxManagerBuilder) WithRuntimeTLSBundle(bundle *runtime.TLSBundle) *SandboxManagerBuilder {
	b.runtimeTLSBundle = bundle
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
	if err := sandboxid.ValidatePrefix(b.opts.ShortSandboxIDPrefix); err != nil {
		return nil, errors.NewError(errors.ErrorInternal, "short sandbox id prefix: %v", err)
	}
	if len(b.opts.ShortSandboxIDPrefix)+sandboxid.ShortIDLength > content.LabelValueMaxLength {
		return nil, errors.NewError(
			errors.ErrorInternal,
			"short sandbox id prefix %q is too long: prefix plus %d-character short ID must fit a %d-character label value, so the prefix may use at most %d characters",
			b.opts.ShortSandboxIDPrefix,
			sandboxid.ShortIDLength,
			content.LabelValueMaxLength,
			content.LabelValueMaxLength-sandboxid.ShortIDLength,
		)
	}
	if err := config.ValidateTrafficAccessTokenOptions(b.opts.TrafficAccessToken); err != nil {
		return nil, errors.NewError(errors.ErrorInternal, "invalid traffic access token options: %v", err)
	}
	// Build infra
	if b.buildInfraFunc == nil {
		return nil, errors.NewError(errors.ErrorInternal, "infra builder is not configured: call WithSandboxInfra or WithCustomInfra before Build")
	}
	builder, err := b.buildInfraFunc()
	if err != nil {
		return nil, errors.NewError(errors.ErrorInternal, "failed to get infra builder: %v", err)
	}
	b.instance.infra = builder.Build()
	routeSource := b.instance.infra.GetSandboxRouteSource()
	if routeSource == nil {
		return nil, errors.NewError(errors.ErrorInternal, "sandbox route source is not configured")
	}
	b.instance.routeSource = routeSource

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

	routeSource infra.SandboxRouteSource

	systemNamespace   string
	enableShortID     bool
	shortIDPrefix     string
	generateSandboxID func() (string, error)

	primary *primaryState
	elector *primaryElector

	quota            QuotaEnforcer          // nil until InitQuota or builder injection
	quotaAntiDrift   *quota.AntiDriftDriver // nil when Redis is not configured
	quotaRedisClient RedisClient            // nil when Redis is not configured

	trafficTokenOptions      identity.TokenOptions
	trafficTokenSingleflight *trafficTokenSingleflight
	trafficTokenIssues       trafficTokenIssueLifecycle
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

	if m.routeSource != nil {
		if err := m.routeSource.Subscribe(ctx, m.handleSandboxRouteEvent); err != nil {
			return fmt.Errorf("subscribe manager route feeder: %w", err)
		}
	}

	if m.elector != nil {
		go m.elector.Run(ctx)
	} else {
		m.primary.set(true)
	}

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
	m.trafficTokenIssues.init(ctx)
	if m.enableShortID {
		if err := m.initializeSandboxIDGenerator(ctx); err != nil {
			return err
		}
	}

	go func() {
		klog.InfoS("starting proxy")
		err := m.proxy.Run()
		if err != nil {
			klog.Error(err, "proxy stopped")
		}
	}()
	if m.quotaAntiDrift != nil {
		m.quotaAntiDrift.Run(ctx)
	}
	return nil
}

func (m *SandboxManager) initializeSandboxIDGenerator(ctx context.Context) error {
	workerID, err := m.allocateWorkerID(ctx, m.shortIDPrefix)
	if err != nil {
		return fmt.Errorf("allocate sandbox ID worker for prefix %q: %w", m.shortIDPrefix, err)
	}
	generator, err := sandboxid.NewGenerator(workerID)
	if err != nil {
		return err
	}
	m.generateSandboxID = generator
	klog.FromContext(ctx).Info("sandbox ID generator initialized", "workerID", workerID, "prefix", m.shortIDPrefix)
	return nil
}

func (m *SandboxManager) Stop(ctx context.Context) {
	log := klog.FromContext(ctx)
	if err := m.trafficTokenIssues.stop(ctx); err != nil {
		log.Error(err, "failed to stop traffic access token issuance")
	}
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
