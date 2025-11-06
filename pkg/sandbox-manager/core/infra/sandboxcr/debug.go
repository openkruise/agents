package sandboxcr

import "github.com/openkruise/agents/pkg/sandbox-manager/core/consts"

type DebugInfo struct {
	Pending int
	Claimed int
	Total   int
}

func (i *Infra) LoadDebugInfo() map[string]any {
	infos := make(map[string]any)
	i.Pools.Range(func(key, value any) bool {
		pool := value.(*Pool)
		infos[pool.template.Name] = DebugInfo{
			Pending: int(pool.Status.pending.Load()),
			Claimed: int(pool.Status.claimed.Load()),
			Total:   int(pool.Status.total.Load()),
		}
		return true
	})
	infos["infra"] = consts.InfraSandboxCR
	return infos
}
