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
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
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

// Generated RPC handlers exercise the real transport while controlling native
// stream failures and signal timing that successful SDK calls cannot induce.
type commandTestProcess struct {
	processconnect.UnimplementedProcessHandler
	start   func(context.Context, *connect.Request[process.StartRequest], *connect.ServerStream[process.StartResponse]) error
	signal  func(context.Context, *connect.Request[process.SendSignalRequest]) error
	starts  atomic.Int32
	signals atomic.Int32
}

func (p *commandTestProcess) Start(ctx context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
	p.starts.Add(1)
	return p.start(ctx, req, stream)
}

func (p *commandTestProcess) SendSignal(ctx context.Context, req *connect.Request[process.SendSignalRequest]) (*connect.Response[process.SendSignalResponse], error) {
	p.signals.Add(1)
	if p.signal != nil {
		if err := p.signal(ctx, req); err != nil {
			return nil, err
		}
	}
	return connect.NewResponse(&process.SendSignalResponse{}), nil
}

func commandTestRequest(r *http.Request) *http.Request {
	// The admitted snapshot deliberately differs from the mutable external
	// header, so these handler tests also catch accidental header re-reads.
	r.Header.Set(executionHeader, "external-header-is-not-the-snapshot")
	binding := newCredentialBinding("command-test-token", 1)
	return r.WithContext(context.WithValue(r.Context(), bindingContextKey{}, binding))
}

func newCommandTestServer(t *testing.T, native *commandTestProcess) (*Server, *httptest.Server) {
	t.Helper()
	_, nativeHandler := processconnect.NewProcessHandler(native)
	nativeServer := httptest.NewServer(nativeHandler)
	t.Cleanup(nativeServer.Close)
	s := &Server{backend: envd.Client{BaseURL: nativeServer.URL, HTTP: nativeServer.Client()}}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = commandTestRequest(r)
		if r.Method == http.MethodDelete {
			s.interruptCommand(w, r)
			return
		}
		s.runCommand(w, r)
	}))
	t.Cleanup(api.Close)
	return s, api
}

func commandStartEvent(pid uint32) *process.StartResponse {
	return &process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_Start{
		Start: &process.ProcessEvent_StartEvent{Pid: pid},
	}}}
}

func commandDataEvent(stderr bool, data []byte) *process.StartResponse {
	event := &process.ProcessEvent_DataEvent{Output: &process.ProcessEvent_DataEvent_Stdout{Stdout: data}}
	if stderr {
		event.Output = &process.ProcessEvent_DataEvent_Stderr{Stderr: data}
	}
	return &process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_Data{Data: event}}}
}

func commandEndEvent(exit int32, exited bool, message *string) *process.StartResponse {
	return &process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_End{
		End: &process.ProcessEvent_EndEvent{ExitCode: exit, Exited: exited, Error: message},
	}}}
}

func commandEvents(t *testing.T, body io.Reader) []streamEvent {
	t.Helper()
	var events []streamEvent
	scanner := bufio.NewScanner(body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event streamEvent
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event))
		events = append(events, event)
	}
	require.NoError(t, scanner.Err())
	return events
}

func commandPost(t *testing.T, api *httptest.Server, body string) *http.Response {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, api.URL+"/command", strings.NewReader(body))
	require.NoError(t, err)
	resp, err := api.Client().Do(r)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resp.Body.Close()) })
	return resp
}

