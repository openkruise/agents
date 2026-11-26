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
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestControllerDrainRequiresContinuousIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := NewController(ControllerOptions{})
		sc.registerRoutes()
		request := func(method, path string, want int) {
			t.Helper()
			resp := httptest.NewRecorder()
			sc.server.Handler.ServeHTTP(resp, httptest.NewRequest(method, path, nil))
			require.Equal(t, want, resp.Code, "%s %s", method, path)
		}
		checkNonBusinessRequests := func() {
			for _, path := range []string{"/health", "/kruise/api/health", "/metrics"} {
				request(http.MethodGet, path, http.StatusOK)
			}
			request(http.MethodGet, "/missing", http.StatusNotFound)
			request(http.MethodGet, "/kruise/api/sandboxes", http.StatusMethodNotAllowed)
			request(http.MethodPost, "/kruise/api//sandboxes", http.StatusTemporaryRedirect)
		}
		businessPaths := []string{"/sandboxes", "/kruise/api/sandboxes"}

		drained := waitForTestDrain(sc)
		defer func() { <-drained }()
		time.Sleep(600 * time.Millisecond)
		requireDrainPending(t, drained)
		for _, path := range businessPaths {
			// The matched create handler rejects the empty request body before backend access.
			request(http.MethodPost, path, http.StatusBadRequest)
		}

		time.Sleep(600 * time.Millisecond)
		requireDrainPending(t, drained)
		checkNonBusinessRequests()
		time.Sleep(399 * time.Millisecond)
		requireDrainPending(t, drained)
		time.Sleep(time.Millisecond)
		requireDrainSealed(t, drained)
		for _, path := range businessPaths {
			request(http.MethodPost, path, http.StatusServiceUnavailable)
		}
		checkNonBusinessRequests()
	})
}

func TestControllerDrainWaitsForConcurrentHandlers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := NewController(ControllerOptions{})
		sc.handleBusiness("POST /long", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(2 * time.Minute)
			w.WriteHeader(http.StatusCreated)
		}))
		sc.handleBusiness("POST /short", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		request := func(path string) <-chan int {
			status := make(chan int, 1)
			go func() {
				resp := httptest.NewRecorder()
				sc.server.Handler.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, path, nil))
				status <- resp.Code
			}()
			return status
		}

		longStatus := request("/long")
		synctest.Wait()
		drained := waitForTestDrain(sc)
		time.Sleep(time.Second)
		require.Equal(t, http.StatusNoContent, <-request("/short"))
		time.Sleep(100 * time.Second)
		requireDrainPending(t, drained)
		require.Equal(t, http.StatusCreated, <-longStatus)
		time.Sleep(time.Second)
		requireDrainSealed(t, drained)
	})
}

func TestControllerDrainRetiresPinnedConnections(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := NewController(ControllerOptions{})
		sc.registerRoutes()
		sc.handleBusiness("POST /work", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		serve := func(method, path string) *http.Response {
			rec := httptest.NewRecorder()
			sc.server.Handler.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
			// Result reports the headers as written, not later mutations.
			return rec.Result()
		}

		resp := serve(http.MethodPost, "/work")
		require.Equal(t, http.StatusNoContent, resp.StatusCode)
		require.Empty(t, resp.Header.Get("Connection"), "normal service keeps connections reusable")

		drained := waitForTestDrain(sc)
		resp = serve(http.MethodPost, "/work")
		require.Equal(t, http.StatusNoContent, resp.StatusCode)
		require.Equal(t, "close", resp.Header.Get("Connection"),
			"a draining response must retire a connection pinned to this Pod")
		for _, path := range []string{"/health", "/kruise/api/health", "/metrics"} {
			resp = serve(http.MethodGet, path)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Empty(t, resp.Header.Get("Connection"), "%s carries no business traffic", path)
		}

		time.Sleep(time.Second)
		requireDrainSealed(t, drained)
		resp = serve(http.MethodPost, "/work")
		require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		require.Equal(t, "close", resp.Header.Get("Connection"),
			"a rejected request must not keep reusing a connection pinned to this Pod")
	})
}

