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

package runtime

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/proto/envd/filesystem/filesystemconnect"
	"github.com/openkruise/agents/proto/envd/process"
	"github.com/openkruise/agents/proto/envd/process/processconnect"
)

// sandboxWithPodIP returns a sandbox whose Pod IP is set (but with no runtime
// URL annotation), so TLS-mode addressing must derive the dial target from the
// Pod IP while addressing the request by the certificate authority hostname.
func sandboxWithPodIP(ip string) *agentsv1alpha1.Sandbox {
	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "tls-sandbox", Namespace: "default"},
		Status: agentsv1alpha1.SandboxStatus{
			PodInfo: agentsv1alpha1.PodInfo{PodIP: ip},
		},
	}
}

// TestTLSMode_ResolvesAuthorityButDialsPodIP verifies the curl --resolve
// behaviour: the request is addressed to the certificate authority hostname
// (which has no DNS record) while the TCP connection is pinned to the sandbox
// Pod IP, and TLS verification still succeeds against the server certificate.
func TestTLSMode_ResolvesAuthorityButDialsPodIP(t *testing.T) {
	var gotHost string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/storage/mounts", func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		writeMountResponse(t, w, http.StatusOK, CreateMountResponse{Success: true, MountPath: "/m"})
	})
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)

	// The httptest server certificate is issued for "example.com" (and the
	// loopback IPs); use it as both the trust anchor and the authority so the
	// handshake validates when we pin the dial to 127.0.0.1.
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})

	host, portStr, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)

	rt := NewRuntime(
		sandboxWithPodIP(host), // Pod IP == loopback the test server listens on
		WithRetry(wait.Backoff{Steps: 1}),
		WithTLS(TLSBundle{CABundle: caPEM}),
		WithAuthority("example.com"),
		WithTLSPort(port),
	)

	resp, err := rt.Storage().Mount(context.Background(), testMountRequest("oss"))
	require.NoError(t, err)
	assert.True(t, resp.Success)
	// The Host header must carry the authority, not the dialed IP, proving the
	// request was addressed by domain even though the connection went to the IP.
	assert.Contains(t, gotHost, "example.com")
}

// TestTLSMode_OneShotTransportDoesNotLeakConnections verifies that the
// per-attempt pinned transport disables keep-alives: each call opens exactly
// one connection and the server observes it closed after the response, so the
// transport discarded after the attempt never strands an idle TLS connection.
func TestTLSMode_OneShotTransportDoesNotLeakConnections(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/storage/mounts", func(w http.ResponseWriter, r *http.Request) {
		writeMountResponse(t, w, http.StatusOK, CreateMountResponse{Success: true, MountPath: "/m"})
	})
	server := httptest.NewUnstartedServer(mux)
	var mu sync.Mutex
	opened, closed := 0, 0
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		mu.Lock()
		defer mu.Unlock()
		switch state {
		case http.StateNew:
			opened++
		case http.StateClosed:
			closed++
		}
	}
	server.StartTLS()
	t.Cleanup(server.Close)

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	host, portStr, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)

	rt := NewRuntime(
		sandboxWithPodIP(host),
		WithRetry(wait.Backoff{Steps: 1}),
		WithTLS(TLSBundle{CABundle: caPEM}),
		WithAuthority("example.com"),
		WithTLSPort(port),
	)

	for i := 0; i < 2; i++ {
		_, err := rt.Storage().Mount(context.Background(), testMountRequest("oss"))
		require.NoError(t, err)
	}

	// Keep-alives are disabled, so every call must open its own connection and
	// the server must observe each of them closed shortly after the response
	// instead of lingering as an idle kept-alive connection.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return opened == 2 && closed == 2
	}, 3*time.Second, 20*time.Millisecond)
}

