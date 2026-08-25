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

package config

import (
	"fmt"
	"time"

	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/utils"
	"k8s.io/client-go/rest"
)

const (
	// DefaultMemberlistBindPort is the default port for memberlist gossip
	DefaultMemberlistBindPort = 7946
	// DefaultTrafficAccessTokenValidity preserves the existing traffic-token
	// lifetime while clients migrate to automatic refresh.
	DefaultTrafficAccessTokenValidity = 100 * 365 * 24 * time.Hour
	// DefaultTrafficAccessTokenMinValidity prevents unusably short tokens.
	DefaultTrafficAccessTokenMinValidity = 5 * time.Minute
	// DefaultTrafficAccessTokenMaxValidity is the default upper bound for the
	// configured traffic access token validity.
	DefaultTrafficAccessTokenMaxValidity = DefaultTrafficAccessTokenValidity
)

// TrafficAccessTokenOptions defines sandbox-manager's traffic-token lifetime
// policy. API callers cannot override these values.
type TrafficAccessTokenOptions struct {
	Validity    time.Duration
	MinValidity time.Duration
	MaxValidity time.Duration
}

// QuotaOptions holds runtime configuration for API-key quota enforcement.
type QuotaOptions struct {
	RedisAddr         string
	RedisUsername     string
	RedisPassword     string
	RedisDB           int
	OperationTimeout  time.Duration
	BreakerN          int
	BreakerD          time.Duration
	AntiDriftInterval time.Duration
	AntiDriftGrace    time.Duration
}

type SandboxManagerOptions struct {
	SystemNamespace       string
	PeerSelector          string
	SandboxNamespace      string
	SandboxLabelSelector  string
	MaxClaimWorkers       int
	MaxCreateQPS          int
	ExtProcMaxConcurrency uint32
	MemberlistBindPort    int
	EnableShortSandboxID  bool
	ShortSandboxIDPrefix  string
	RestConfig            *rest.Config
	Quota                 QuotaOptions
	TrafficAccessToken    TrafficAccessTokenOptions
}

func InitOptions(opts SandboxManagerOptions) SandboxManagerOptions {
	if opts.SystemNamespace == "" {
		opts.SystemNamespace = utils.DefaultSandboxDeployNamespace
	}
	if opts.MaxClaimWorkers <= 0 {
		opts.MaxClaimWorkers = consts.DefaultClaimWorkers
	}
	if opts.ExtProcMaxConcurrency <= 0 {
		opts.ExtProcMaxConcurrency = consts.DefaultExtProcConcurrency
	}
	if opts.MaxCreateQPS <= 0 {
		opts.MaxCreateQPS = consts.DefaultCreateQPS
	}
	if opts.MemberlistBindPort <= 0 {
		opts.MemberlistBindPort = DefaultMemberlistBindPort
	}
	if opts.TrafficAccessToken.Validity <= 0 {
		opts.TrafficAccessToken.Validity = DefaultTrafficAccessTokenValidity
	}
	if opts.TrafficAccessToken.MinValidity <= 0 {
		opts.TrafficAccessToken.MinValidity = DefaultTrafficAccessTokenMinValidity
	}
	if opts.TrafficAccessToken.MaxValidity <= 0 {
		opts.TrafficAccessToken.MaxValidity = DefaultTrafficAccessTokenMaxValidity
	}
	// Quota defaults
	if opts.Quota.OperationTimeout <= 0 {
		opts.Quota.OperationTimeout = consts.DefaultQuotaRedisOperationTimeout
	}
	if opts.Quota.BreakerN <= 0 {
		opts.Quota.BreakerN = consts.DefaultQuotaRedisBreakerN
	}
	if opts.Quota.BreakerD <= 0 {
		opts.Quota.BreakerD = consts.DefaultQuotaRedisBreakerD
	}
	if opts.Quota.AntiDriftInterval <= 0 {
		opts.Quota.AntiDriftInterval = consts.DefaultQuotaAntiDriftInterval
	}
	if opts.Quota.AntiDriftGrace <= 0 {
		opts.Quota.AntiDriftGrace = consts.DefaultQuotaAntiDriftGrace
	}
	return opts
}

// ValidateTrafficAccessTokenOptions rejects inconsistent lifetime policy.
func ValidateTrafficAccessTokenOptions(opts TrafficAccessTokenOptions) error {
	if opts.MinValidity <= 0 {
		return fmt.Errorf("traffic access token minimum validity must be positive")
	}
	if opts.MaxValidity < opts.MinValidity {
		return fmt.Errorf("traffic access token maximum validity must not be less than minimum validity")
	}
	if opts.Validity < opts.MinValidity || opts.Validity > opts.MaxValidity {
		return fmt.Errorf("traffic access token validity must be between %s and %s", opts.MinValidity, opts.MaxValidity)
	}
	return nil
}
