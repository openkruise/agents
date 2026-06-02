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
	"time"

	"github.com/envoyproxy/envoy/contrib/golang/common/go/api"
	envoyhttp "github.com/envoyproxy/envoy/contrib/golang/filters/http/source/go/pkg/http"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-gateway/controller"
	"github.com/openkruise/agents/pkg/sandbox-gateway/filter"
	peerserver "github.com/openkruise/agents/pkg/sandbox-gateway/server"
	"github.com/openkruise/agents/pkg/sandbox-gateway/wake"
)

// systemKeyEnsureTimeout bounds the blocking initial read of the system-key
// Secret during package init. If the Secret is not populated within this window
// (e.g. the manager is down), the gateway exits so the pod restarts and the
// failure is visible, instead of init() blocking forever and never registering
// the Envoy filter factory.
const systemKeyEnsureTimeout = 2 * time.Minute

func init() {
	ctx := context.Background()

	// Reuse the controller manager's direct API reader for the gateway's
	// one-shot reads (system-key Secret, peer Pod listing) instead of building
	// a separate standalone client. GetAPIReader bypasses the cache and is
	// usable before mgr.Start, which the blocking system-key read requires.
	mgr, err := controller.NewManager()
	if err != nil {
		api.LogErrorf("failed to create sandbox controller manager: %v", err)
		os.Exit(1)
	}
	reader := mgr.GetAPIReader()

	registerWaker(ctx, reader)

	envoyhttp.RegisterHttpFilterFactoryAndConfigParser(
		"sandbox-gateway",
		filter.FilterFactory,
		&filter.ConfigParser{},
	)

	go func() {
		if err := mgr.Start(ctx); err != nil {
			api.LogErrorf("sandbox controller manager exited with error: %v", err)
		}
	}()

	// Start the peer server for handling route synchronization from other peers
	go func() {
		peerServer := peerserver.NewServer(reader, proxy.SystemPort)
		if err := peerServer.Start(ctx); err != nil {
			api.LogErrorf("failed to start peer server: %v", err)
			os.Exit(1)
		}
	}()
}

func main() {}

func registerWaker(ctx context.Context, client ctrlclient.Reader) {
	managerURL := os.Getenv(filter.EnvManagerE2BBaseURL)
	if managerURL == "" {
		api.LogErrorf("%s must be set for sandbox-gateway wake-on-traffic", filter.EnvManagerE2BBaseURL)
		os.Exit(1)
	}
	api.LogInfof("using manager URL [%s] for sandbox-gateway wake-on-traffic", managerURL)
	namespace := os.Getenv(peerserver.EnvNamespace)
	if namespace == "" {
		namespace = filter.DefaultSystemNamespace
	}
	reader := &wake.SystemKeyReader{
		Reader:    client,
		Namespace: namespace,
		Backoff:   5 * time.Second,
	}
	ensureCtx, cancel := context.WithTimeout(ctx, systemKeyEnsureTimeout)
	defer cancel()
	key, err := reader.WaitForKey(ensureCtx)
	if err != nil {
		api.LogErrorf("failed to read gateway system key within %s: %v", systemKeyEnsureTimeout, err)
		os.Exit(1)
	}
	connectClient, err := wake.NewConnectClient(managerURL, key)
	if err != nil {
		api.LogErrorf("failed to create gateway wake client: %v", err)
		os.Exit(1)
	}
	filter.RegisterWaker(wake.NewWaker(connectClient))
}
