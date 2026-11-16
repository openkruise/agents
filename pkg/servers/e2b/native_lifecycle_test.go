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
	"testing"
	"time"

	"github.com/openkruise/agents/pkg/servers/kruise"
	"github.com/stretchr/testify/require"
)

func TestControllerSecondAPIBindFailureClosesFirstListener(t *testing.T) {
	for _, address := range []string{"127.0.0.1:0", "not-a-bind-address"} {
		t.Run(address, func(t *testing.T) {
			occupied, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			defer occupied.Close()
			nativeAddress := address
			if address == "127.0.0.1:0" {
				nativeAddress = occupied.Addr().String()
			}
			first, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			firstAddress := first.Addr().String()
			require.NoError(t, first.Close())
			sc := &Controller{server: &http.Server{Addr: firstAddress}, nativeAPI: kruise.NewController(nil, nil, nativeAddress)}
			require.Error(t, sc.startAPIServers())
			rebound, err := net.Listen("tcp", firstAddress)
			require.NoError(t, err, "E2B listener must be released synchronously")
			require.NoError(t, rebound.Close())
		})
	}
}

func TestControllerShutdownDrainsAPIsBeforeCompletion(t *testing.T) {
	first, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	nativeAddress := first.Addr().String()
	require.NoError(t, first.Close())
	entered, release := make(chan struct{}), make(chan struct{})
	sc := &Controller{
		server: &http.Server{Addr: "127.0.0.1:0", Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			close(entered)
			<-release
			w.WriteHeader(http.StatusOK)
		})},
		nativeAPI: kruise.NewController(nil, nil, nativeAddress),
	}
	require.NoError(t, sc.nativeAPI.Start())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	serveDone := make(chan error, 1)
	go func() { serveDone <- sc.server.Serve(listener) }()
	requestDone := make(chan error, 1)
	go func() {
		response, err := http.Get("http://" + listener.Addr().String())
		if err == nil {
			err = response.Body.Close()
		}
		requestDone <- err
	}()
	<-entered
	finished := make(chan struct{})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	go sc.shutdown(ctx, func() { close(finished) })
	require.Eventually(t, func() bool {
		connection, err := net.DialTimeout("tcp", nativeAddress, 100*time.Millisecond)
		if err == nil {
			require.NoError(t, connection.Close())
		}
		return err != nil
	}, time.Second, 10*time.Millisecond, "native listener must shut down during E2B drain")
	select {
	case <-finished:
		t.Fatal("shutdown completed before active E2B request drained")
	default:
	}
	close(release)
	require.NoError(t, <-requestDone)
	<-finished
	require.ErrorIs(t, <-serveDone, http.ErrServerClosed)
}
