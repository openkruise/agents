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

package filecred

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/proto/envd/filesystem"
	"github.com/openkruise/agents/proto/envd/filesystem/filesystemconnect"
	"github.com/openkruise/agents/proto/envd/process"
	"github.com/openkruise/agents/proto/envd/process/processconnect"
)

const (
	testCredPath = "/var/opt/sandbox/agent-token/id.token"
	testToken    = "header.payload.signature"
)

// mockRuntime serves the three endpoints a credential touches: the multipart
// files API for the write, the Process service for the chmod, and the Filesystem
// service for the removal. Serving all three from one sandbox is the point: it is
// how the propagator and its cleaner are exercised against the real client code
// rather than against a stub of it.
type mockRuntime struct {
	filesystemconnect.UnimplementedFilesystemHandler
	processconnect.UnimplementedProcessHandler

	// injected behaviour
	writeStatus int
	chmodExit   int32
	removeErr   error

	// observed
	writeCalls       int
	writePath        string
	writeUsername    string
	writeAuthUser    string
	writeAccessToken string
	writeBody        []byte

	chmodCalls int
	chmodArgs  []string

	removeCalls    int
	removePath     string
	removeAuthUser string
}

func (m *mockRuntime) Remove(_ context.Context, req *connect.Request[filesystem.RemoveRequest],
) (*connect.Response[filesystem.RemoveResponse], error) {
	m.removeCalls++
	m.removePath = req.Msg.GetPath()
	m.removeAuthUser = req.Header().Get("Authorization")
	if m.removeErr != nil {
		return nil, m.removeErr
	}
	return connect.NewResponse(&filesystem.RemoveResponse{}), nil
}

func (m *mockRuntime) Start(_ context.Context, req *connect.Request[process.StartRequest],
	stream *connect.ServerStream[process.StartResponse]) error {
	m.chmodCalls++
	cfg := req.Msg.GetProcess()
	if cfg != nil {
		m.chmodArgs = append([]string{cfg.Cmd}, cfg.Args...)
	}
	return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{
		Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{
			ExitCode: m.chmodExit, Exited: true,
		}},
	}})
}

// ServeHTTP handles the multipart write, which does not go through connect.
func (m *mockRuntime) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.writeCalls++
	m.writePath = r.URL.Query().Get("path")
	m.writeUsername = r.URL.Query().Get("username")
	m.writeAuthUser = r.Header.Get("Authorization")
	m.writeAccessToken = r.Header.Get("X-Access-Token")
	if err := r.ParseMultipartForm(1 << 20); err == nil {
		if f, _, err := r.FormFile("file"); err == nil {
			defer func() { _ = f.Close() }()
			m.writeBody, _ = io.ReadAll(f)
		}
	}
	status := m.writeStatus
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
}

// newMockSandbox starts m and returns a Sandbox pointing at it.
func newMockSandbox(t *testing.T, m *mockRuntime) *agentsv1alpha1.Sandbox {
	t.Helper()
	fsPath, fsHandler := filesystemconnect.NewFilesystemHandler(m)
	procPath, procHandler := processconnect.NewProcessHandler(m)

	mux := http.NewServeMux()
	mux.Handle(fsPath, fsHandler)
	mux.Handle(procPath, procHandler)
	mux.Handle("/", m)

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-cred",
			Namespace: "default",
			Annotations: map[string]string{
				agentsv1alpha1.AnnotationRuntimeURL:         server.URL,
				agentsv1alpha1.AnnotationRuntimeAccessToken: "runtime-access-token",
			},
		},
	}
}

func mustCredential(t *testing.T, cfg Config) *Credential {
	t.Helper()
	c, err := New(cfg)
	require.NoError(t, err)
	return c
}

