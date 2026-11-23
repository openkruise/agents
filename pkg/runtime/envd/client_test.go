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

package envd

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/openkruise/agents/proto/envd/process"
	"github.com/openkruise/agents/proto/envd/process/processconnect"
)

type recordingProcess struct {
	processconnect.UnimplementedProcessHandler
	starts  chan *connect.Request[process.StartRequest]
	signals chan *connect.Request[process.SendSignalRequest]
	lists   chan *connect.Request[process.ListRequest]
}

func (p *recordingProcess) Start(_ context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
	p.starts <- req
	return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_Start{Start: &process.ProcessEvent_StartEvent{Pid: 42}}}})
}

func (p *recordingProcess) SendSignal(_ context.Context, req *connect.Request[process.SendSignalRequest]) (*connect.Response[process.SendSignalResponse], error) {
	p.signals <- req
	if req.Msg.GetProcess().GetPid() == 404 {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("process not found"))
	}
	return connect.NewResponse(&process.SendSignalResponse{}), nil
}

func (p *recordingProcess) List(_ context.Context, req *connect.Request[process.ListRequest]) (*connect.Response[process.ListResponse], error) {
	p.lists <- req
	switch req.Header().Get("X-Access-Token") {
	case "authorized":
		return connect.NewResponse(&process.ListResponse{Processes: []*process.ProcessInfo{{Pid: 42, Config: &process.ProcessConfig{Envs: map[string]string{"PRIVATE_VALUE": "must-not-be-read"}}}}}), nil
	case "unavailable":
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("runtime unavailable"))
	default:
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid token"))
	}
}

func nativeClient(t *testing.T, handler *recordingProcess) Client {
	t.Helper()
	_, native := processconnect.NewProcessHandler(handler)
	server := httptest.NewServer(native)
	t.Cleanup(server.Close)
	return Client{BaseURL: server.URL, HTTP: server.Client()}
}

func requireAuthorization(t *testing.T, headers http.Header, token, user string) {
	t.Helper()
	require.Equal(t, token, headers.Get("X-Access-Token"))
	r := &http.Request{Header: headers}
	actual, password, ok := r.BasicAuth()
	if user == "" {
		require.False(t, ok)
		return
	}
	require.True(t, ok)
	require.Equal(t, user, actual)
	require.Empty(t, password)
}

func TestClientStartExecutionTimeout(t *testing.T) {
	for _, tt := range []struct {
		name             string
		executionTimeout time.Duration
		callerDeadline   bool
		wantTimeout      string
	}{
		{name: "no process deadline"},
		{name: "caller deadline does not become execution deadline", callerDeadline: true},
		{name: "execution deadline survives shorter caller deadline", executionTimeout: 90 * time.Second, callerDeadline: true, wantTimeout: "90000"},
		{name: "positive sub-millisecond deadline", executionTimeout: time.Microsecond, wantTimeout: "1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handler := &recordingProcess{starts: make(chan *connect.Request[process.StartRequest], 1)}
			client := nativeClient(t, handler)
			ctx := t.Context()
			if tt.callerDeadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
			}
			cwd := "/home/user"
			cfg := &process.ProcessConfig{Cmd: "/bin/bash", Args: []string{"-l", "-c", "echo hello"}, Cwd: &cwd, Envs: map[string]string{"TEST": "value"}}
			stream, err := client.Start(ctx, "runtime-token", "user", cfg, tt.executionTimeout)
			require.NoError(t, err)
			require.True(t, stream.Receive())
			require.EqualValues(t, 42, stream.Msg().GetEvent().GetStart().GetPid())
			require.False(t, stream.Receive())
			require.NoError(t, stream.Err())
			require.NoError(t, stream.Close())
			received := <-handler.starts
			requireAuthorization(t, received.Header(), "runtime-token", "user")
			require.Nil(t, received.Msg.Tag)
			require.True(t, proto.Equal(cfg, received.Msg.Process))
			require.Equal(t, "50", received.Header().Get("Keepalive-Ping-Interval"))
			require.Equal(t, tt.wantTimeout, received.Header().Get("Connect-Timeout-Ms"))
		})
	}
}