func TestCommandRequestMapping(t *testing.T) {
	for _, tt := range []struct {
		name, body, timeout, cmd string
		args                     []string
	}{
		{name: "omitted timeout and login shell", body: `{"command":"echo $A","cwd":"/tmp/request dir","envs":{"A":"quoted value"}}`, timeout: "60000", cmd: "/bin/bash", args: []string{"-l", "-c", "echo $A"}},
		{name: "zero timeout", body: `{"command":"true","timeout":0}`, cmd: "/bin/bash", args: []string{"-l", "-c", "true"}},
		{name: "null timeout", body: `{"command":"true","timeout":null}`, cmd: "/bin/bash", args: []string{"-l", "-c", "true"}},
		{name: "explicit timeout and literal argv", body: `{"argv":["printf","%s","two words","$HOME"],"timeout":5000}`, timeout: "5000", cmd: "printf", args: []string{"%s", "two words", "$HOME"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			type observation struct {
				request  *process.StartRequest
				headers  http.Header
				deadline bool
			}
			observed := make(chan observation, 1)
			native := &commandTestProcess{start: func(ctx context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
				_, deadline := ctx.Deadline()
				observed <- observation{req.Msg, req.Header().Clone(), deadline}
				if err := stream.Send(commandStartEvent(42)); err != nil {
					return err
				}
				return stream.Send(commandEndEvent(0, true, nil))
			}}
			_, api := newCommandTestServer(t, native)
			resp := commandPost(t, api, tt.body)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			events := commandEvents(t, resp.Body)
			require.Len(t, events, 3)
			require.Equal(t, "init", events[0].Type)
			require.Equal(t, "42", events[0].Text)
			require.Equal(t, "status", events[1].Type)
			require.Equal(t, "execution_complete", events[2].Type)
			require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
			require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
			require.Equal(t, "no", resp.Header.Get("X-Accel-Buffering"))
			got := <-observed
			require.Nil(t, got.request.Tag)
			require.Equal(t, tt.cmd, got.request.Process.Cmd)
			require.Equal(t, tt.args, got.request.Process.Args)
			require.Equal(t, tt.timeout, got.headers.Get("Connect-Timeout-Ms"))
			require.Equal(t, tt.timeout != "", got.deadline)
			require.Equal(t, "command-test-token", got.headers.Get("X-Access-Token"))
			require.Equal(t, "Basic dXNlcjo=", got.headers.Get("Authorization"))
			if tt.name == "omitted timeout and login shell" {
				require.Equal(t, "/tmp/request dir", got.request.Process.GetCwd())
				require.Equal(t, map[string]string{"A": "quoted value"}, got.request.Process.Envs)
			}
			require.Zero(t, native.signals.Load())
		})
	}
}

func TestCommandRejectsInvalidOrUnsupportedRequests(t *testing.T) {
	native := &commandTestProcess{start: func(context.Context, *connect.Request[process.StartRequest], *connect.ServerStream[process.StartResponse]) error {
		return errors.New("invalid requests must not start a process")
	}}
	_, api := newCommandTestServer(t, native)
	for _, tt := range []struct {
		body   string
		status int
	}{
		{`{`, 400}, {`{}`, 400}, {`{"command":""}`, 400}, {`{"argv":[]}`, 400},
		{`{"command":"true","argv":["true"]}`, 400}, {`{"argv":["\u0000"]}`, 400},
		{`{"command":"true","timeout":-1}`, 400}, {`{"command":"true","timeout":1.1}`, 400},
		{`{"command":"true","timeout":"1"}`, 400}, {`{"command":"true","timeout":true}`, 400},
		{`{"command":"true","timeout":{}}`, 400}, {`{"command":"true","timeout":9223372036855}`, 400},
		{`{"command":"true","timeout":9223372036854775808}`, 400},
		{`{"command":"true","background":true}`, 501}, {`{"command":"true","uid":0}`, 501}, {`{"command":"true","gid":0}`, 501},
	} {
		t.Run(tt.body, func(t *testing.T) {
			resp := commandPost(t, api, tt.body)
			require.Equal(t, tt.status, resp.StatusCode)
			require.Equal(t, "application/json", resp.Header.Get("Content-Type"))
		})
	}
	require.Zero(t, native.starts.Load())
	require.Zero(t, native.signals.Load())
}

