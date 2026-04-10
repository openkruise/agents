package main

import (
	"context"
	"os"

	"github.com/envoyproxy/envoy/contrib/golang/common/go/api"
	envoyhttp "github.com/envoyproxy/envoy/contrib/golang/filters/http/source/go/pkg/http"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-gateway/controller"
	"github.com/openkruise/agents/pkg/sandbox-gateway/filter"
	peerserver "github.com/openkruise/agents/pkg/sandbox-gateway/server"
)

func init() {
	envoyhttp.RegisterHttpFilterFactoryAndConfigParser(
		"sandbox-gateway",
		filter.FilterFactory,
		&filter.ConfigParser{},
	)

	go func() {
		if err := controller.StartManager(context.Background()); err != nil {
			api.LogErrorf("sandbox controller manager exited with error: %v", err)
		}
	}()

	// Start the peer server for handling route synchronization from other peers
	go func() {
		ctx := context.Background()

		// Get Kubernetes config
		cfg, err := rest.InClusterConfig()
		if err != nil {
			api.LogErrorf("failed to get in-cluster config: %v", err)
			os.Exit(1)
		}

		// Create controller-runtime client for peer discovery
		scheme := runtime.NewScheme()
		utilruntime.Must(clientgoscheme.AddToScheme(scheme))
		utilruntime.Must(agentsv1alpha1.AddToScheme(scheme))
		client, err := ctrlclient.New(cfg, ctrlclient.Options{Scheme: scheme})
		if err != nil {
			api.LogErrorf("failed to create controller-runtime client: %v", err)
			os.Exit(1)
		}

		peerServer := peerserver.NewServer(client, proxy.SystemPort)
		if err := peerServer.Start(ctx); err != nil {
			api.LogErrorf("failed to start peer server: %v", err)
			os.Exit(1)
		}
	}()
}

func main() {}