func TestClientKillUsesPID(t *testing.T) {
	for _, tt := range []struct {
		name string
		pid  uint32
		code connect.Code
	}{
		{name: "signal accepted", pid: 42},
		{name: "native missing process", pid: 404, code: connect.CodeNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handler := &recordingProcess{signals: make(chan *connect.Request[process.SendSignalRequest], 1)}
			client := nativeClient(t, handler)
			err := client.Kill(t.Context(), "runtime-token", "root", tt.pid)
			if tt.code == 0 {
				require.NoError(t, err)
			} else {
				require.Equal(t, tt.code, connect.CodeOf(err))
			}
			received := <-handler.signals
			requireAuthorization(t, received.Header(), "runtime-token", "root")
			require.IsType(t, &process.ProcessSelector_Pid{}, received.Msg.GetProcess().GetSelector())
			require.Equal(t, tt.pid, received.Msg.GetProcess().GetPid())
			require.Equal(t, process.Signal_SIGNAL_SIGKILL, received.Msg.GetSignal())
		})
	}
}

func TestClientProbeListStatus(t *testing.T) {
	for _, tt := range []struct {
		token  string
		status int
	}{
		{token: "authorized", status: http.StatusOK},
		{token: "invalid", status: http.StatusUnauthorized},
		{token: "unavailable", status: http.StatusServiceUnavailable},
	} {
		t.Run(tt.token, func(t *testing.T) {
			handler := &recordingProcess{lists: make(chan *connect.Request[process.ListRequest], 1)}
			client := nativeClient(t, handler)
			status, err := client.ProbeList(t.Context(), tt.token)
			require.NoError(t, err)
			require.Equal(t, tt.status, status)
			received := <-handler.lists
			requireAuthorization(t, received.Header(), tt.token, "")
			require.Equal(t, "application/json", received.Header().Get("Content-Type"))
			require.Equal(t, "1", received.Header().Get("Connect-Protocol-Version"))
		})
	}
}

func TestClientProbeListClosesWithoutReadingBody(t *testing.T) {
	closed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(closed)
	}))
	defer server.Close()
	client := Client{BaseURL: server.URL, HTTP: server.Client()}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	status, err := client.ProbeList(ctx, "runtime-token")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal("probe kept the response body open")
	}
}

func TestClientProbeListCancellation(t *testing.T) {
	entered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return
		}
		close(entered)
		<-r.Context().Done()
	}))
	defer server.Close()
	client := Client{BaseURL: server.URL, HTTP: server.Client()}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	go func() {
		select {
		case <-entered:
			cancel()
		case <-ctx.Done():
		}
	}()
	status, err := client.ProbeList(ctx, "runtime-token")
	require.Zero(t, status)
	require.ErrorIs(t, err, context.Canceled)
}

func TestClientFilesRequest(t *testing.T) {
	for _, tt := range []struct {
		name, user, path string
	}{
		{name: "batch upload omits path", user: "user"},
		{name: "download preserves literal path", user: "root", path: "~/a b/file.txt"},
		{name: "empty username remains present"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := Client{BaseURL: "http://127.0.0.1:49983/"}
			req, err := client.FilesRequest(t.Context(), http.MethodPost, "runtime-token", tt.user, tt.path, nil)
			require.NoError(t, err)
			require.Equal(t, "/files", req.URL.Path)
			require.Equal(t, []string{tt.user}, req.URL.Query()["username"])
			require.Equal(t, tt.path != "", req.URL.Query().Has("path"))
			require.Equal(t, tt.path, req.URL.Query().Get("path"))
			requireAuthorization(t, req.Header, "runtime-token", tt.user)
		})
	}
}
