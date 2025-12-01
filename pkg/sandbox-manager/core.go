package sandbox_manager

import (
	"context"
	"fmt"
	"sync"

	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"k8s.io/klog/v2"
)

type SandboxManager struct {
	Namespace string

	client *clients.ClientSet

	infra infra.Infrastructure
	proxy *proxy.Server

	timers sync.Map
}

// NewSandboxManager creates a new SandboxManager instance.
//
//	Params:
//	- namespace: The namespace where the helm and all managed sandbox pods are running
//	- templateDir: The directory where the built-in sandbox templates are stored
//	- client / restConfig: The k8s client and rest config
//	- adapter: The request adapter for mapping helm logic to a specific framework like 'e2b'
//	- debug: run in prod or debug mode (debug mode is useful in developing, making it possible to run locally)
func NewSandboxManager(namespace string, client *clients.ClientSet, adapter proxy.RequestAdapter, infra string) (*SandboxManager, error) {
	m := &SandboxManager{
		client:    client,
		Namespace: namespace,
		proxy:     proxy.NewServer(adapter),
	}
	var err error
	switch infra {
	case consts.InfraSandboxCR:
		m.infra, err = sandboxcr.NewInfra(namespace, client.SandboxClient, m.proxy)
	default:
		err = fmt.Errorf("infra must be one of: [%s]",
			consts.InfraSandboxCR)
	}
	return m, err
}

func (m *SandboxManager) Run(ctx context.Context) error {
	go func() {
		klog.InfoS("starting proxy")
		err := m.proxy.Run()
		if err != nil {
			klog.Error(err, "proxy stopped")
		}
	}()
	if err := m.infra.Run(ctx); err != nil {
		return err
	}
	return nil
}

func (m *SandboxManager) Stop() {
	m.infra.Stop()
}

func (m *SandboxManager) GetInfra() infra.Infrastructure {
	return m.infra
}
