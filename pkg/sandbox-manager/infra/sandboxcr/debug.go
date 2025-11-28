package sandboxcr

import (
	"context"

	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type DebugInfo struct {
	Pending int
	Claimed int
	Total   int
}

func (i *Infra) LoadDebugInfo() map[string]any {
	infos := make(map[string]any)
	i.Pools.Range(func(key, value any) bool {
		pool := value.(*Pool)
		infos[pool.Name] = pool.LoadDebugInfo()
		return true
	})
	infos["infra"] = consts.InfraSandboxCR
	return infos
}

func (p *Pool) LoadDebugInfo() map[string]any {
	sbs, err := p.client.ApiV1alpha1().SandboxSets(p.Namespace).Get(context.Background(), p.Name, metav1.GetOptions{})
	if err != nil {
		return map[string]any{
			"error": err.Error(),
		}
	}
	return map[string]any{
		"total":     sbs.Status.Replicas,
		"available": sbs.Status.AvailableReplicas,
	}
}
