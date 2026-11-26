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

package e2b

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/servers/web"
	"github.com/openkruise/agents/pkg/tracing"
)

// requestActivity belongs to one controller. The signal receiver is the sole waiter.
type requestActivity struct {
	mu     sync.Mutex
	active int
	// draining: shutdown started; business admission stays open while the
	// responses ask clients to retire their connections.
	draining bool
	// sealed: admission closed; business requests get 503.
	sealed    bool
	idle      *sync.Cond
	idleSince time.Time
}

// admit reports whether the request may proceed and whether the drain has
// started, in which case the response must tell the client to retire the
// connection.
func (a *requestActivity) admit() (admitted, draining bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sealed {
		return false, a.draining
	}
	a.active++
	return true, a.draining
}

// beginTask transfers a share of the parent handler's activity to detached
// work. Register before launching it, while the parent is still admitted.
func (a *requestActivity) beginTask() func() {
	a.mu.Lock()
	a.active++
	a.mu.Unlock()
	return a.finish
}

func (a *requestActivity) finish() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.active--
	if a.active == 0 && a.draining {
		a.idleSince = time.Now()
		a.idle.Signal()
	}
}

// known-limit: a quiet window can precede ingress convergence under sparse
// traffic. Deployments needing more propagation time can delay TERM via preStop.
//
// Draining starts before the idle wait and business responses then advertise
// Connection: close. A keep-alive connection pinned to this Pod (through a
// Service ClusterIP) would otherwise keep feeding it business traffic until
// the process is killed, so the idle window could never close.
func (a *requestActivity) waitAndSeal() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.idle = sync.NewCond(&a.mu)
	a.idleSince = time.Now()
	a.draining = true
	for {
		for a.active != 0 {
			a.idle.Wait()
		}
		remaining := time.Until(a.idleSince.Add(time.Second))
		if remaining <= 0 {
			a.sealed = true
			return
		}
		a.mu.Unlock()
		time.Sleep(remaining)
		a.mu.Lock()
		// Even a request that started and finished during the sleep moves idleSince.
	}
}

func (a *requestActivity) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		admitted, draining := a.admit()
		if draining {
			// Retire the connection after this response so a client that pinned it
			// to this Pod through a Service reconnects and gets routed again instead
			// of keeping the drain busy.
			w.Header().Set("Connection", "close")
		}
		if !admitted {
			id := r.Header.Get("X-Request-ID")
			if id == "" {
				id = tracing.NewRequestID()
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			if err := json.NewEncoder(w).Encode(web.ApiError{Code: http.StatusServiceUnavailable, Message: "sandbox manager is shutting down", RequestID: id}); err != nil {
				klog.FromContext(r.Context()).Error(err, "Failed to write shutdown response")
			}
			return
		}
		defer a.finish()
		next.ServeHTTP(w, r)
	})
}
