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
	"net"
	"net/http"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/openkruise/agents/pkg/servers/e2b"
	"github.com/openkruise/agents/pkg/servers/kruise"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateE2BTimeoutFlags(t *testing.T) {
	cases := []struct {
		name        string
		maxTimeout  int
		expectError string
	}{
		{name: "ok-default", maxTimeout: 2592000, expectError: ""},
		{name: "ok-small", maxTimeout: 60, expectError: ""},
		{name: "zero", maxTimeout: 0, expectError: "--e2b-max-timeout must be greater than 0"},
		{name: "negative", maxTimeout: -1, expectError: "--e2b-max-timeout must be greater than 0"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateE2BTimeoutFlags(tc.maxTimeout)
			if tc.expectError == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.expectError)
			}
		})
	}
}

func TestValidateMetricsPort(t *testing.T) {
	cases := []struct {
		name               string
		metricsPort        int
		controlPort        int
		memberlistBindPort int
		expectError        string
	}{
		{name: "shared-zero", controlPort: 8080, memberlistBindPort: 7946},
		{name: "shared-control-listener", metricsPort: 8080, controlPort: 8080, memberlistBindPort: 7946},
		{name: "min-valid", metricsPort: 1, controlPort: 8080, memberlistBindPort: 7946},
		{name: "max-valid", metricsPort: 65535, controlPort: 8080, memberlistBindPort: 7946},
		{name: "negative", metricsPort: -1, controlPort: 8080, memberlistBindPort: 7946, expectError: "valid TCP port"},
		{name: "too-large", metricsPort: 65536, controlPort: 8080, memberlistBindPort: 7946, expectError: "valid TCP port"},
		{name: "matches-default-memberlist-port", metricsPort: 7946, controlPort: 8080, memberlistBindPort: 7946, expectError: "must differ from --memberlist-bind-port"},
		{name: "matches-custom-memberlist-port", metricsPort: 9000, controlPort: 8080, memberlistBindPort: 9000, expectError: "must differ from --memberlist-bind-port"},
		{name: "matches-normalized-default-memberlist-port", metricsPort: 7946, controlPort: 8080, memberlistBindPort: 0, expectError: "must differ from --memberlist-bind-port"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateMetricsPort(tc.metricsPort, tc.controlPort, tc.memberlistBindPort)
			if tc.expectError == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.expectError)
		})
	}
}

func TestRunControllersPropagatesStartupFailure(t *testing.T) {
	controller := e2b.NewController(e2b.ControllerOptions{})
	ctx, err := runControllers(controller, "127.0.0.1:0", make(chan os.Signal, 1))
	require.ErrorContains(t, err, "ConfigSource is required")
	require.Error(t, ctx.Err())
	require.Nil(t, controller.Manager(), "native API must not start before shared dependencies initialize")
}

func TestRunControllersSignalDuringStartup(t *testing.T) {
	stop := make(chan os.Signal, 1)
	stop <- syscall.SIGTERM
	controller := e2b.NewController(e2b.ControllerOptions{})
	ctx, err := runControllers(controller, "127.0.0.1:0", stop)
	// A queued startup error can win over the signal, just as in E2B Run.
	if err != nil {
		require.ErrorContains(t, err, "ConfigSource is required")
	}
	require.Error(t, ctx.Err())
}

func TestShutdownControllersWaitsForNativeRequests(t *testing.T) {
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := reservation.Addr().String()
	require.NoError(t, reservation.Close())
	native := kruise.NewController(nil, nil, address)
	entered, release := make(chan struct{}), make(chan struct{})
	native.Handler().(*http.ServeMux).HandleFunc("GET /blocking", func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	})
	require.NoError(t, native.Start())
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	t.Cleanup(func() { require.NoError(t, native.Shutdown(ctx)) })
	requestDone := make(chan error, 1)
	go func() {
		response, err := http.Get("http://" + address + "/blocking")
		if err == nil {
			err = response.Body.Close()
		}
		requestDone <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("native request did not start")
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		shutdownControllers(ctx, e2b.NewController(e2b.ControllerOptions{}), native)
	}()
	require.Eventually(t, func() bool {
		connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			require.NoError(t, connection.Close())
		}
		return err != nil
	}, time.Second, time.Millisecond)
	select {
	case <-finished:
		t.Fatal("process shutdown completed before the native request drained")
	default:
	}
	close(release)
	require.NoError(t, <-requestDone)
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("process shutdown did not finish after the native request drained")
	}
}
