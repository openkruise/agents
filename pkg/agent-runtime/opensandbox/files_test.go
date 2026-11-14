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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openkruise/agents/pkg/runtime/envd"
	"github.com/openkruise/agents/proto/envd/process"
	"github.com/openkruise/agents/proto/envd/process/processconnect"
)

type fileAttributeCall struct {
	command, cwd, user, token, timeout string
	args                               []string
	tag                                *string
}

type fileProcess struct {
	processconnect.UnimplementedProcessHandler
	mu          sync.Mutex
	calls       []fileAttributeCall
	failAt      int
	signalled   int
	nativeToken atomic.Value
}

func (p *fileProcess) Start(_ context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
	r := &http.Request{Header: req.Header()}
	user, _, _ := r.BasicAuth()
	p.mu.Lock()
	p.calls = append(p.calls, fileAttributeCall{
		command: req.Msg.Process.Cmd, cwd: req.Msg.Process.GetCwd(), user: user,
		token: req.Header().Get("X-Access-Token"), timeout: req.Header().Get("Connect-Timeout-Ms"),
		args: append([]string(nil), req.Msg.Process.Args...), tag: req.Msg.Tag,
	})
	failed := p.failAt > 0 && len(p.calls) == p.failAt
	p.mu.Unlock()
	if err := stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_Start{
		Start: &process.ProcessEvent_StartEvent{Pid: 42},
	}}}); err != nil {
		return err
	}
	end := &process.ProcessEvent_EndEvent{Exited: true}
	if failed {
		end.ExitCode = 1
	}
	return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_End{End: end}}})
}

func (p *fileProcess) SendSignal(_ context.Context, _ *connect.Request[process.SendSignalRequest]) (*connect.Response[process.SendSignalResponse], error) {
	p.mu.Lock()
	p.signalled++
	p.mu.Unlock()
	return connect.NewResponse(&process.SendSignalResponse{}), nil
}

type fileUploadCapture struct {
	mu                         sync.Mutex
	posts                      int
	paths, contents            []string
	queryPath, username, token string
	response                   string
	status                     int
}

func (u *fileUploadCapture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.posts++
	u.queryPath = r.URL.Query().Get("path")
	if r.URL.Query().Has("path") {
		u.queryPath = "present:" + u.queryPath
	}
	u.username = r.URL.Query().Get("username")
	u.token = r.Header.Get("X-Access-Token")
	reader, err := r.MultipartReader()
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		content, err := io.ReadAll(part)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		u.paths = append(u.paths, params["filename"])
		u.contents = append(u.contents, string(content))
	}
	status := u.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if _, err := io.WriteString(w, u.response); err != nil {
		return
	}
}

func newFileServer(t *testing.T, files http.Handler, proc *fileProcess) *Server {
	t.Helper()
	_, processHandler := processconnect.NewProcessHandler(proc)
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case processconnect.ProcessListProcedure:
			token := "runtime-token"
			if current := proc.nativeToken.Load(); current != nil {
				token = current.(string)
			}
			if r.Header.Get("X-Access-Token") != token {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
		case "/files":
			files.ServeHTTP(w, r)
		default:
			processHandler.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(native.Close)
	state := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(state, []byte("runtime-token"), 0600))
	s, err := New(envd.Client{BaseURL: native.URL, HTTP: native.Client()}, state)
	require.NoError(t, err)
	t.Cleanup(s.Close)
	return s
}

