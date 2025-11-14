package core

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/openkruise/agents/pkg/sandbox-manager/core/clients"
	consts2 "github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/events"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/infra/acs"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/infra/k8s"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/logs"
	proxy2 "github.com/openkruise/agents/pkg/sandbox-manager/core/proxy"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

var DebugLevel = 5

type SandboxManager struct {
	Namespace   string
	templateDir string

	client     *clients.ClientSet
	restConfig *rest.Config

	infra infra.Infrastructure
	proxy *proxy2.Server

	eventer *events.Eventer
	timers  sync.Map
}

// NewSandboxManager creates a new SandboxManager instance.
//
//	Params:
//	- namespace: The namespace where the helm and all managed sandbox pods are running
//	- templateDir: The directory where the built-in sandbox templates are stored
//	- client / restConfig: The k8s client and rest config
//	- adapter: The request adapter for mapping helm logic to a specific framework like 'e2b'
//	- debug: run in prod or debug mode (debug mode is useful in developing, making it possible to run locally)
func NewSandboxManager(namespace, templateDir string, client *clients.ClientSet, restConfig *rest.Config,
	adapter proxy2.RequestAdapter, infra string) (*SandboxManager, error) {
	eventer := events.NewEventer()

	m := &SandboxManager{
		client:      client,
		restConfig:  restConfig,
		Namespace:   namespace,
		templateDir: templateDir,
		eventer:     eventer,
		proxy:       proxy2.NewServer(adapter),
	}
	var err error
	switch infra {
	case consts2.InfraACS:
		m.infra, err = acs.NewInfra(namespace, templateDir, eventer, client.K8sClient, restConfig)
	case consts2.InfraK8S:
		m.infra, err = k8s.NewInfra(namespace, templateDir, eventer, client.K8sClient, restConfig)
	case consts2.InfraSandboxCR:
		m.infra, err = sandboxcr.NewInfra(namespace, templateDir, eventer, client.SandboxClient)
	default:
		err = fmt.Errorf("infra must be one of: [%s, %s, %s, %s]",
			consts2.InfraACS, consts2.InfraK8S, consts2.InfraSandboxCR, consts2.InfraMicroVM)
	}
	return m, err
}

func (m *SandboxManager) Run(ctx context.Context) error {
	m.RegisterHandler(consts2.SandboxCreated, "DefaultOnSandboxCreated", m.handleSandboxCreated, nil)
	m.RegisterHandler(consts2.SandboxKill, "DefaultOnSandboxKill", m.handleSandboxKill, nil)
	go func() {
		klog.InfoS("starting proxy")
		err := m.proxy.Run()
		if err != nil {
			klog.Error(err, "proxy stopped")
		}
	}()
	go func() {
		ticker := time.NewTicker(time.Minute)
		for {
			select {
			case <-ticker.C:
				m.RefreshProxy(logs.NewContext())
			}
		}
	}()
	if err := m.infra.Run(ctx); err != nil {
		return err
	}
	if err := m.recoverTimers(ctx); err != nil {
		return err
	}
	return nil
}

func (m *SandboxManager) Stop() {
	m.infra.Stop()
}

func (m *SandboxManager) RegisterHandler(evt consts2.EventType, name string, handleFunc events.HandleFunc, onError events.OnErrorFunc) {
	m.eventer.RegisterHandler(evt, &events.Handler{
		Name:        name,
		HandleFunc:  handleFunc,
		OnErrorFunc: onError,
	})
}

func (m *SandboxManager) GetInfra() infra.Infrastructure {
	return m.infra
}

func (m *SandboxManager) RefreshProxy(ctx context.Context) {
	log := klog.FromContext(ctx).V(DebugLevel)
	routes := m.proxy.ListRoutes()
	for _, route := range routes {
		sbx, err := m.infra.GetSandbox(route.ID)
		if err != nil {
			log.Info("removing route for sandbox not exist", "id", route.ID)
			m.proxy.DeleteRoute(route.ID)
		} else {
			owner := sbx.GetOwnerUser()
			ip := sbx.GetIP()
			if ip != route.IP || owner != route.Owner || !reflect.DeepEqual(sbx.GetRouteHeader(), route.ExtraHeaders) {
				log.Info("updating route for sandbox changed", "id", route.ID,
					"oldIP", route.IP, "newIP", ip, "oldOwner", route.Owner, "newOwner", owner)
				route.IP = ip
				route.Owner = owner
				route.ExtraHeaders = sbx.GetRouteHeader()
				m.proxy.SetRoute(route.ID, route)
			}
		}
	}
}
