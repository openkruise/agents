/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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