func TestCommandStartupErrorsRemainHTTP(t *testing.T) {
	for _, tt := range []struct {
		name   string
		first  *process.StartResponse
		err    error
		status int
	}{
		{name: "RPC invalid argument", err: connect.NewError(connect.CodeInvalidArgument, errors.New("invalid process")), status: 400},
		{name: "RPC unavailable", err: connect.NewError(connect.CodeUnavailable, errors.New("not available")), status: 503},
		{name: "RPC authentication", err: connect.NewError(connect.CodeUnauthenticated, errors.New("wrong token")), status: 401},
		{name: "EOF before start", status: 502},
		{name: "data before start", first: commandDataEvent(false, []byte("not started")), status: 502},
		{name: "zero PID", first: commandStartEvent(0), status: 502},
		{name: "empty event", first: &process.StartResponse{}, status: 502},
	} {
		t.Run(tt.name, func(t *testing.T) {
			native := &commandTestProcess{start: func(_ context.Context, _ *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
				if tt.first != nil {
					return stream.Send(tt.first)
				}
				return tt.err
			}}
			_, api := newCommandTestServer(t, native)
			resp := commandPost(t, api, `{"command":"true"}`)
			require.Equal(t, tt.status, resp.StatusCode)
			require.Equal(t, "application/json", resp.Header.Get("Content-Type"))
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NotContains(t, string(body), "data: ")
			require.Zero(t, native.signals.Load())
		})
	}
}

func TestCommandStreamCompletionAndUTF8(t *testing.T) {
	nonzero := "exit status 127"
	abnormal := "native process failure"
	numericError := "23"
	for _, tt := range []struct {
		name, stdout, stderr, failure, value string
		messages                             []*process.StartResponse
		err                                  error
	}{
		{name: "split channels and quoted output", stdout: "中\n\"text\"", stderr: "🙂", messages: []*process.StartResponse{
			commandDataEvent(false, []byte{0xe4}), commandDataEvent(true, []byte{0xf0, 0x9f}),
			commandDataEvent(false, []byte{0xb8, 0xad, '\n', '"', 't', 'e', 'x', 't', '"'}), commandDataEvent(true, []byte{0x99, 0x82}), commandEndEvent(0, true, nil),
		}},
		{name: "invalid subsequence and trailing bytes", stdout: "�A�", messages: []*process.StartResponse{
			commandDataEvent(false, []byte{0xe1, 0x80}), commandDataEvent(false, []byte{'A', 0xe4, 0xb8}), commandEndEvent(0, true, nil),
		}},
		{name: "numeric nonzero exit", failure: "CommandExecutionError", value: "127", messages: []*process.StartResponse{commandEndEvent(127, true, &nonzero)}},
		{name: "abnormal process", failure: runtimeErrorName, value: "native process error: " + abnormal, messages: []*process.StartResponse{commandEndEvent(0, false, &abnormal)}},
		{name: "numeric abnormal process error is not an exit code", failure: runtimeErrorName, value: "native process error: 23", messages: []*process.StartResponse{commandEndEvent(0, false, &numericError)}},
		{name: "zero exit with numeric error is not success", failure: runtimeErrorName, value: "native process error: 23", messages: []*process.StartResponse{commandEndEvent(0, true, &numericError)}},
		{name: "EOF flushes tail", stdout: "�", failure: runtimeErrorName, messages: []*process.StartResponse{commandDataEvent(false, []byte{0xe4, 0xb8})}},
		{name: "RPC failure flushes tail", stderr: "�", failure: runtimeErrorName, messages: []*process.StartResponse{commandDataEvent(true, []byte{0xe4})}, err: connect.NewError(connect.CodeInternal, errors.New("transport failed"))},
		{name: "duplicate start", failure: runtimeErrorName, messages: []*process.StartResponse{commandStartEvent(43)}},
		{name: "empty event after start", failure: runtimeErrorName, messages: []*process.StartResponse{{}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			native := &commandTestProcess{start: func(_ context.Context, _ *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
				if err := stream.Send(commandStartEvent(42)); err != nil {
					return err
				}
				if err := stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_Keepalive{Keepalive: &process.ProcessEvent_KeepAlive{}}}}); err != nil {
					return err
				}
				for _, message := range tt.messages {
					if err := stream.Send(message); err != nil {
						return err
					}
				}
				return tt.err
			}}
			_, api := newCommandTestServer(t, native)
			resp := commandPost(t, api, `{"command":"test"}`)
			require.Equal(t, 200, resp.StatusCode)
			events := commandEvents(t, resp.Body)
			var stdout, stderr strings.Builder
			counts := map[string]int{}
			for _, event := range events {
				counts[event.Type]++
				switch event.Type {
				case "stdout":
					stdout.WriteString(event.Text)
				case "stderr":
					stderr.WriteString(event.Text)
				case "error":
					require.Equal(t, tt.failure, event.Error.Name)
					if tt.value != "" {
						require.Equal(t, tt.value, event.Error.Value)
					}
				}
			}
			require.Equal(t, tt.stdout, stdout.String())
			require.Equal(t, tt.stderr, stderr.String())
			require.Equal(t, 1, counts["init"])
			require.Equal(t, 1, counts["status"])
			require.Equal(t, 1, counts["ping"])
			require.Equal(t, 1, counts["execution_complete"])
			require.Equal(t, "execution_complete", events[len(events)-1].Type)
			if tt.failure == "" {
				require.Zero(t, counts["error"])
			} else {
				require.Equal(t, 1, counts["error"])
			}
			require.Zero(t, native.signals.Load())
		})
	}
}

