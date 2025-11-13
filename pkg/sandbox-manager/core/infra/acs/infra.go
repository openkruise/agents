package acs

import (
	"github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/events"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/infra/k8s"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type Infra struct {
	*k8s.Infra
}

func (i *Infra) convertToACSSandbox(sandbox infra.Sandbox) infra.Sandbox {
	if sandbox == nil {
		return nil
	}
	return &Sandbox{
		sandbox.(*k8s.Sandbox),
	}
}

func (i *Infra) convertToACSSandboxes(sandboxes []infra.Sandbox) []infra.Sandbox {
	if len(sandboxes) == 0 {
		return nil
	}
	list := make([]infra.Sandbox, 0, len(sandboxes))
	for _, s := range sandboxes {
		list = append(list, i.convertToACSSandbox(s))
	}
	return list
}

func (i *Infra) LoadDebugInfo() map[string]any {
	info := i.Infra.LoadDebugInfo()
	info["infra"] = "acs"
	return info
}

func (i *Infra) SelectSandboxes(options infra.SandboxSelectorOptions) ([]infra.Sandbox, error) {
	list, err := i.Infra.SelectSandboxes(options)
	return i.convertToACSSandboxes(list), err
}

func (i *Infra) GetSandbox(sandboxID string) (infra.Sandbox, error) {
	sbx, err := i.Infra.GetSandbox(sandboxID)
	return i.convertToACSSandbox(sbx), err
}

func (i *Infra) InjectTemplateMetadata() metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Labels: map[string]string{
			consts.LabelACS: "true",
		},
	}
}

func NewInfra(namespace string, templateDir string, eventer *events.Eventer, client kubernetes.Interface, restConfig *rest.Config) (*Infra, error) {
	i, err := k8s.NewInfra(namespace, templateDir, eventer, client, restConfig)
	return &Infra{i}, err
}
