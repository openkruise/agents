package sandbox_manager

import (
	"context"
	"fmt"
	"net"
	"os"

	"github.com/google/uuid"
	"github.com/openkruise/agents/pkg/peers"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

const (
	// MemberlistBindPort is the default port for memberlist gossip
	MemberlistBindPort = 7946
)

type SandboxManager struct {
	Namespace string

	client       *clients.ClientSet
	peersManager peers.Peers

	infra infra.Infrastructure
	proxy *proxy.Server
}

// NewSandboxManager creates a new SandboxManager instance.
func NewSandboxManager(client *clients.ClientSet, adapter proxy.RequestAdapter, opts config.SandboxManagerOptions) (*SandboxManager, error) {
	opts = config.InitOptions(opts)
	klog.InfoS("sandbox-manager options", "options", opts)

	// Create peers manager with memberlist
	nodeName := os.Getenv("HOSTNAME")
	if nodeName == "" {
		nodeName = os.Getenv("POD_NAME")
	}
	if nodeName == "" {
		nodeName = uuid.NewString()
	}
	peersManager := peers.NewMemberlistPeers(nodeName)

	m := &SandboxManager{
		client:       client,
		peersManager: peersManager,
		proxy:        proxy.NewServer(adapter, peersManager, opts),
	}
	var err error
	m.infra, err = sandboxcr.NewInfra(client, client.K8sClient, m.proxy, opts)
	return m, err
}

func getFirstNonLoopbackIP() string {
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, addr := range addresses {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}
	return ""
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

	// Get pod IP for memberlist binding
	podIP := os.Getenv("POD_IP")
	if podIP == "" {
		podIP = getFirstNonLoopbackIP()
	}
	if podIP == "" {
		return fmt.Errorf("failed to determine local IP for memberlist")
	}

	// Get existing peers from Kubernetes API for initial join
	log.Info("discovering existing peers for memberlist join", "podIP", podIP)
	peerList, err := m.client.CoreV1().Pods(sysNs).List(ctx, metav1.ListOptions{
		LabelSelector: peerSelector,
	})
	if err != nil {
		return fmt.Errorf("failed to list peer pods: %w", err)
	}

	// Build list of existing peer IPs for initial join
	existingPeers := make([]string, 0)
	for _, peer := range peerList.Items {
		ip := peer.Status.PodIP
		if ip == "" || ip == podIP {
			continue
		}
		// Memberlist uses the bind port for gossip
		existingPeers = append(existingPeers, fmt.Sprintf("%s:%d", ip, MemberlistBindPort))
	}
	log.Info("found existing peers for memberlist join", "count", len(existingPeers))

	// Start memberlist
	if err := m.peersManager.Start(ctx, podIP, MemberlistBindPort, existingPeers); err != nil {
		return fmt.Errorf("failed to start memberlist: %w", err)
	}
	log.Info("memberlist started successfully")

	if err := m.infra.Run(ctx); err != nil {
		return err
	}
	return nil
}

func (m *SandboxManager) Stop(ctx context.Context) {
	log := klog.FromContext(ctx)
	m.proxy.Stop(ctx)
	m.infra.Stop(ctx)
	if m.peersManager != nil {
		if err := m.peersManager.Stop(); err != nil {
			log.Error(err, "failed to stop peers manager")
		}
	}
}

func (m *SandboxManager) GetInfra() infra.Infrastructure {
	return m.infra
}
