package config

import (
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/utils"
	"k8s.io/client-go/rest"
)

const (
	// DefaultMemberlistBindPort is the default port for memberlist gossip
	DefaultMemberlistBindPort = 7946
)

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
	return opts
}
