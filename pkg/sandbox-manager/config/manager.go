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
	"time"

	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/utils"
	"k8s.io/client-go/rest"
)

const (
	// DefaultMemberlistBindPort is the default port for memberlist gossip
	DefaultMemberlistBindPort = 7946
)

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
	SystemNamespace            string
	PeerSelector               string
	SandboxNamespace           string
	SandboxLabelSelector       string
	MaxClaimWorkers            int
	MaxCreateQPS               int
	ExtProcMaxConcurrency      uint32
	MemberlistBindPort         int
	DisableRouteReconciliation bool
	RestConfig                 *rest.Config
	Quota                      QuotaOptions
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
	// Quota defaults
	if opts.Quota.OperationTimeout <= 0 {
		opts.Quota.OperationTimeout = 50 * time.Millisecond
	}
	if opts.Quota.BreakerN <= 0 {
		opts.Quota.BreakerN = 3
	}
	if opts.Quota.BreakerD <= 0 {
		opts.Quota.BreakerD = 30 * time.Second
	}
	if opts.Quota.AntiDriftInterval <= 0 {
		opts.Quota.AntiDriftInterval = 5 * time.Minute
	}
	if opts.Quota.AntiDriftGrace <= 0 {
		opts.Quota.AntiDriftGrace = 10 * time.Minute
	}
	return opts
}