func fileUploadRequest(t *testing.T, metadata []string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for i, meta := range metadata {
		part, err := writer.CreateFormField("metadata")
		require.NoError(t, err)
		_, err = io.WriteString(part, meta)
		require.NoError(t, err)
		part, err = writer.CreateFormFile("file", "same-basename")
		require.NoError(t, err)
		_, err = io.WriteString(part, strings.Repeat("content", i+1))
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	r := httptest.NewRequest(http.MethodPost, "/files/upload", &body)
	r.Header.Set("Content-Type", writer.FormDataContentType())
	r.Header.Set(executionHeader, "runtime-token")
	return r
}

func TestFileUploadBatchAndAttributes(t *testing.T) {
	for _, tt := range []struct {
		name     string
		metadata []string
		paths    []string
		attrs    []fileAttributeCall
	}{
		{
			name: "full paths and duplicate basenames stay in one request",
			metadata: []string{
				`{"path":"relative/a"}`, `{"path":"~/other/a","mode":null}`,
				`{"path":"/Unicode/文\"件\\a","owner":"","group":""}`,
			},
			paths: []string{"relative/a", "~/other/a", "/Unicode/文\"件\\a"},
		},
		{
			name:     "owner only",
			metadata: []string{`{"path":"relative/a","owner":"alice"}`},
			paths:    []string{"relative/a"},
			attrs:    []fileAttributeCall{{command: "chown", args: []string{"--", "alice", "/actual/0/a"}}},
		},
		{
			name:     "group only",
			metadata: []string{`{"path":"~/a","group":"staff"}`},
			paths:    []string{"~/a"},
			attrs:    []fileAttributeCall{{command: "chown", args: []string{"--", ":staff", "/actual/0/a"}}},
		},
		{
			name: "all content precedes ordered explicit attributes",
			metadata: []string{
				`{"path":"/first/a","owner":"alice","group":"staff","mode":640}`,
				`{"path":"/second/a","mode":755}`,
			},
			paths: []string{"/first/a", "/second/a"},
			attrs: []fileAttributeCall{
				{command: "chown", args: []string{"--", "alice:staff", "/actual/0/a"}},
				{command: "chmod", args: []string{"--", "640", "/actual/0/a"}},
				{command: "chmod", args: []string{"--", "755", "/actual/1/a"}},
			},
		},
		{
			name:     "owner whitespace is a literal value",
			metadata: []string{`{"path":"/a","owner":" alice "}`},
			paths:    []string{"/a"},
			attrs:    []fileAttributeCall{{command: "chown", args: []string{"--", " alice ", "/actual/0/a"}}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			paths := make([]uploadedFile, len(tt.paths))
			for i := range paths {
				paths[i].Path = "/actual/" + strconv.Itoa(i) + "/a"
			}
			response, err := json.Marshal(paths)
			require.NoError(t, err)
			capture := &fileUploadCapture{response: string(response)}
			proc := &fileProcess{}
			s := newFileServer(t, capture, proc)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, fileUploadRequest(t, tt.metadata))
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			capture.mu.Lock()
			assert.Equal(t, 1, capture.posts)
			assert.Equal(t, tt.paths, capture.paths)
			assert.Empty(t, capture.queryPath)
			assert.Equal(t, executionUser, capture.username)
			assert.Equal(t, "runtime-token", capture.token)
			for i, content := range capture.contents {
				assert.Equal(t, strings.Repeat("content", i+1), content)
			}
			capture.mu.Unlock()
			proc.mu.Lock()
			defer proc.mu.Unlock()
			require.Len(t, proc.calls, len(tt.attrs))
			for i, call := range proc.calls {
				assert.Equal(t, tt.attrs[i].command, call.command)
				assert.Equal(t, tt.attrs[i].args, call.args)
				assert.Equal(t, "/", call.cwd)
				assert.Equal(t, "root", call.user)
				assert.Equal(t, "runtime-token", call.token)
				assert.Equal(t, "10000", call.timeout)
				assert.Nil(t, call.tag)
			}
			assert.Zero(t, proc.signalled)
		})
	}
}

func TestFileUploadFailureBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name, response string
		nativeStatus   int
		attributeFail  int
		wantStatus     int
		wantCalls      int
	}{
		{name: "native partial content failure", nativeStatus: 400, response: `{"message":"duplicate target"}`, wantStatus: 400},
		{name: "204 is not a batch path response", nativeStatus: 204, wantStatus: 502},
		{name: "malformed response", response: `{"path":"/a"}`, wantStatus: 502},
		{name: "missing response path", response: `[{"name":"a"},{"path":"/b"}]`, wantStatus: 502},
		{name: "wrong entry count", response: `[{"path":"/a"}]`, wantStatus: 502},
		{name: "trailing response", response: `[{"path":"/a"},{"path":"/b"}] {}`, wantStatus: 502},
		{name: "attribute failure keeps content and stops later attributes", response: `[{"path":"/a"},{"path":"/b"}]`, attributeFail: 1, wantStatus: 502, wantCalls: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			capture := &fileUploadCapture{status: tt.nativeStatus, response: tt.response}
			proc := &fileProcess{failAt: tt.attributeFail}
			s := newFileServer(t, capture, proc)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, fileUploadRequest(t, []string{`{"path":"/a","mode":600}`, `{"path":"/b","mode":644}`}))
			require.Equal(t, tt.wantStatus, w.Code, w.Body.String())
			capture.mu.Lock()
			assert.Equal(t, []string{"content", "contentcontent"}, capture.contents)
			assert.Equal(t, 1, capture.posts)
			capture.mu.Unlock()
			proc.mu.Lock()
			assert.Len(t, proc.calls, tt.wantCalls)
			assert.Zero(t, proc.signalled)
			proc.mu.Unlock()
		})
	}
}