func TestControllerDrainRacesWithAdmissionAtIdleDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := NewController(ControllerOptions{})
		sc.handleBusiness("POST /work", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(2 * time.Second)
			w.WriteHeader(http.StatusNoContent)
		}))
		status := make(chan int, 1)
		go func() {
			time.Sleep(time.Second)
			resp := httptest.NewRecorder()
			sc.server.Handler.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/work", nil))
			status <- resp.Code
		}()
		drained := waitForTestDrain(sc)

		time.Sleep(time.Second)
		synctest.Wait()
		select {
		case code := <-status:
			require.Equal(t, http.StatusServiceUnavailable, code)
			requireDrainSealed(t, drained)
		default:
			requireDrainPending(t, drained)
			time.Sleep(time.Second)
			requireDrainPending(t, drained)
			require.Equal(t, http.StatusNoContent, <-status)
			time.Sleep(time.Second)
			requireDrainSealed(t, drained)
		}
	})
}

func TestControllerDrainWaitsForCompensationAfterClientDisconnect(t *testing.T) {
	sc := NewController(ControllerOptions{})
	sc.manager = newStopProbeManager(t, func() {})
	entered, compensating := make(chan struct{}), make(chan struct{})
	compensationDone := make(chan struct{})
	defer close(compensationDone)
	sc.handleBusiness("POST /work", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(compensating)
		<-compensationDone
		w.WriteHeader(http.StatusCreated)
	}))
	sc.handleBusiness("GET /work", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	apiAddr := make(chan string, 1)
	sc.server.Addr = "127.0.0.1:0"
	sc.server.BaseContext = func(listener net.Listener) context.Context {
		apiAddr <- listener.Addr().String()
		return context.Background()
	}
	stop := make(chan os.Signal, 1)
	runCtx, err := sc.Run(stop)
	require.NoError(t, err)

	signalSent := false
	t.Cleanup(func() {
		if !signalSent {
			stop <- syscall.SIGTERM
		}
		select {
		case <-runCtx.Done():
		case <-time.After(5 * time.Second):
			t.Error("controller did not exit after compensation finished")
			require.NoError(t, sc.server.Close())
		}
	})

	var baseURL string
	select {
	case addr := <-apiAddr:
		baseURL = "http://" + addr
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP server did not start")
	}
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{}}
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/work", nil)
	require.NoError(t, err)
	requestErr := make(chan error, 1)
	go func() {
		resp, err := client.Do(req)
		if err == nil {
			err = resp.Body.Close()
		}
		requestErr <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("business handler did not start")
	}

	stop <- syscall.SIGTERM
	signalSent = true
	cancel()
	require.ErrorIs(t, <-requestErr, context.Canceled)
	select {
	case <-compensating:
	case <-time.After(3 * time.Second):
		t.Fatal("business handler did not observe client cancellation")
	}

	time.Sleep(1100 * time.Millisecond)
	resp, err := client.Get(baseURL + "/work")
	require.NoError(t, err, "listener shut down while compensation was still running")
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
}

func waitForTestDrain(sc *Controller) <-chan struct{} {
	drained := make(chan struct{})
	go func() {
		sc.activity.waitAndSeal()
		close(drained)
	}()
	synctest.Wait()
	return drained
}

func requireDrainPending(t *testing.T, drained <-chan struct{}) {
	t.Helper()
	synctest.Wait()
	select {
	case <-drained:
		t.Fatal("drain closed admission before a full second of business inactivity")
	default:
	}
}

func requireDrainSealed(t *testing.T, drained <-chan struct{}) {
	t.Helper()
	synctest.Wait()
	select {
	case <-drained:
	default:
		t.Fatal("drain did not close admission after a full second of business inactivity")
	}
}