// TestTLSMode_InvalidCAIsPermanentError verifies that an unusable CA bundle is
// reported as a permanent error on the first call instead of being retried.
func TestTLSMode_InvalidCAIsPermanentError(t *testing.T) {
	rt := NewRuntime(
		sandboxWithPodIP("10.0.0.1"),
		WithRetry(fastBackoff),
		WithTLS(TLSBundle{CABundle: []byte("not a pem")}),
	)
	_, err := rt.Storage().Mount(context.Background(), testMountRequest("oss"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid runtime TLS configuration")
}

// TestBuildClientTLSConfig covers the bundle validation branches.
func TestBuildClientTLSConfig(t *testing.T) {
	// Reuse a valid CA PEM from a throwaway TLS server certificate.
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(server.Close)
	validCA := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	keyDER, err := x509.MarshalPKCS8PrivateKey(server.TLS.Certificates[0].PrivateKey)
	require.NoError(t, err)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	// Exercise the retained Secret loader entrypoint and its parsed cache.
	reader := fake.NewClientBuilder().WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "certs", Name: "runtime"},
		Data:       map[string][]byte{"ca.crt": validCA, "tls.crt": validCA, "tls.key": keyPEM},
	}).Build()
	preParsed, err := NewTLSBundleFromSecret(t.Context(), reader, "certs", "runtime")
	require.NoError(t, err)
	cachedCAs, _, err := preParsed.Parsed()
	require.NoError(t, err)
	// Exercise the directory forwarding entrypoint as well.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ca.crt"), validCA, 0o600))
	fromDir, err := NewTLSBundle(dir)
	require.NoError(t, err)

	tests := []struct {
		name    string
		bundle  TLSBundle
		wantErr bool
	}{
		{name: "missing CA", bundle: TLSBundle{}, wantErr: true},
		{name: "invalid CA", bundle: TLSBundle{CABundle: []byte("garbage")}, wantErr: true},
		{name: "valid CA only", bundle: TLSBundle{CABundle: validCA}, wantErr: false},
		{name: "invalid client cert", bundle: TLSBundle{CABundle: validCA, ClientCertPEM: []byte("x"), ClientKeyPEM: []byte("y")}, wantErr: true},
		{name: "cached parse is reused", bundle: *preParsed, wantErr: false},
		{name: "directory loader bundle", bundle: *fromDir, wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := buildClientTLSConfig(tt.bundle, "example.com")
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, cfg)
			assert.Equal(t, "example.com", cfg.ServerName)
			assert.NotNil(t, cfg.RootCAs)
			if tt.name == "cached parse is reused" {
				assert.Same(t, cachedCAs, cfg.RootCAs)
				require.Len(t, cfg.Certificates, 1)
				assert.Equal(t, [][]byte{server.Certificate().Raw}, cfg.Certificates[0].Certificate)
				assert.Equal(t, server.TLS.Certificates[0].PrivateKey, cfg.Certificates[0].PrivateKey)
			} else {
				assert.Empty(t, cfg.Certificates)
			}
		})
	}
}

// TestTransportOptionsFor covers the capability decision matrix: the sandbox
// capability annotation drives the transport, and a sandbox advertising it
// while the caller holds no TLS bundle is an error rather than a silent
// downgrade to plaintext.
func TestTransportOptionsFor(t *testing.T) {
	bundle := &TLSBundle{CABundle: []byte("pem")}
	sandboxWithTLSPort := func(port string) *agentsv1alpha1.Sandbox {
		sbx := sandboxWithPodIP("10.0.0.1")
		sbx.Annotations = map[string]string{agentsv1alpha1.AnnotationRuntimeTLSPort: port}
		return sbx
	}

	tests := []struct {
		name        string
		sbx         *agentsv1alpha1.Sandbox
		bundle      *TLSBundle
		wantTLS     bool
		wantTLSPort int
		wantErr     bool
	}{
		{name: "nil sandbox", sbx: nil, bundle: bundle},
		{name: "no annotation stays HTTP", sbx: sandboxWithPodIP("10.0.0.1"), bundle: bundle},
		{name: "annotation without bundle is an error", sbx: sandboxWithTLSPort("49984"), bundle: nil, wantErr: true},
		{name: "annotation with bundle enables TLS", sbx: sandboxWithTLSPort("49984"), bundle: bundle, wantTLS: true, wantTLSPort: 49984},
		{name: "annotation with custom port", sbx: sandboxWithTLSPort("50000"), bundle: bundle, wantTLS: true, wantTLSPort: 50000},
		{name: "non-numeric annotation is an error", sbx: sandboxWithTLSPort("not-a-port"), bundle: bundle, wantErr: true},
		{name: "out-of-range annotation is an error", sbx: sandboxWithTLSPort("70000"), bundle: nil, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, err := TransportOptionsFor(tt.sbx, tt.bundle)
			if tt.wantErr {
				require.Error(t, err)
				assert.Nil(t, opts)
				return
			}
			require.NoError(t, err)
			if !tt.wantTLS {
				assert.Nil(t, opts)
				return
			}
			// Apply the options through the regular constructor and inspect the
			// resulting client to prove TLS mode and port took effect.
			rc, ok := NewRuntime(tt.sbx, opts...).(*runtimeClient)
			require.True(t, ok)
			assert.True(t, rc.tlsEnabled)
			assert.Equal(t, tt.wantTLSPort, rc.tlsPort)
		})
	}
}

