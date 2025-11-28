package clients

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	sandboxclient "github.com/openkruise/agents/client/clientset/versioned"
	sandboxfake "github.com/openkruise/agents/client/clientset/versioned/fake"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

type K8sClient kubernetes.Interface
type SandboxClient sandboxclient.Interface

type ClientSet struct {
	K8sClient
	SandboxClient
	*rest.Config
}

func NewClientSet(infra string) (*ClientSet, error) {
	client := &ClientSet{}
	// Try to use in-cluster config first (when running inside a Kubernetes pod)
	config, err := rest.InClusterConfig()
	if err != nil {
		// Fall back to kubeconfig file if not running in cluster
		var kubeconfig string

		// Check if kubeconfig is set in environment variable
		if kubeconfigEnv := os.Getenv("KUBECONFIG"); kubeconfigEnv != "" {
			kubeconfig = kubeconfigEnv
		} else {
			// Use default kubeconfig path
			if home := homedir.HomeDir(); home != "" {
				kubeconfig = filepath.Join(home, ".kube", "config")
			}
		}

		// Use the current context in kubeconfig
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("failed to build config from kubeconfig: %w", err)
		}
	}

	// Configure rate limiter to handle client-side throttling
	// These values can be adjusted based on your cluster's capacity and requirements
	// Default values are typically QPS=5, Burst=10 which might be too low for active applications
	// QPS (Queries Per Second): Maximum requests per second to the API server
	// Burst: Maximum burst requests allowed in a short period
	// For high-activity applications, increasing these can reduce client-side throttling
	// Be careful not to set these too high as it might overload the Kubernetes API server
	// These can be configured via environment variables:
	// KUBE_CLIENT_QPS (default: 50)
	// KUBE_CLIENT_BURST (default: 100)
	config.QPS = 50    // Default QPS
	config.Burst = 100 // Default Burst

	// Override with environment variables if set
	if qpsStr := os.Getenv("KUBE_CLIENT_QPS"); qpsStr != "" {
		if qps, err := strconv.ParseFloat(qpsStr, 32); err == nil {
			config.QPS = float32(qps)
		}
	}
	if burstStr := os.Getenv("KUBE_CLIENT_BURST"); burstStr != "" {
		if burst, err := strconv.Atoi(burstStr); err == nil {
			config.Burst = burst
		}
	}
	client.Config = config
	// Create the client
	client.K8sClient, err = kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create client: %w", err)
	}

	if infra == consts.InfraSandboxCR {
		client.SandboxClient, err = sandboxclient.NewForConfig(config)
		if err != nil {
			return nil, fmt.Errorf("failed to create sandbox client: %w", err)
		}
	}

	return client, nil
}

//goland:noinspection GoDeprecation
func NewFakeClientSet() *ClientSet {
	client := &ClientSet{}
	client.K8sClient = k8sfake.NewClientset()
	client.SandboxClient = sandboxfake.NewSimpleClientset()
	return client
}