func TestFileUploadInvalidInput(t *testing.T) {
	for _, metadata := range [][]string{
		nil,
		{`{"path":"/a","mode":888}`},
		{`{"path":"/a"}`, `{"path":""}`},
	} {
		t.Run(strings.Join(metadata, ","), func(t *testing.T) {
			capture := &fileUploadCapture{response: `[]`}
			proc := &fileProcess{}
			s := newFileServer(t, capture, proc)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, fileUploadRequest(t, metadata))
			assert.Equal(t, 400, w.Code, w.Body.String())
			proc.mu.Lock()
			assert.Empty(t, proc.calls)
			assert.Zero(t, proc.signalled)
			proc.mu.Unlock()
		})
	}
}

type fileRoundTripper func(*http.Request) (*http.Response, error)

func (f fileRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A server request body must allow Close to interrupt a concurrent Read. This
// reader exposes that boundary without relying on network buffering sizes.
type blockedFileBody struct {
	entered   chan struct{}
	closed    chan struct{}
	once      sync.Once
	closeOnce sync.Once
}

func (b *blockedFileBody) Read(_ []byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.closed
	return 0, io.ErrClosedPipe
}

func (b *blockedFileBody) Close() error {
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

func TestFileUploadStopsBlockedProduction(t *testing.T) {
	for _, mode := range []string{"native error", "premature native success", "request cancellation", "transport failure"} {
		t.Run(mode, func(t *testing.T) {
			body := &blockedFileBody{entered: make(chan struct{}), closed: make(chan struct{})}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			client := &http.Client{Transport: fileRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == processconnect.ProcessListProcedure {
					status := 200
					if r.Header.Get("X-Access-Token") == "" {
						status = 401
					}
					return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
				}
				<-body.entered
				switch mode {
				case "request cancellation":
					cancel()
					<-r.Context().Done()
					return nil, r.Context().Err()
				case "transport failure":
					return nil, errors.New("native transport failed")
				}
				status := http.StatusBadRequest
				if mode == "premature native success" {
					status = http.StatusOK
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`[]`)), Header: make(http.Header)}, nil
			})}
			state := filepath.Join(t.TempDir(), "token")
			require.NoError(t, os.WriteFile(state, []byte("runtime-token"), 0600))
			s, err := New(envd.Client{BaseURL: "http://native.invalid", HTTP: client}, state)
			require.NoError(t, err)
			t.Cleanup(s.Close)
			r := httptest.NewRequest(http.MethodPost, "/files/upload", nil).WithContext(ctx)
			r.Body = body
			r.Header.Set("Content-Type", "multipart/form-data; boundary=blocked")
			r.Header.Set(executionHeader, "runtime-token")
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				s.ServeHTTP(w, r)
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("upload left its input producer blocked")
			}
			select {
			case <-body.closed:
			default:
				t.Fatal("cancelled upload did not close the incoming body")
			}
			if mode == "native error" {
				assert.Equal(t, 400, w.Code, w.Body.String())
			} else {
				assert.NotEqual(t, 200, w.Code, w.Body.String())
			}
		})
	}
}