// TestTLSMode_EveryCapabilityGroupRidesTheResolvedTransport is the regression
// test for the invariant that made the security-token propagation reachable over
// TLS: all three wire protocols spoken to the runtime (JSON for storage,
// multipart for the files route, connect-gRPC for the filesystem and process
// services) must resolve their endpoint through the same transport decision.
//
// Before the capability groups existed, only the JSON path honoured WithTLS
// while the multipart and connect paths hard-coded the plaintext runtime URL, so
// a TLS-capable sandbox silently received its credential in clear text. Each
// case therefore asserts two things: the call reaches the HTTPS server at all
// (proving the pinned TLS transport is used), and the request is addressed by
// the certificate authority hostname rather than the dialed Pod IP.
func TestTLSMode_EveryCapabilityGroupRidesTheResolvedTransport(t *testing.T) {
	const authority = "example.com"

	fsHandler := &mockFilesystemHandler{}
	procHandler := &mockProcessHandler{
		startFn: func(_ context.Context, _ *connect.Request[process.StartRequest],
			stream *connect.ServerStream[process.StartResponse]) error {
			// A single End event is enough: Run only has to observe the process
			// exiting to return, and the transport is what this test exercises.
			return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{
				Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{ExitCode: 0, Exited: true}},
			}})
		},
	}

	mux := http.NewServeMux()
	var filesCalls int
	mux.HandleFunc("/files", func(w http.ResponseWriter, _ *http.Request) {
		filesCalls++
		w.WriteHeader(http.StatusOK)
	})
	fsPath, fsHTTP := filesystemconnect.NewFilesystemHandler(fsHandler)
	mux.Handle(fsPath, fsHTTP)
	procPath, procHTTP := processconnect.NewProcessHandler(procHandler)
	mux.Handle(procPath, procHTTP)

	// Record the Host of every request ahead of the routing so the connect
	// handlers (which never expose it) are covered just like the plain routes.
	var mu sync.Mutex
	var gotHosts []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotHosts = append(gotHosts, r.Host)
		mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	host, portStr, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)

	newTLSRuntime := func() Runtime {
		return NewRuntime(
			sandboxWithPodIP(host), // Pod IP == loopback the TLS server listens on
			WithRetry(wait.Backoff{Steps: 1}),
			WithTLS(TLSBundle{CABundle: caPEM}),
			WithAuthority(authority),
			WithTLSPort(port),
		)
	}

	tests := []struct {
		name string
		// invoke performs one capability call against the TLS runtime. A nil
		// error means the call completed over HTTPS.
		invoke func(ctx context.Context, rt Runtime) error
		// verify asserts the server side observed the call.
		verify func(t *testing.T)
	}{
		{
			name: "filesystem write over the multipart files route",
			invoke: func(ctx context.Context, rt Runtime) error {
				_, err := rt.Filesystem().Write(ctx, WriteFileRequest{
					FilePath: "/var/opt/token", Content: []byte("credential"), AuthUser: "root",
				})
				return err
			},
			verify: func(t *testing.T) {
				assert.Equal(t, 1, filesCalls, "the files route must be reached over TLS")
			},
		},
		{
			name: "filesystem listdir over connect-gRPC",
			invoke: func(ctx context.Context, rt Runtime) error {
				_, err := rt.Filesystem().ListDir(ctx, ListDirRequest{Path: "/var/opt", AuthUser: "root"})
				return err
			},
			verify: func(t *testing.T) {
				assert.Equal(t, basicAuthHeader("root"), fsHandler.gotAuth,
					"the Filesystem service must be reached over TLS with the user identity")
			},
		},
		{
			name: "filesystem remove over connect-gRPC",
			invoke: func(ctx context.Context, rt Runtime) error {
				return rt.Filesystem().Remove(ctx, RemovePathRequest{Path: "/var/opt/token", AuthUser: "root"})
			},
			verify: func(t *testing.T) {
				assert.Equal(t, basicAuthHeader("root"), fsHandler.gotAuth)
			},
		},
		{
			name: "process run over connect-gRPC",
			invoke: func(ctx context.Context, rt Runtime) error {
				res, err := rt.Process().Run(ctx, RunCommandRequest{
					ProcessConfig: &process.ProcessConfig{Cmd: "true"},
					Timeout:       5 * time.Second,
					AuthUser:      "root",
				})
				if err == nil {
					assert.True(t, res.Exited, "the End event must be observed over TLS")
				}
				return err
			},
		},
		{
			// Chmod is the follow-up a credential writer needs to tighten the
			// file mode, so it has to be TLS-capable together with Write.
			name: "process chmod over connect-gRPC",
			invoke: func(ctx context.Context, rt Runtime) error {
				return rt.Process().Chmod(ctx, "/var/opt/token", "0600")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mu.Lock()
			gotHosts = nil
			mu.Unlock()

			require.NoError(t, tt.invoke(context.Background(), newTLSRuntime()))
			if tt.verify != nil {
				tt.verify(t)
			}

			mu.Lock()
			defer mu.Unlock()
			require.NotEmpty(t, gotHosts, "the call must reach the HTTPS server")
			for _, h := range gotHosts {
				// Addressed by certificate hostname even though the connection
				// was pinned to the Pod IP.
				assert.Contains(t, h, authority)
			}
		})
	}
}