func TestCommandTimeoutClosesOnlyTheStream(t *testing.T) {
	canceled := make(chan struct{})
	native := &commandTestProcess{start: func(ctx context.Context, _ *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
		defer close(canceled)
		if err := stream.Send(commandStartEvent(42)); err != nil {
			return err
		}
		if err := stream.Send(commandDataEvent(false, []byte{0xe4})); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}}
	_, api := newCommandTestServer(t, native)
	resp := commandPost(t, api, `{"command":"sleep 60","timeout":100}`)
	require.Equal(t, 200, resp.StatusCode)
	events := commandEvents(t, resp.Body)
	require.Len(t, events, 5)
	require.Equal(t, "�", events[2].Text)
	require.Equal(t, "CommandTimeout", events[3].Error.Name)
	require.Equal(t, "execution_complete", events[4].Type)
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("native stream did not observe cancellation")
	}
	require.Zero(t, native.signals.Load())
}

func TestCommandConcurrentStreamsKeepNativePIDIdentity(t *testing.T) {
	var nextPID atomic.Uint32
	started, finish := make(chan struct{}, 2), make(chan struct{})
	canceled, signaled := make(chan uint32, 2), make(chan uint32, 1)
	native := &commandTestProcess{start: func(ctx context.Context, _ *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
		pid := 41 + nextPID.Add(1)
		if err := stream.Send(commandStartEvent(pid)); err != nil {
			return err
		}
		started <- struct{}{}
		select {
		case <-finish:
		case <-ctx.Done():
			canceled <- pid
			return ctx.Err()
		}
		if err := stream.Send(commandDataEvent(false, []byte(strconv.FormatUint(uint64(pid), 10)))); err != nil {
			return err
		}
		return stream.Send(commandEndEvent(0, true, nil))
	}, signal: func(_ context.Context, req *connect.Request[process.SendSignalRequest]) error {
		signaled <- req.Msg.GetProcess().GetPid()
		return nil
	}}
	_, api := newCommandTestServer(t, native)
	type result struct {
		body string
		err  error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, api.URL+"/command", strings.NewReader(`{"command":"test"}`))
			if err != nil {
				results <- result{err: err}
				return
			}
			resp, err := api.Client().Do(r)
			if err != nil {
				results <- result{err: err}
				return
			}
			body, readErr := io.ReadAll(resp.Body)
			results <- result{body: string(body), err: errors.Join(readErr, resp.Body.Close())}
		}()
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("concurrent commands did not both start")
		}
	}
	interrupt, err := http.NewRequestWithContext(t.Context(), http.MethodDelete, api.URL+"/command?id=42", nil)
	require.NoError(t, err)
	resp, err := api.Client().Do(interrupt)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, 200, resp.StatusCode)
	require.EqualValues(t, 42, <-signaled)
	select {
	case pid := <-canceled:
		t.Fatalf("interrupt canceled the forwarding request for PID %d", pid)
	default:
	}
	close(finish)
	ids := map[string]bool{}
	for range 2 {
		got := <-results
		require.NoError(t, got.err)
		events := commandEvents(t, strings.NewReader(got.body))
		require.Len(t, events, 4)
		require.Equal(t, "init", events[0].Type)
		require.Equal(t, events[0].Text, events[2].Text)
		require.False(t, ids[events[0].Text])
		ids[events[0].Text] = true
	}
	require.Equal(t, map[string]bool{"42": true, "43": true}, ids)
	require.EqualValues(t, 1, native.signals.Load())
}

