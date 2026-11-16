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
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNativeAPIBindFailure(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer occupied.Close()
	native := NewController(nil, nil, occupied.Addr().String())
	require.ErrorContains(t, native.Start(), "listen for Kruise API")
}

func TestNativeAPIShutdownDrainsRequests(t *testing.T) {
	native := NewController(nil, nil, "127.0.0.1:0")
	entered, release := make(chan struct{}), make(chan struct{})
	native.mux.HandleFunc("GET /blocking", func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	})
	listener, err := net.Listen("tcp", native.server.Addr)
	require.NoError(t, err)
	served := make(chan error, 1)
	go func() { served <- native.server.Serve(listener) }()
	requestDone := make(chan error, 1)
	go func() {
		response, err := http.Get("http://" + listener.Addr().String() + "/blocking")
		if err == nil {
			err = response.Body.Close()
		}
		requestDone <- err
	}()
	<-entered
	shutdownDone := make(chan error, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	go func() { shutdownDone <- native.Shutdown(ctx) }()
	select {
	case err := <-shutdownDone:
		t.Fatalf("shutdown returned before request drained: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-requestDone)
	require.NoError(t, <-shutdownDone)
	require.ErrorIs(t, <-served, http.ErrServerClosed)
}

func TestNativeAPIImmediateShutdownReleasesListener(t *testing.T) {
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := reservation.Addr().String()
	require.NoError(t, reservation.Close())
	native := NewController(nil, nil, address)
	require.NoError(t, native.Start())
	require.NoError(t, native.Shutdown(t.Context()))
	rebound, err := net.Listen("tcp", address)
	require.NoError(t, err)
	require.NoError(t, rebound.Close())
}