// TestTLSMode_CapabilityGroupsRejectInvalidBundle verifies that an unusable TLS
// bundle is reported by the multipart and connect capability groups too, instead
// of silently falling back to the plaintext runtime URL. Without this, a broken
// certificate mount would downgrade credential delivery to clear text.
func TestTLSMode_CapabilityGroupsRejectInvalidBundle(t *testing.T) {
	rt := NewRuntime(
		sandboxWithPodIP("10.0.0.1"),
		WithRetry(wait.Backoff{Steps: 1}),
		WithTLS(TLSBundle{CABundle: []byte("not a pem")}),
	)

	tests := []struct {
		name   string
		invoke func(ctx context.Context) error
	}{
		{
			name: "filesystem write",
			invoke: func(ctx context.Context) error {
				_, err := rt.Filesystem().Write(ctx, WriteFileRequest{FilePath: "/tmp/f", AuthUser: "root"})
				return err
			},
		},
		{
			name: "filesystem listdir",
			invoke: func(ctx context.Context) error {
				_, err := rt.Filesystem().ListDir(ctx, ListDirRequest{Path: "/tmp", AuthUser: "root"})
				return err
			},
		},
		{
			// Remove deletes recursively, so a broken bundle must stop the call
			// outright rather than replay it over plaintext.
			name: "filesystem remove",
			invoke: func(ctx context.Context) error {
				return rt.Filesystem().Remove(ctx, RemovePathRequest{Path: "/tmp/f", AuthUser: "root"})
			},
		},
		{
			name: "process run",
			invoke: func(ctx context.Context) error {
				_, err := rt.Process().Run(ctx, RunCommandRequest{
					ProcessConfig: &process.ProcessConfig{Cmd: "true"}, Timeout: time.Second,
				})
				return err
			},
		},
		{
			// Chmod wraps Run, so the case also pins that the wrapping keeps the
			// TLS configuration failure visible instead of masking it as a chmod
			// exit status.
			name: "process chmod",
			invoke: func(ctx context.Context) error {
				return rt.Process().Chmod(ctx, "/tmp/f", "0600")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.invoke(context.Background())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "invalid runtime TLS configuration")
		})
	}
}
