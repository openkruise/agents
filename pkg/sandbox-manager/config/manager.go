package config

import (
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/utils"
)

type SandboxManagerOptions struct {
	SystemNamespace       string
	MaxClaimWorkers       int
	ExtProcMaxConcurrency uint32
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
	return opts
}
