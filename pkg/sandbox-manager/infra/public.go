package infra

import (
	"sync"

	"github.com/openkruise/agents/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type BaseInfra struct {
	Namespace   string
	TemplateDir string
	Pools       sync.Map
}

func (i *BaseInfra) GetPoolByObject(sbx metav1.Object) (pool SandboxPool, ok bool) {
	poolName := sbx.GetLabels()[v1alpha1.LabelSandboxPool]
	return i.GetPoolByTemplate(poolName)
}

func (i *BaseInfra) GetPoolByTemplate(name string) (pool SandboxPool, ok bool) {
	p, ok := i.Pools.Load(name)
	if ok {
		pool = p.(SandboxPool)
	}
	return
}

func (i *BaseInfra) AddPool(name string, pool SandboxPool) {
	i.Pools.Store(name, pool)
}