func TestFileUploadNativeRejectsBackpressuredWriter(t *testing.T) {
	client := &http.Client{Transport: fileRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == processconnect.ProcessListProcedure {
			status := http.StatusOK
			if r.Header.Get("X-Access-Token") == "" {
				status = http.StatusUnauthorized
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
		}
		// Consume only one header byte, leaving the multipart writer blocked on
		// the rest of its first part. Native rejection must release that writer.
		var first [1]byte
		if _, err := r.Body.Read(first[:]); err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
	})}
	state := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(state, []byte("runtime-token"), 0600))
	s, err := New(envd.Client{BaseURL: "http://native.invalid", HTTP: client}, state)
	require.NoError(t, err)
	t.Cleanup(s.Close)
	done := make(chan struct{})
	w := httptest.NewRecorder()
	r := fileUploadRequest(t, []string{`{"path":"/a","mode":644}`})
	go func() {
		s.ServeHTTP(w, r)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("native rejection left the multipart writer blocked")
	}
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

func TestFileUploadPrematureSuccessBeforeFinalBoundary(t *testing.T) {
	var attributes atomic.Int32
	closingBoundaryStarted := make(chan struct{})
	client := &http.Client{Transport: fileRoundTripper(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case processconnect.ProcessListProcedure:
			status := http.StatusOK
			if r.Header.Get("X-Access-Token") == "" {
				status = http.StatusUnauthorized
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
		case "/files":
			// Read the native header without read-ahead into the file body or
			// closing boundary. Each Write must be consumed explicitly below.
			var header bytes.Buffer
			var next [1]byte
			for !bytes.HasSuffix(header.Bytes(), []byte("\r\n\r\n")) {
				if _, err := io.ReadFull(r.Body, next[:]); err != nil {
					return nil, err
				}
				header.WriteByte(next[0])
			}
			content := make([]byte, len("content"))
			if _, err := io.ReadFull(r.Body, content); err != nil {
				return nil, err
			}
			// Reading one byte of the final boundary proves the input EOF has
			// been observed and writer.Close is now blocked on its remainder.
			if _, err := io.ReadFull(r.Body, next[:]); err != nil {
				return nil, err
			}
			close(closingBoundaryStarted)
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`[{"path":"/actual/a"}]`)), Header: make(http.Header)}, nil
		default:
			attributes.Add(1)
			return nil, errors.New("attribute operation ran after a premature upload response")
		}
	})}
	state := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(state, []byte("runtime-token"), 0600))
	s, err := New(envd.Client{BaseURL: "http://native.invalid", HTTP: client}, state)
	require.NoError(t, err)
	t.Cleanup(s.Close)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r := fileUploadRequest(t, []string{`{"path":"/a","mode":644}`}).WithContext(ctx)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		s.ServeHTTP(w, r)
		close(done)
	}()
	select {
	case <-closingBoundaryStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("native request did not reach its final boundary")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		cancel()
		<-done
		t.Fatal("premature native success left writer.Close blocked")
	}
	assert.Equal(t, http.StatusBadGateway, w.Code, w.Body.String())
	assert.Zero(t, attributes.Load())
}

// Reading the request EOF is deliberately delayed until the handler begins
// reading the native response, exposing a reader-close race in a real transport.
type fileResponseAfterRequestEOF struct {
	io.Reader
	request  io.Reader
	observed chan error
	checked  bool
	err      error
}

func (b *fileResponseAfterRequestEOF) Read(p []byte) (int, error) {
	if !b.checked {
		b.checked = true
		var extra [1]byte
		n, err := b.request.Read(extra[:])
		b.observed <- err
		if n != 0 || !errors.Is(err, io.EOF) {
			b.err = fmt.Errorf("native request did not finish with EOF: %w", err)
		}
	}
	if b.err != nil {
		return 0, b.err
	}
	return b.Reader.Read(p)
}

func (*fileResponseAfterRequestEOF) Close() error { return nil }