func TestNew(t *testing.T) {
	tests := []struct {
		name         string
		cfg          Config
		expectError  string
		wantMode     string
		wantFileMode os.FileMode
		wantAuthUser string
	}{
		{
			name:         "defaults fill in an owner-only mode and the root identity",
			cfg:          Config{Path: testCredPath},
			wantMode:     defaultMode,
			wantFileMode: 0600,
			wantAuthUser: defaultAuthUser,
		},
		{
			name:         "an explicit mode and auth user are kept",
			cfg:          Config{Path: testCredPath, Mode: "0400", AuthUser: "agent"},
			wantMode:     "0400",
			wantFileMode: 0400,
			wantAuthUser: "agent",
		},
		{
			name:        "a relative path is rejected",
			cfg:         Config{Path: "agent-token/id.token"},
			expectError: "must be absolute",
		},
		{
			// A traversal segment would let the configured path escape the
			// directory a deployment meant to confine credentials to.
			name:        "an unclean path is rejected",
			cfg:         Config{Path: "/var/opt/sandbox/../../etc/id.token"},
			expectError: "must be clean",
		},
		{
			name:        "a non-octal mode is rejected",
			cfg:         Config{Path: testCredPath, Mode: "rw-------"},
			expectError: "three octal digits",
		},
		{
			name:        "an out-of-range octal digit is rejected",
			cfg:         Config{Path: testCredPath, Mode: "0680"},
			expectError: "three octal digits",
		},
		{
			// A credential file has no business carrying a setuid, setgid or
			// sticky bit, so the four digit form is out of range.
			name:        "a mode carrying a special bit is rejected",
			cfg:         Config{Path: testCredPath, Mode: "1777"},
			expectError: "three octal digits",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New(tt.cfg)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				assert.Nil(t, c)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantMode, c.cfg.Mode)
			assert.Equal(t, tt.wantFileMode, c.fileMode,
				"the derived mode must match the string the chmod receives")
			assert.Equal(t, tt.wantAuthUser, c.cfg.AuthUser)
			assert.Equal(t, defaultTimeout, c.cfg.Timeout)
		})
	}
}

func TestPropagate(t *testing.T) {
	tests := []struct {
		name        string
		tokenResp   *identity.TokenResponse
		writeStatus int
		chmodExit   int32
		removeErr   error
		nilSandbox  bool
		wantWrites  int
		wantChmods  int
		wantRemoves int
		expectError string
	}{
		{
			name:       "the token is written and the mode is applied",
			tokenResp:  &identity.TokenResponse{AccessToken: testToken},
			wantWrites: 1,
			wantChmods: 1,
		},
		{
			name:        "a rejected write stops before the chmod",
			tokenResp:   &identity.TokenResponse{AccessToken: testToken},
			writeStatus: http.StatusForbidden,
			wantWrites:  1,
			wantChmods:  0,
			expectError: "failed to write credential",
		},
		{
			// The write lands at the runtime default, so a failing chmod leaves the
			// token readable by anything else in the sandbox. Nothing would come
			// back for that file later, so it is removed before the error returns.
			name:        "a failing chmod removes the credential it could not protect",
			tokenResp:   &identity.TokenResponse{AccessToken: testToken},
			chmodExit:   1,
			wantWrites:  1,
			wantChmods:  1,
			wantRemoves: 1,
			expectError: "credential removed",
		},
		{
			// If the removal fails too the credential is still exposed, and the
			// caller needs both facts: the chmod is the cause, the failed removal
			// is what it has to act on.
			name:        "a failing chmod whose removal also fails reports both",
			tokenResp:   &identity.TokenResponse{AccessToken: testToken},
			chmodExit:   1,
			removeErr:   errors.New("runtime unreachable"),
			wantWrites:  1,
			wantChmods:  1,
			wantRemoves: 1,
			expectError: "removing it also failed",
		},
		{
			name:        "a nil token response is rejected before any call",
			tokenResp:   nil,
			expectError: "no token to propagate",
		},
		{
			name:        "an empty token is rejected before any call",
			tokenResp:   &identity.TokenResponse{AccessToken: ""},
			expectError: "no token to propagate",
		},
		{
			name:        "a nil sandbox is rejected before any call",
			tokenResp:   &identity.TokenResponse{AccessToken: testToken},
			nilSandbox:  true,
			expectError: "sandbox is nil",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &mockRuntime{writeStatus: tt.writeStatus, chmodExit: tt.chmodExit, removeErr: tt.removeErr}
			sbx := newMockSandbox(t, m)
			if tt.nilSandbox {
				sbx = nil
			}
			c := mustCredential(t, Config{Path: testCredPath})

			err := c.Propagate(context.Background(), sbx, tt.tokenResp)

			assert.Equal(t, tt.wantWrites, m.writeCalls)
			assert.Equal(t, tt.wantChmods, m.chmodCalls)
			assert.Equal(t, tt.wantRemoves, m.removeCalls,
				"a credential that could not be protected must not be left behind")
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, testCredPath, m.writePath)
			assert.Equal(t, testToken, string(m.writeBody), "the file body must be the minted token")
			assert.Equal(t, defaultAuthUser, m.writeUsername)
			assert.NotEmpty(t, m.writeAuthUser, "the runtime resolves the write against a user identity")
			assert.Equal(t, "runtime-access-token", m.writeAccessToken,
				"the sandbox access token must gate the write")
			assert.Equal(t, []string{"chmod", defaultMode, testCredPath}, m.chmodArgs)
		})
	}
}

