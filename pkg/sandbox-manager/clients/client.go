package clients

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	sandboxclient "github.com/openkruise/agents/client/clientset/versioned"
	sandboxfake "github.com/openkruise/agents/client/clientset/versioned/fake"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"k8s.io/klog/v2"
)

type K8sClient kubernetes.Interface
type SandboxClient sandboxclient.Interface

type ClientSet struct {
	K8sClient
	SandboxClient
	*rest.Config
}

func NewClientSetWithOptions(qps float32, burst int) (*ClientSet, error) {
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
	config.QPS = qps
	config.Burst = burst

	// Override with environment variables if set (for backward compatibility)
	if qpsStr := os.Getenv("KUBE_CLIENT_QPS"); qpsStr != "" {
		if qpsEnv, err := strconv.ParseFloat(qpsStr, 32); err == nil {
			config.QPS = float32(qpsEnv)
		}
	}
	if burstStr := os.Getenv("KUBE_CLIENT_BURST"); burstStr != "" {
		if burstEnv, err := strconv.Atoi(burstStr); err == nil {
			config.Burst = burstEnv
		}
	}
	client.Config = config
	klog.InfoS("client config", "qps", config.QPS, "burst", config.Burst)
	// Create the client
	client.K8sClient, err = kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create client: %w", err)
	}

	client.SandboxClient, err = sandboxclient.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create sandbox client: %w", err)
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