func TestFileUploadCompleteBoundaryPreservesRequestEOF(t *testing.T) {
	observedEOF := make(chan error, 1)
	response := `[{"path":"/actual/a","name":"` + strings.Repeat("x", 128*1024) + `"}]`
	client := &http.Client{Transport: fileRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == processconnect.ProcessListProcedure {
			status := http.StatusOK
			if r.Header.Get("X-Access-Token") == "" {
				status = http.StatusUnauthorized
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
		}
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			return nil, err
		}
		// Consume exactly the header, content, and complete closing boundary;
		// do not perform the following EOF read until the response is consumed.
		var header bytes.Buffer
		var next [1]byte
		for !bytes.HasSuffix(header.Bytes(), []byte("\r\n\r\n")) {
			if _, err := io.ReadFull(r.Body, next[:]); err != nil {
				return nil, err
			}
			header.WriteByte(next[0])
		}
		content := make([]byte, len("content"))
		if _, err := io.ReadFull(r.Body, content); err != nil {
			return nil, err
		}
		boundary := make([]byte, len("\r\n--"+params["boundary"]+"--\r\n"))
		if _, err := io.ReadFull(r.Body, boundary); err != nil {
			return nil, err
		}
		body := &fileResponseAfterRequestEOF{Reader: strings.NewReader(response), request: r.Body, observed: observedEOF}
		return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header)}, nil
	})}
	state := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(state, []byte("runtime-token"), 0600))
	s, err := New(envd.Client{BaseURL: "http://native.invalid", HTTP: client}, state)
	require.NoError(t, err)
	t.Cleanup(s.Close)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, fileUploadRequest(t, []string{`{"path":"/a"}`}))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	select {
	case err := <-observedEOF:
		assert.ErrorIs(t, err, io.EOF)
	default:
		t.Fatal("the native success response was not read")
	}
}

type observedFileBody struct {
	io.ReadCloser
	onRead func()
}

func (b observedFileBody) Read(p []byte) (int, error) {
	b.onRead()
	return b.ReadCloser.Read(p)
}

func TestFileUploadNativeRejectionInterruptsHTTPInput(t *testing.T) {
	readStarted := make(chan struct{})
	var once sync.Once
	client := &http.Client{Transport: fileRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == processconnect.ProcessListProcedure {
			status := http.StatusOK
			if r.Header.Get("X-Access-Token") == "" {
				status = http.StatusUnauthorized
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
		}
		<-readStarted
		return &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
	})}
	state := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(state, []byte("runtime-token"), 0600))
	s, err := New(envd.Client{BaseURL: "http://native.invalid", HTTP: client}, state)
	require.NoError(t, err)
	t.Cleanup(s.Close)
	handlerDone := make(chan struct{})
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		r.Body = observedFileBody{ReadCloser: r.Body, onRead: func() { once.Do(func() { close(readStarted) }) }}
		s.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	conn, err := net.DialTimeout("tcp", front.Listener.Addr().String(), time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	require.NoError(t, conn.SetDeadline(time.Now().Add(3*time.Second)))
	// Deliberately stop before the declared body is complete. Closing a server
	// body alone may wait for Read or drain; cancellation must set its deadline.
	_, err = fmt.Fprintf(conn, "POST /files/upload HTTP/1.1\r\nHost: local\r\nContent-Type: multipart/form-data; boundary=blocked\r\nX-EXECD-ACCESS-TOKEN: runtime-token\r\nContent-Length: 4096\r\nConnection: close\r\n\r\n--blocked\r\n")
	require.NoError(t, err)
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err == nil {
		require.NoError(t, response.Body.Close())
		assert.Equal(t, http.StatusBadRequest, response.StatusCode)
	} else {
		// Interrupting an incomplete HTTP/1 body may also close the connection
		// before an error response can be written. It must not wait for input.
		assert.True(t, errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF), err)
	}
	select {
	case <-handlerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("native rejection left the HTTP upload handler blocked")
	}
}