func TestCommandCancellationBeforeStartReleasesTheStream(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	native := &commandTestProcess{start: func(ctx context.Context, _ *connect.Request[process.StartRequest], _ *connect.ServerStream[process.StartResponse]) error {
		close(started)
		<-ctx.Done()
		close(canceled)
		return ctx.Err()
	}}
	_, api := newCommandTestServer(t, native)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, api.URL+"/command", strings.NewReader(`{"command":"sleep 60","timeout":0}`))
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		resp, err := api.Client().Do(r)
		if resp != nil {
			_ = resp.Body.Close()
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("native Start did not begin")
	}
	cancel()
	require.Error(t, <-done)
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("canceling before the first event did not release native reception")
	}
	require.Zero(t, native.signals.Load())
}

func TestCommandInterruptIsIndependentOfTheStream(t *testing.T) {
	for _, tt := range []struct {
		name       string
		disconnect bool
		signalErr  error
		status     int
	}{
		{name: "stream remains open", status: 200},
		{name: "after disconnect", disconnect: true, status: 200},
		{name: "signal failure leaves original stream open", signalErr: connect.NewError(connect.CodeInternal, errors.New("signal failed")), status: 502},
	} {
		t.Run(tt.name, func(t *testing.T) {
			postCanceled, finish := make(chan struct{}), make(chan struct{})
			signalDeadline := make(chan time.Duration, 1)
			native := &commandTestProcess{
				start: func(ctx context.Context, _ *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
					if err := stream.Send(commandStartEvent(42)); err != nil {
						return err
					}
					select {
					case <-ctx.Done():
						close(postCanceled)
						return ctx.Err()
					case <-finish:
						return stream.Send(commandEndEvent(0, true, nil))
					}
				},
				signal: func(ctx context.Context, req *connect.Request[process.SendSignalRequest]) error {
					if req.Msg.GetProcess().GetPid() != 42 || req.Msg.GetProcess().GetTag() != "" || req.Msg.Signal != process.Signal_SIGNAL_SIGKILL || req.Header().Get("X-Access-Token") != "command-test-token" {
						return connect.NewError(connect.CodeInvalidArgument, errors.New("incorrect signal target or credential"))
					}
					deadline, ok := ctx.Deadline()
					if !ok {
						return errors.New("signal RPC must have an independent deadline")
					}
					signalDeadline <- time.Until(deadline)
					return tt.signalErr
				},
			}
			_, api := newCommandTestServer(t, native)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			r, err := http.NewRequestWithContext(ctx, http.MethodPost, api.URL+"/command", strings.NewReader(`{"command":"sleep 60","timeout":0}`))
			require.NoError(t, err)
			resp, err := api.Client().Do(r)
			require.NoError(t, err)
			defer resp.Body.Close()
			reader := bufio.NewReader(resp.Body)
			for events := 0; events < 2; {
				line, err := reader.ReadString('\n')
				require.NoError(t, err)
				if strings.HasPrefix(line, "data: ") {
					events++
				}
			}
			if tt.disconnect {
				cancel()
				select {
				case <-postCanceled:
				case <-time.After(time.Second):
					t.Fatal("disconnect did not close the native stream")
				}
			}
			require.Zero(t, native.signals.Load())
			interrupt, err := http.NewRequestWithContext(t.Context(), http.MethodDelete, api.URL+"/command?id=42", nil)
			require.NoError(t, err)
			stopped, err := api.Client().Do(interrupt)
			require.NoError(t, err)
			require.NoError(t, stopped.Body.Close())
			require.Equal(t, tt.status, stopped.StatusCode)
			remaining := <-signalDeadline
			require.Greater(t, remaining, 55*time.Second)
			require.LessOrEqual(t, remaining, time.Minute)
			if !tt.disconnect {
				select {
				case <-postCanceled:
					t.Fatal("interrupt canceled the original stream instead of only signaling the process")
				default:
				}
				close(finish)
				events := commandEvents(t, reader)
				require.Len(t, events, 1)
				require.Equal(t, "execution_complete", events[0].Type)
			}
			require.EqualValues(t, 1, native.signals.Load())
		})
	}
}

