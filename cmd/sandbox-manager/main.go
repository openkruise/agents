/*
Copyright 2025.

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

package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"         // Added for pprof server
	_ "net/http/pprof" // #nosec -- intentional pprof endpoint for diagnostics
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/pflag"
	zapRaw "go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"k8s.io/klog/v2"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	agentsclient "github.com/openkruise/agents/client"
	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/servers/e2b"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/tracing"
	"github.com/openkruise/agents/pkg/utils"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
	utilruntime "github.com/openkruise/agents/pkg/utils/runtime"
)

const (
	E2BKeyStorageDSNEnvVar   = "E2B_KEY_STORAGE_DSN"
	E2BKeyHashPepperEnvVar   = "E2B_KEY_HASH_PEPPER"
	QuotaRedisUsernameEnvVar = "QUOTA_REDIS_USERNAME"
	QuotaRedisPasswordEnvVar = "QUOTA_REDIS_PASSWORD"
)

// validateE2BTimeoutFlags rejects misconfigurations that would either
// (a) make floor enforcement no-op or pathological (min <= 0), or
// (b) push effectiveTimeout past the user-facing maxTimeout ceiling.
func validateE2BTimeoutFlags(minResumeTimeout, maxTimeout int) error {
	if minResumeTimeout <= 0 {
		return fmt.Errorf("--e2b-min-resume-timeout must be greater than 0, got %d", minResumeTimeout)
	}
	if minResumeTimeout > maxTimeout {
		return fmt.Errorf(
			"--e2b-min-resume-timeout (%d) must not exceed --e2b-max-timeout (%d); "+
				"otherwise floor enforcement could bump a valid request past the API ceiling",
			minResumeTimeout, maxTimeout)
	}
	return nil
}

func main() {
	// Define variables for pprof configuration
	var enablePprof bool
	var pprofAddr string

	// Define variables for server configuration
	var port int
	var e2bAdminKey string
	var e2bEnableAuth bool
	var domain string
	var e2bMaxTimeout int
	var enableShortSandboxID bool
	var shortSandboxIDPrefix string
	var e2bMinResumeTimeout int
	var sysNs string
	var peerSelector string
	var sandboxNamespace string
	var sandboxLabelSelector string
	var maxClaimWorkers int
	var maxCreateQPS int
	var extProcMaxConcurrency int
	var kubeClientQPS float64
	var kubeClientBurst int
	var memberlistBindPort int
	var e2bKeyStorage string
	var e2bKeyStorageDisableAutoMigrate bool
	var quotaRedisAddr string
	var quotaRedisDB int
	var quotaRedisOperationTimeout time.Duration
	var quotaRedisBreakerN int
	var quotaRedisBreakerD time.Duration
	var quotaAntiDriftInterval time.Duration
	var quotaAntiDriftGrace time.Duration
	var runtimeClientCertSecret string
	var trafficTokenValidity time.Duration
	var trafficTokenMinValidity time.Duration
	var trafficTokenMaxValidity time.Duration

	utilfeature.DefaultMutableFeatureGate.AddFlag(pflag.CommandLine)

	// Register the new pprof flags
	pflag.BoolVar(&enablePprof, "enable-pprof", false, "Enable pprof profiling")
	pflag.StringVar(&pprofAddr, "pprof-addr", ":6060", "The address the pprof debug maps to.")

	// Register server configuration flags
	pflag.IntVar(&port, "port", 8080, "The port the server listens on")
	pflag.StringVar(&e2bAdminKey, "e2b-admin-key", "", "E2B admin API key (if empty, a random UUID will be generated)")
	pflag.BoolVar(&e2bEnableAuth, "e2b-enable-auth", true, "Enable E2B authentication")
	pflag.StringVar(&domain, "e2b-domain", "",
		"Static E2B domain. When empty, the domain is resolved per-request from "+
			"the HTTP Host header (api. prefix stripped for native paths; host "+
			"preserved for /kruise/* customized paths).")
	pflag.IntVar(&e2bMaxTimeout, "e2b-max-timeout", models.DefaultMaxTimeout, "E2B maximum timeout in seconds")
	pflag.BoolVar(&enableShortSandboxID, "enable-short-sandbox-id", false, "Assign short IDs to successfully claimed or cloned Sandboxes")
	pflag.StringVar(&shortSandboxIDPrefix, "short-sandbox-id-prefix", "",
		"Prefix prepended verbatim to newly assigned short Sandbox IDs when --enable-short-sandbox-id is set; "+
			"must start with a lowercase letter or digit and otherwise contain only lowercase letters, digits, or hyphens; "+
			"must not contain the legacy ID separator --; "+
			"at most 50 characters (validated at startup: prefix plus the 13-character short ID must fit a 63-character Kubernetes label value); "+
			"with Native E2B dynamic domains (<port>-<sandbox-id>.<domain>) keep the prefix at 44 characters or fewer so the DNS label stays valid; during mixed-version rollout keep it at 37 characters or fewer; the customized path is not subject to this DNS limit; "+
			"use the same value on every sandbox-manager replica")
	pflag.IntVar(&e2bMinResumeTimeout, "e2b-min-resume-timeout", models.DefaultMinResumeTimeoutSeconds,
		"Minimum value (seconds) for the timeout parameter carried by the E2B connect API; "+
			"timeout values below this floor will be raised to this value.")
	pflag.StringVar(&sysNs, "system-namespace", utils.DefaultSandboxDeployNamespace, "The namespace where the sandbox manager is running (required)")
	pflag.StringVar(&peerSelector, "peer-selector", "", "Peer selector for sandbox manager (required)")
	pflag.StringVar(&sandboxNamespace, "sandbox-namespace", "", "Namespace to filter sandbox-related custom resources (Sandbox, SandboxSet, Checkpoint, SandboxTemplate, TrafficPolicy). Defaults to all.")
	pflag.StringVar(&sandboxLabelSelector, "sandbox-label-selector", "", "Label selector to filter sandbox-related custom resources (Sandbox, SandboxSet, Checkpoint, SandboxTemplate, TrafficPolicy). Defaults to all.")
	pflag.IntVar(&maxClaimWorkers, "max-claim-workers", consts.DefaultClaimWorkers, "Maximum number of claim workers (0 uses default)")
	pflag.IntVar(&maxCreateQPS, "max-create-qps", consts.DefaultCreateQPS, "Maximum QPS for sandbox creation (0 uses default)")
	pflag.IntVar(&extProcMaxConcurrency, "ext-proc-max-concurrency", consts.DefaultExtProcConcurrency, "Maximum concurrency for external processor (0 uses default)")
	pflag.Float64Var(&kubeClientQPS, "kube-client-qps", 500, "QPS for Kubernetes client")
	pflag.IntVar(&kubeClientBurst, "kube-client-burst", 1000, "Burst for Kubernetes client")
	pflag.IntVar(&memberlistBindPort, "memberlist-bind-port", 7946, "Port for memberlist gossip (default 7946)")
	pflag.StringVar(&e2bKeyStorage, "e2b-key-storage", "secret",
		"Storage backend for E2B API keys. Valid values: 'secret' (K8s Secret, default), 'mysql' (MySQL via GORM). "+
			"When --e2b-key-storage=mysql and auth is enabled, both the MySQL DSN (env "+E2BKeyStorageDSNEnvVar+
			") and the HMAC key-hash pepper (env "+E2BKeyHashPepperEnvVar+") are required; secret mode does not use either.")
	pflag.BoolVar(&e2bKeyStorageDisableAutoMigrate, "e2b-key-storage-disable-schema-auto-update", false,
		"Disable schema auto-migration for DB-Based key storage like mysql; when enabled, schema changes are skipped but admin team/key bootstrap still runs")
	pflag.StringVar(&quotaRedisAddr, "quota-redis-addr", "", "Redis address for sandbox-manager quota enforcement. Empty disables enforcement and fails open.")
	pflag.IntVar(&quotaRedisDB, "quota-redis-db", 0, "Redis DB for sandbox-manager quota enforcement.")
	pflag.DurationVar(&quotaRedisOperationTimeout, "quota-redis-operation-timeout", consts.DefaultQuotaRedisOperationTimeout, "Per-operation timeout for Redis quota commands.")
	pflag.IntVar(&quotaRedisBreakerN, "quota-redis-breaker-max-failures", consts.DefaultQuotaRedisBreakerN, "Consecutive Redis quota backend errors required to open the fail-open breaker.")
	pflag.DurationVar(&quotaRedisBreakerD, "quota-redis-breaker-open-duration", consts.DefaultQuotaRedisBreakerD, "How long the Redis quota fail-open breaker stays open before probing again.")
	pflag.DurationVar(&quotaAntiDriftInterval, "quota-anti-drift-interval", consts.DefaultQuotaAntiDriftInterval, "Interval for quota anti-drift reconciliation.")
	pflag.DurationVar(&quotaAntiDriftGrace, "quota-anti-drift-grace", consts.DefaultQuotaAntiDriftGrace, "Grace period before periodic quota anti-drift releases suspected leaked entries.")
	pflag.StringVar(&runtimeClientCertSecret, "runtime-client-cert-secret", "",
		"namespace/name of the Secret holding the agent-runtime client TLS bundle. Leave it empty to disable the runtime mTLS.")
	pflag.DurationVar(&trafficTokenValidity, "traffic-access-token-validity", config.DefaultTrafficAccessTokenValidity, "Validity requested for traffic access tokens.")
	pflag.DurationVar(&trafficTokenMinValidity, "traffic-access-token-min-validity", config.DefaultTrafficAccessTokenMinValidity, "Minimum allowed traffic access token validity.")
	pflag.DurationVar(&trafficTokenMaxValidity, "traffic-access-token-max-validity", config.DefaultTrafficAccessTokenMaxValidity, "Maximum allowed traffic access token validity.")

	// Tracing flags (definitions shared with agent-sandbox-controller via
	// tracing.Config.BindFlags; pulled into pflag by AddGoFlagSet below)
	var tracingCfg tracing.Config
	tracingCfg.BindFlags(flag.CommandLine)

	opts := zap.Options{
		Development: false,
	}
	opts.BindFlags(flag.CommandLine)
	klog.InitFlags(nil)
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	pflag.Parse()

	klog.SetLogger(zap.New(
		zap.UseFlagOptions(&opts),
		zap.RawZapOpts(zapRaw.AddCaller()),
		zap.StacktraceLevel(zapcore.DPanicLevel),
	))

	// Start pprof server if enabled
	if enablePprof {
		go func() {
			klog.Infof("Starting pprof server on %s", pprofAddr)
			pprofServer := &http.Server{Addr: pprofAddr, ReadHeaderTimeout: 10 * time.Second}
			if err := pprofServer.ListenAndServe(); err != nil {
				klog.Errorf("Unable to start pprof server: %v", err)
			}
		}()
	}

	// Validate required flags
	if sysNs == "" {
		klog.Fatalf("--system-namespace is required")
	}

	if peerSelector == "" {
		klog.Fatalf("--peer-selector is required")
	}

	// Generate admin key if not provided
	if e2bAdminKey == "" {
		e2bAdminKey = uuid.NewString()
	}

	// Validate positive values
	if e2bMaxTimeout <= 0 {
		klog.Fatalf("--e2b-max-timeout must be greater than 0")
	}

	if err := validateE2BTimeoutFlags(e2bMinResumeTimeout, e2bMaxTimeout); err != nil {
		klog.Fatalf("invalid e2b timeout flags: %v", err)
	}
	if quotaRedisOperationTimeout <= 0 {
		klog.Fatalf("--quota-redis-operation-timeout must be greater than 0")
	}
	trafficTokenOpts := config.TrafficAccessTokenOptions{
		Validity: trafficTokenValidity, MinValidity: trafficTokenMinValidity, MaxValidity: trafficTokenMaxValidity,
	}

	if maxClaimWorkers < 0 {
		klog.Fatalf("--max-claim-workers must be non-negative")
	}

	if maxCreateQPS < 0 {
		klog.Fatalf("--max-create-qps must be non-negative")
	}

	if extProcMaxConcurrency < 0 {
		klog.Fatalf("--ext-proc-max-concurrency must be non-negative")
	}

	if kubeClientQPS <= 0 {
		klog.Fatalf("--kube-client-qps must be greater than 0")
	}

	if kubeClientBurst <= 0 {
		klog.Fatalf("--kube-client-burst must be greater than 0")
	}

	e2bKeyStorageDSN := strings.TrimSpace(os.Getenv(E2BKeyStorageDSNEnvVar))
	e2bKeyStoragePepper := strings.TrimSpace(os.Getenv(E2BKeyHashPepperEnvVar))
	quotaRedisUsername := strings.TrimSpace(os.Getenv(QuotaRedisUsernameEnvVar))
	quotaRedisPassword := strings.TrimSpace(os.Getenv(QuotaRedisPasswordEnvVar))

	quotaOpts := config.QuotaOptions{
		RedisAddr:         quotaRedisAddr,
		RedisUsername:     quotaRedisUsername,
		RedisPassword:     quotaRedisPassword,
		RedisDB:           quotaRedisDB,
		OperationTimeout:  quotaRedisOperationTimeout,
		BreakerN:          quotaRedisBreakerN,
		BreakerD:          quotaRedisBreakerD,
		AntiDriftInterval: quotaAntiDriftInterval,
		AntiDriftGrace:    quotaAntiDriftGrace,
	}

	// Initialize Kubernetes client and config
	clientConfig, err := clients.NewRestConfig(float32(kubeClientQPS), kubeClientBurst)
	if err != nil {
		klog.Fatalf("Failed to initialize Kubernetes client: %v", err)
	}

	// Initialize tracing
	tracingCfg.ServiceName = "sandbox-manager"
	tracingShutdown, err := tracing.InitTracerProvider(context.Background(), tracingCfg)
	if err != nil {
		klog.Fatalf("Failed to initialize tracing: %v", err)
	}
	defer func() {
		if err := tracingShutdown(context.Background()); err != nil {
			klog.Errorf("Failed to shutdown tracing: %v", err)
		}
	}()

	// Initialize the generic client registry so CRD discovery
	// (discovery.DiscoverGVK) works; the shared cache uses it to skip
	// optional CRD-dependent setup when a CRD is not installed.
	if err := agentsclient.NewRegistry(clientConfig); err != nil {
		klog.Fatalf("Failed to initialize generic client registry: %v", err)
	}

	// Load the runtime client TLS bundle. The certificate Secret is fetched once
	// at startup (fail fast on a broken reference) and held for the lifetime of
	// the process: the material is long-lived and static, so a replacement is
	// picked up by the next restart.
	var runtimeTLSBundle *utilruntime.TLSBundle
	if runtimeClientCertSecret != "" {
		secretNamespace, secretName, found := strings.Cut(runtimeClientCertSecret, "/")
		if !found || secretNamespace == "" || secretName == "" {
			klog.Fatalf("--runtime-client-cert-secret must be in namespace/name form, got %q", runtimeClientCertSecret)
		}
		secretReader, err := ctrlclient.New(clientConfig, ctrlclient.Options{})
		if err != nil {
			klog.Fatalf("Failed to create client for the runtime client TLS bundle: %v", err)
		}
		loadCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		runtimeTLSBundle, err = utilruntime.NewTLSBundleFromSecret(loadCtx, secretReader, secretNamespace, secretName)
		cancel()
		if err != nil {
			klog.Fatalf("Failed to load the runtime client TLS bundle: %v", err)
		}
		klog.InfoS("runtime client TLS enabled", "secret", runtimeClientCertSecret)
	} else {
		klog.InfoS("runtime client TLS disabled, using the legacy plaintext runtime paths")
	}

	var keyCfg *keys.Config
	if e2bEnableAuth {
		keyCfg = &keys.Config{
			Mode:               keys.StorageMode(e2bKeyStorage),
			Namespace:          sysNs,
			AdminKey:           e2bAdminKey,
			DSN:                e2bKeyStorageDSN,
			DisableAutoMigrate: e2bKeyStorageDisableAutoMigrate,
			Pepper:             e2bKeyStoragePepper,
		}
	}

	sandboxController := e2b.NewController(e2b.ControllerOptions{
		Domain:           domain,
		Port:             port,
		MaxTimeout:       e2bMaxTimeout,
		MinResumeTimeout: e2bMinResumeTimeout,
		KeyConfig:        keyCfg,
		Manager: config.SandboxManagerOptions{
			SystemNamespace:       sysNs,
			PeerSelector:          peerSelector,
			SandboxNamespace:      sandboxNamespace,
			SandboxLabelSelector:  sandboxLabelSelector,
			MaxClaimWorkers:       maxClaimWorkers,
			MaxCreateQPS:          maxCreateQPS,
			ExtProcMaxConcurrency: uint32(extProcMaxConcurrency),
			MemberlistBindPort:    memberlistBindPort,
			EnableShortSandboxID:  enableShortSandboxID,
			ShortSandboxIDPrefix:  shortSandboxIDPrefix,
			RestConfig:            clientConfig,
			Quota:                 quotaOpts,
			TrafficAccessToken:    trafficTokenOpts,
		},
		RuntimeTLSBundle: runtimeTLSBundle,
	})

	if err := sandboxController.Init(); err != nil {
		klog.Fatalf("Failed to initialize sandbox controller: %v", err)
	}

	// Start HTTP Server
	sandboxCtx, err := sandboxController.Run()
	if err != nil {
		klog.Fatalf("Failed to start sandbox controller: %v", err)
	}
	<-sandboxCtx.Done()
	klog.Info("Sandbox controller stopped")
}