func TestFileDownloadRangeAndNativeErrors(t *testing.T) {
	for _, tt := range []struct {
		name, requestedRange, nativeBody string
		nativeStatus, wantStatus         int
		contentRange                     string
	}{
		{name: "range", requestedRange: "bytes=1-3", nativeStatus: 206, wantStatus: 206, nativeBody: "bcd", contentRange: "bytes 1-3/6"},
		{name: "unsatisfiable", requestedRange: "bytes=100-", nativeStatus: 416, wantStatus: 416, nativeBody: "range failure", contentRange: "bytes */6"},
		{name: "not found", nativeStatus: 404, wantStatus: 404, nativeBody: "native details"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var gotRange, gotIfRange, gotPath, gotUser, gotToken string
			native := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				gotRange, gotIfRange = r.Header.Get("Range"), r.Header.Get("If-Range")
				gotPath, gotUser = r.URL.Query().Get("path"), r.URL.Query().Get("username")
				gotToken = r.Header.Get("X-Access-Token")
				w.Header().Set("Content-Range", tt.contentRange)
				w.Header().Set("Accept-Ranges", "bytes")
				w.Header().Set("ETag", `"version"`)
				w.WriteHeader(tt.nativeStatus)
				if _, err := io.WriteString(w, tt.nativeBody); err != nil {
					return
				}
			})
			s := newFileServer(t, native, &fileProcess{})
			r := httptest.NewRequest(http.MethodGet, "/files/download?path=%2Fhome%2Fuser%2Fa", nil)
			r.Header.Set(executionHeader, "runtime-token")
			r.Header.Set("Range", tt.requestedRange)
			r.Header.Set("If-Range", `"version"`)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			mu.Lock()
			defer mu.Unlock()
			assert.Equal(t, tt.wantStatus, w.Code, w.Body.String())
			assert.Equal(t, tt.requestedRange, gotRange)
			assert.Equal(t, `"version"`, gotIfRange)
			assert.Equal(t, "/home/user/a", gotPath)
			assert.Equal(t, executionUser, gotUser)
			assert.Equal(t, "runtime-token", gotToken)
			if tt.nativeStatus != 404 {
				assert.Equal(t, tt.nativeBody, w.Body.String())
				assert.Equal(t, tt.contentRange, w.Header().Get("Content-Range"))
				assert.Equal(t, "bytes", w.Header().Get("Accept-Ranges"))
				assert.Equal(t, `"version"`, w.Header().Get("ETag"))
			} else {
				assert.NotContains(t, w.Body.String(), tt.nativeBody)
			}
		})
	}
}

type fileCommitWriter struct {
	http.ResponseWriter
	committed chan struct{}
}

func (w fileCommitWriter) WriteHeader(status int) {
	w.ResponseWriter.WriteHeader(status)
	close(w.committed)
}

func (w fileCommitWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func TestFileDownloadCancellationClosesNativeStream(t *testing.T) {
	for _, mode := range []string{"request cancellation", "server close", "credential rebinding"} {
		t.Run(mode, func(t *testing.T) {
			started := make(chan struct{})
			nativeClosed := make(chan struct{})
			proc := &fileProcess{}
			s := newFileServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				if _, err := io.WriteString(w, "first segment"); err != nil {
					return
				}
				if err := http.NewResponseController(w).Flush(); err != nil {
					return
				}
				close(started)
				<-r.Context().Done()
				close(nativeClosed)
			}), proc)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			r := httptest.NewRequest(http.MethodGet, "/files/download?path=%2Fa", nil).WithContext(ctx)
			r.Header.Set(executionHeader, "runtime-token")
			w := httptest.NewRecorder()
			committed := make(chan struct{})
			done := make(chan struct{})
			go func() {
				s.ServeHTTP(fileCommitWriter{ResponseWriter: w, committed: committed}, r)
				close(done)
			}()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("native download did not start")
			}
			select {
			case <-committed:
			case <-time.After(3 * time.Second):
				t.Fatal("download did not commit the native response")
			}
			actionDone := make(chan struct{})
			go func() {
				switch mode {
				case "request cancellation":
					cancel()
				case "server close":
					s.Close()
				case "credential rebinding":
					proc.nativeToken.Store("replacement-token")
					initRequest := httptest.NewRequest(http.MethodPost, "/init", strings.NewReader(`{"accessToken":"replacement-token"}`))
					initResponse := httptest.NewRecorder()
					s.ServeHTTP(initResponse, initRequest)
					assert.Equal(t, http.StatusNoContent, initResponse.Code, initResponse.Body.String())
				}
				close(actionDone)
			}()
			for _, completion := range []<-chan struct{}{done, nativeClosed, actionDone} {
				select {
				case <-completion:
				case <-time.After(3 * time.Second):
					t.Fatal("cancelled download left a forwarding stream open")
				}
			}
			assert.Equal(t, http.StatusOK, w.Code)
		})
	}
}