func TestCommandInterruptHonorsItsOwnRequestCancellation(t *testing.T) {
	canceled := make(chan struct{})
	native := &commandTestProcess{signal: func(ctx context.Context, _ *connect.Request[process.SendSignalRequest]) error {
		<-ctx.Done()
		close(canceled)
		return ctx.Err()
	}}
	s, _ := newCommandTestServer(t, native)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	r := httptest.NewRequest(http.MethodDelete, "/command?id=42", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	s.interruptCommand(w, commandTestRequest(r))
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("native signal RPC ignored the interrupt request cancellation")
	}
	require.EqualValues(t, 1, native.signals.Load())
	require.Empty(t, w.Body.String())
}

func TestCommandInterruptErrors(t *testing.T) {
	for _, tt := range []struct {
		id     string
		code   connect.Code
		status int
	}{
		{id: "", status: 400}, {id: "0", status: 400}, {id: "-1", status: 400}, {id: "uuid", status: 400}, {id: "4294967296", status: 400},
		{id: "42", code: connect.CodeNotFound, status: 404}, {id: "42", code: connect.CodeInternal, status: 502},
		{id: "42", code: connect.CodePermissionDenied, status: 403}, {id: "42", code: connect.CodeDeadlineExceeded, status: 504},
	} {
		t.Run(tt.id+"/"+tt.code.String(), func(t *testing.T) {
			native := &commandTestProcess{signal: func(context.Context, *connect.Request[process.SendSignalRequest]) error {
				return connect.NewError(tt.code, errors.New("native signal rejected"))
			}}
			_, api := newCommandTestServer(t, native)
			r, err := http.NewRequestWithContext(t.Context(), http.MethodDelete, api.URL+"/command?id="+tt.id, nil)
			require.NoError(t, err)
			resp, err := api.Client().Do(r)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, tt.status, resp.StatusCode)
			if tt.status == 400 {
				require.Zero(t, native.signals.Load())
			} else {
				require.EqualValues(t, 1, native.signals.Load())
			}
		})
	}
}

// Fault injection at the HTTP writer is necessary to verify local stream
// cleanup when flushing or setting a deadline fails after native Start.
type commandTestWriter struct {
	*httptest.ResponseRecorder
	deadlineCalls, writes, flushes int
	failDeadline, failWrite        int
	failFlush                      bool
	onDeadline                     func()
}

type commandTestNoFlushWriter struct{ http.ResponseWriter }

func (commandTestNoFlushWriter) SetWriteDeadline(time.Time) error { return nil }

func (w *commandTestWriter) SetWriteDeadline(time.Time) error {
	w.deadlineCalls++
	if w.onDeadline != nil {
		w.onDeadline()
	}
	if w.deadlineCalls == w.failDeadline {
		return http.ErrNotSupported
	}
	return nil
}

func (w *commandTestWriter) Write(data []byte) (int, error) {
	w.writes++
	if w.writes == w.failWrite {
		return 0, net.ErrClosed
	}
	return w.ResponseRecorder.Write(data)
}

func (w *commandTestWriter) FlushError() error {
	w.flushes++
	if w.failFlush {
		return net.ErrClosed
	}
	w.ResponseRecorder.Flush()
	return nil
}

