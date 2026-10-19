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

package opensandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	"github.com/openkruise/agents/pkg/runtime/envd"
	"github.com/openkruise/agents/proto/envd/process"
	"github.com/openkruise/agents/proto/envd/process/processconnect"
)

// Initialization/authentication and remote cancellation are tested below because
// SDK happy-path E2E cannot reliably exercise their ordering and disconnect races.
func TestInitializationAuthentication(t *testing.T) {
	var initialized atomic.Bool
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if initialized.Load() && r.Header.Get("X-Access-Token") != "runtime-token" {
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(200)
	}))
	defer native.Close()
	state := filepath.Join(t.TempDir(), "token")
	s, err := New(envd.Client{BaseURL: native.URL, HTTP: native.Client()}, state)
	require.NoError(t, err)
	for _, tt := range []struct {
		name, method, path, body, token string
		init                            bool
		status                          int
	}{
		{name: "pre-init is closed", method: "POST", path: "/command", body: `{"command":"echo unsafe"}`, status: 503},
		{name: "unprotected envd cannot authorize init", method: "POST", path: "/init", body: `{"accessToken":"runtime-token"}`, status: 503},
		{name: "wrong init token", method: "POST", path: "/init", body: `{"accessToken":"wrong-token"}`, init: true, status: 401},
		{name: "native token binds service", method: "POST", path: "/init", body: `{"accessToken":"runtime-token"}`, init: true, status: 204},
		{name: "missing credential", method: "GET", path: "/policy", init: true, status: 401},
		{name: "wrong credential", method: "GET", path: "/policy", token: "wrong-token", init: true, status: 401},
		{name: "explicit unsupported operation", method: "GET", path: "/policy", token: "runtime-token", init: true, status: 501},
		{name: "explicit unsupported vault", method: "POST", path: "/credential-vault", token: "runtime-token", init: true, status: 501},
		{name: "explicit unsupported vault credential", method: "GET", path: "/credential-vault/credentials/example", token: "runtime-token", init: true, status: 501},
	} {
		t.Run(tt.name, func(t *testing.T) {
			initialized.Store(tt.init)
			r := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			r.Header.Set(executionHeader, tt.token)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			require.Equal(t, tt.status, w.Code, w.Body.String())
		})
	}
	restored, err := New(s.backend, state)
	require.NoError(t, err)
	require.Equal(t, "runtime-token", restored.token)
	info, err := os.Stat(state)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
}

type cancellationProcess struct {
	processconnect.UnimplementedProcessHandler
	killed chan string
}

func (p *cancellationProcess) Start(ctx context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
	if req.Header().Get("X-Access-Token") != "runtime-token" {
		return connect.NewError(connect.CodeUnauthenticated, nil)
	}
	if err := stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_Start{Start: &process.ProcessEvent_StartEvent{Pid: 42}}}}); err != nil {
		return err
	}
	if req.Msg.Process.GetArgs()[1] == "exit 127" {
		message := "exit status 127"
		return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{ExitCode: 127, Exited: true, Error: &message}}}})
	}
	<-ctx.Done()
	return ctx.Err()
}

func (p *cancellationProcess) SendSignal(_ context.Context, req *connect.Request[process.SendSignalRequest]) (*connect.Response[process.SendSignalResponse], error) {
	p.killed <- req.Msg.Process.GetTag()
	return connect.NewResponse(&process.SendSignalResponse{}), nil
}

func TestCommandStreamBoundary(t *testing.T) {
	for _, mode := range []string{"disconnect", "timeout", "interrupt", "nonzero"} {
		t.Run(mode, func(t *testing.T) {
			processHandler := &cancellationProcess{killed: make(chan string, 1)}
			_, nativeHandler := processconnect.NewProcessHandler(processHandler)
			native := httptest.NewServer(nativeHandler)
			defer native.Close()
			state := filepath.Join(t.TempDir(), "token")
			require.NoError(t, os.WriteFile(state, []byte("runtime-token"), 0600))
			handler, err := New(envd.Client{BaseURL: native.URL, HTTP: native.Client()}, state)
			require.NoError(t, err)
			server := httptest.NewServer(handler)
			defer server.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			body := `{"command":"sleep 60"}`
			if mode == "nonzero" {
				body = `{"command":"exit 127"}`
			}
			if mode == "timeout" {
				body = `{"command":"sleep 60","timeout":250}`
			}
			r, err := http.NewRequestWithContext(ctx, "POST", server.URL+"/command", strings.NewReader(body))
			require.NoError(t, err)
			r.Header.Set(executionHeader, "runtime-token")
			resp, err := server.Client().Do(r)
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, 200, resp.StatusCode)
			reader := bufio.NewReader(resp.Body)
			line, err := reader.ReadString('\n')
			require.NoError(t, err)
			var init streamEvent
			require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &init))
			require.Equal(t, "init", init.Type)
			switch mode {
			case "nonzero":
				remaining, err := io.ReadAll(reader)
				require.NoError(t, err)
				require.Contains(t, string(remaining), `"evalue":"127"`)
				require.Contains(t, string(remaining), `"type":"execution_complete"`)
				return
			case "disconnect":
				cancel()
			case "timeout":
				_, err = io.Copy(io.Discard, resp.Body)
				require.NoError(t, err)
			case "interrupt":
				interrupt, err := http.NewRequestWithContext(t.Context(), "DELETE", server.URL+"/command?id="+init.Text, nil)
				require.NoError(t, err)
				interrupt.Header.Set(executionHeader, "runtime-token")
				stopped, err := server.Client().Do(interrupt)
				require.NoError(t, err)
				require.NoError(t, stopped.Body.Close())
				require.Equal(t, 204, stopped.StatusCode)
			}
			select {
			case id := <-processHandler.killed:
				require.Equal(t, init.Text, id)
			case <-time.After(5 * time.Second):
				t.Fatal("native process was not signalled after cancellation")
			}
		})
	}
}
