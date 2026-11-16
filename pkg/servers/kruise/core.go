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

package kruise

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/servers/apiauth"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/web"
	"k8s.io/klog/v2"
)

// Controller serves the native API using dependencies owned by the process.
type Controller struct {
	manager  *sandboxmanager.SandboxManager
	keys     keys.KeyStorage
	mux      *http.ServeMux
	server   *http.Server
	listener net.Listener
}

func NewController(manager *sandboxmanager.SandboxManager, storage keys.KeyStorage, address string) *Controller {
	sc := &Controller{manager: manager, keys: storage, mux: http.NewServeMux()}
	sc.registerSessionServiceRoutes()
	sc.server = &http.Server{Addr: address, Handler: sc.mux, ReadHeaderTimeout: 5 * time.Second}
	return sc
}

func (sc *Controller) Handler() http.Handler { return sc.mux }

func (sc *Controller) checkAPIKey(ctx context.Context, r *http.Request) (context.Context, *web.ApiError) {
	return apiauth.Authenticate(ctx, r, sc.keys)
}

// Start binds synchronously so the process can unwind an earlier listener on failure.
func (sc *Controller) Start() error {
	listener, err := net.Listen("tcp", sc.server.Addr)
	if err != nil {
		return fmt.Errorf("listen for Kruise API on %s: %w", sc.server.Addr, err)
	}
	sc.listener = listener
	go func() {
		if err := sc.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			klog.Fatalf("Kruise API server failed: %v", err)
		}
	}()
	return nil
}

// Shutdown drains API requests without stopping shared Manager or key storage.
func (sc *Controller) Shutdown(ctx context.Context) error {
	err := sc.server.Shutdown(ctx)
	// Shutdown can race Serve before it registers its listener.
	if sc.listener != nil {
		if closeErr := sc.listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			return errors.Join(err, fmt.Errorf("close Kruise API listener: %w", closeErr))
		}
	}
	return err
}
