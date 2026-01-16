package sandbox_manager

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
)

// means 2 min timeout
const (
	PeerInitInterval   = 6 * time.Second
	PeerInitMaxRetries = 20
)

type SandboxManager struct {
	Namespace string

	client *clients.ClientSet

	infra infra.Infrastructure
	proxy *proxy.Server
}

// NewSandboxManager creates a new SandboxManager instance.
func NewSandboxManager(client *clients.ClientSet, adapter proxy.RequestAdapter) (*SandboxManager, error) {
	m := &SandboxManager{
		client: client,
		proxy:  proxy.NewServer(adapter),
	}
	var err error
	m.infra, err = sandboxcr.NewInfra(client.SandboxClient, client.K8sClient, m.proxy)
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
	for i := 0; i < PeerInitMaxRetries; i++ {
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
			time.Sleep(PeerInitInterval)
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
	m.proxy.Stop()
	m.infra.Stop()
}

func (m *SandboxManager) GetInfra() infra.Infrastructure {
	return m.infra
}
