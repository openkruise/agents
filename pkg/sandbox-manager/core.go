package sandbox_manager

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func (m *SandboxManager) Run(ctx context.Context, sysNs, peerSelector string) error {
	log := klog.FromContext(ctx)
	go func() {
		klog.InfoS("starting proxy")
		err := m.proxy.Run()
		if err != nil {
			klog.Error(err, "proxy stopped")
		}
	}()
	// TODO peer system is not optimized
	var peerInited bool
	log.Info("start to find peers")
	for i := 0; i < 20; i++ {
		peerList, err := m.client.CoreV1().Pods(sysNs).List(ctx, metav1.ListOptions{
			LabelSelector: peerSelector,
		})
		if err != nil {
			return err
		}
		log.Info("peer pods listed", "num", len(peerList.Items))
		var peers []string
		for _, peer := range peerList.Items {
			ip := peer.Status.PodIP
			if ip == "" {
				log.Info("peer pod has no ip", "peer", peer.Name)
				continue
			}
			log.Info("try to say hello to peer", "peer", peer.Name, "ip", ip)
			if helloErr := m.proxy.HelloPeer(ip); helloErr == nil {
				peers = append(peers, ip)
				log.Info("found peer", "peer", peer.Name, "ip", ip)
			} else {
				log.Info("peer is not ready", "peer", peer.Name, "ip", ip, "error", helloErr)
			}
		}
		if len(peers) == len(peerList.Items) {
			log.Info("all peers are ready")
			for _, ip := range peers {
				m.proxy.SetPeer(ip)
			}
			peerInited = true
			break
		} else {
			log.Info("waiting for peers to start", "ready", len(peers), "total", len(peerList.Items))
			time.Sleep(6 * time.Second)
		}
	}
	if !peerInited {
		return fmt.Errorf("failed to init peers")
	}
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