func TestCleanup(t *testing.T) {
	tests := []struct {
		name        string
		removeErr   error
		nilSandbox  bool
		wantRemoves int
		expectError string
	}{
		{
			name:        "the credential is removed",
			wantRemoves: 1,
		},
		{
			// Removal is not idempotent: RemovePathWithRuntime reports a missing
			// path as ErrRuntimePathNotFound, and a cleaner runs on retrying
			// lifecycle events, so it meets that case normally.
			name:        "an already absent credential counts as success",
			removeErr:   connect.NewError(connect.CodeNotFound, fmt.Errorf("file not found")),
			wantRemoves: 1,
		},
		{
			// A runtime predating the Filesystem service never accepted the
			// write, so there is nothing of ours in it.
			name:        "a runtime without the filesystem service counts as success",
			removeErr:   connect.NewError(connect.CodeUnimplemented, fmt.Errorf("unknown service")),
			wantRemoves: 1,
		},
		{
			name:        "any other failure is surfaced",
			removeErr:   connect.NewError(connect.CodeInternal, fmt.Errorf("error removing file")),
			wantRemoves: 1,
			expectError: "failed to remove credential",
		},
		{
			name:        "a nil sandbox is rejected before any call",
			nilSandbox:  true,
			expectError: "sandbox is nil",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &mockRuntime{removeErr: tt.removeErr}
			sbx := newMockSandbox(t, m)
			if tt.nilSandbox {
				sbx = nil
			}
			c := mustCredential(t, Config{Path: testCredPath})

			err := c.Cleanup(context.Background(), sbx)

			assert.Equal(t, tt.wantRemoves, m.removeCalls)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			if tt.wantRemoves > 0 {
				assert.Equal(t, testCredPath, m.removePath)
				assert.NotEmpty(t, m.removeAuthUser,
					"the runtime resolves the removal against a user identity")
			}
		})
	}
}

// TestPropagateThenCleanup runs both halves against one sandbox, which is the
// property that matters: the path the cleaner removes is the path the propagator
// wrote, without either side being told separately.
func TestPropagateThenCleanup(t *testing.T) {
	m := &mockRuntime{}
	sbx := newMockSandbox(t, m)
	c := mustCredential(t, Config{Path: testCredPath, Mode: "0400"})

	require.NoError(t, c.Propagate(context.Background(), sbx,
		&identity.TokenResponse{AccessToken: testToken}))
	require.NoError(t, c.Cleanup(context.Background(), sbx))

	assert.Equal(t, m.writePath, m.removePath)
	assert.Equal(t, []string{"chmod", "0400", testCredPath}, m.chmodArgs)
}

func TestRegisterPropagator(t *testing.T) {
	c := mustCredential(t, Config{Path: testCredPath})

	before := identity.SecurityTokenPropagatorCount()

	c.RegisterPropagator()

	// The registry is append-only by design, so the count is the only observable.
	assert.Equal(t, before+1, identity.SecurityTokenPropagatorCount())
}