func TestCommandWriterFailureStopsForwarding(t *testing.T) {
	for _, tt := range []struct {
		name                    string
		failDeadline, failWrite int
		failFlush               bool
		starts, writes          int
	}{
		{name: "unsupported deadline before Start", failDeadline: 1, starts: 0, writes: 1},
		{name: "deadline reset failure before Start", failDeadline: 2, starts: 0, writes: 1},
		{name: "deadline failure after Start", failDeadline: 3, starts: 1, writes: 0},
		{name: "deadline reset failure after first event", failDeadline: 4, starts: 1, writes: 1},
		{name: "write failure", failWrite: 1, starts: 1, writes: 1},
		{name: "flush failure", failFlush: true, starts: 1, writes: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			canceled := make(chan struct{})
			native := &commandTestProcess{start: func(ctx context.Context, _ *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
				defer close(canceled)
				if err := stream.Send(commandStartEvent(42)); err != nil {
					return err
				}
				<-ctx.Done()
				return ctx.Err()
			}}
			s, _ := newCommandTestServer(t, native)
			w := &commandTestWriter{ResponseRecorder: httptest.NewRecorder(), failDeadline: tt.failDeadline, failWrite: tt.failWrite, failFlush: tt.failFlush}
			r := commandTestRequest(httptest.NewRequest(http.MethodPost, "/command", strings.NewReader(`{"command":"test"}`)))
			s.runCommand(w, r)
			require.EqualValues(t, tt.starts, native.starts.Load())
			require.Equal(t, tt.writes, w.writes)
			require.Zero(t, native.signals.Load())
			if tt.starts == 0 {
				require.Equal(t, 500, w.Code)
				require.Equal(t, "application/json", w.Header().Get("Content-Type"))
			} else {
				select {
				case <-canceled:
				case <-time.After(time.Second):
					t.Fatal("writer failure did not close native reception")
				}
				require.NotContains(t, w.Body.String(), "execution_complete")
			}
		})
	}
}

func TestCommandWriterCapabilitiesAndCancellation(t *testing.T) {
	native := &commandTestProcess{start: func(context.Context, *connect.Request[process.StartRequest], *connect.ServerStream[process.StartResponse]) error {
		return errors.New("unsupported writer must not start a process")
	}}
	s, _ := newCommandTestServer(t, native)
	// ResponseRecorder can flush but cannot set a deadline.
	w := httptest.NewRecorder()
	s.runCommand(w, commandTestRequest(httptest.NewRequest(http.MethodPost, "/command", strings.NewReader(`{"command":"test"}`))))
	require.Equal(t, 500, w.Code)
	require.Zero(t, native.starts.Load())
	w = httptest.NewRecorder()
	s.runCommand(commandTestNoFlushWriter{w}, commandTestRequest(httptest.NewRequest(http.MethodPost, "/command", strings.NewReader(`{"command":"test"}`))))
	require.Equal(t, 500, w.Code)
	require.Zero(t, native.starts.Load())
	for _, cancelOnDeadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "already canceled", true: "canceled before write"}[cancelOnDeadline], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			writer := &commandTestWriter{ResponseRecorder: httptest.NewRecorder()}
			if cancelOnDeadline {
				writer.onDeadline = cancel
			} else {
				cancel()
			}
			require.ErrorIs(t, sendEvent(ctx, writer, streamEvent{Type: "init", Text: "42"}), context.Canceled)
			require.Zero(t, writer.writes)
			require.Zero(t, writer.flushes)
		})
	}
}

func TestCommandHTTPVersionHeaders(t *testing.T) {
	native := &commandTestProcess{start: func(_ context.Context, _ *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
		if err := stream.Send(commandStartEvent(42)); err != nil {
			return err
		}
		return stream.Send(commandEndEvent(0, true, nil))
	}}
	s, _ := newCommandTestServer(t, native)
	for _, version := range []int{1, 2} {
		w := &commandTestWriter{ResponseRecorder: httptest.NewRecorder()}
		r := commandTestRequest(httptest.NewRequest(http.MethodPost, "/command", strings.NewReader(`{"command":"test"}`)))
		r.ProtoMajor = version
		s.runCommand(w, r)
		if version == 1 {
			require.Equal(t, "keep-alive", w.Header().Get("Connection"))
		} else {
			require.Empty(t, w.Header().Get("Connection"))
		}
		require.Equal(t, "text/event-stream", w.Header().Get("Content-Type"))
		require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
		require.Equal(t, "no", w.Header().Get("X-Accel-Buffering"))
	}
}
