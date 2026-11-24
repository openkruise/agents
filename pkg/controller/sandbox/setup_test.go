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

package sandbox

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/hashicorp/memberlist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/controller/sandbox/core"
	"github.com/openkruise/agents/pkg/features"
	"github.com/openkruise/agents/pkg/peers"
	"github.com/openkruise/agents/pkg/peersecurity"
	"github.com/openkruise/agents/pkg/sandboxroute"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
)

// newSandboxTestManager creates a real controller-runtime Manager backed by a
// stub REST config. The manager is never started; we only exercise controller
// registration, so no running apiserver is required.
func newSandboxTestManager(t *testing.T) ctrl.Manager {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add clientgo scheme: %v", err)
	}
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add agents scheme: %v", err)
	}
	mgr, err := ctrl.NewManager(&rest.Config{Host: "http://127.0.0.1:0"}, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return mgr
}

// noopEnqueuer satisfies Enqueuer without side effects.
type noopEnqueuer struct{}

func (n *noopEnqueuer) Enqueue(_, _ string) {}

// TestSandboxReconciler_SetupWithManager verifies that SetupWithManager
// registers the sandbox controller with the manager without error. This
// exercises the wiring path (For/Watches/Named/Complete chain) against a real
// controller-runtime manager backed by a stub REST config; the manager is
// never started so no apiserver is needed.
func TestSandboxReconciler_SetupWithManager(t *testing.T) {
	mgr := newSandboxTestManager(t)

	r := &SandboxReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		controls: core.NewSandboxControl(core.SandboxControlArgs{
			Client:    mgr.GetClient(),
			APIReader: mgr.GetAPIReader(),
			Recorder:  mgr.GetEventRecorderFor("sandbox"),
		}),
		metricsCleanup: &noopEnqueuer{},
	}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatalf("SandboxReconciler.SetupWithManager: unexpected error: %v", err)
	}
}

func TestSetupRouteSyncValidation(t *testing.T) {
	assert.False(t, utilfeature.DefaultFeatureGate.Enabled(features.SandboxRouteSyncGate))
	tests := []struct {
		name        string
		gateEnabled bool
		selector    string
		bindAddress string
		bindPort    int
		security    peersecurity.Inputs
		wantErr     string
	}{
		{name: "defaults do not enable sync", selector: "agents.kruise.io/sandbox-manager-peer-finder=ack-sandbox-manager", bindPort: 7946},
		{name: "disabled gate skips selector address and port validation", selector: "component in (", bindAddress: "invalid-address", bindPort: 65536},
		{name: "custom port does not enable sync", selector: "agents.kruise.io/sandbox-manager-peer-finder=ack-sandbox-manager", bindPort: 9000},
		{name: "empty selector disables sync with gate enabled", gateEnabled: true, bindPort: 65536},
		{name: "enabled gate validates TLS pair", gateEnabled: true, selector: "component=manager", security: peersecurity.Inputs{TLSServerSecret: types.NamespacedName{Namespace: "ns", Name: "server"}}, wantErr: "both server and client"},
		{name: "enabled gate validates Secret reference", gateEnabled: true, selector: "component=manager", security: peersecurity.Inputs{PeerKeySecret: types.NamespacedName{Name: "key"}}, wantErr: "reference is incomplete"},
		{name: "disabled gate skips security validation", selector: "component=manager", security: peersecurity.Inputs{TLSServerSecret: types.NamespacedName{Namespace: "ns", Name: "server"}}},
		{name: "enabled gate validates selector", gateEnabled: true, selector: "component in (", bindPort: 7946, wantErr: "parse peer selector"},
		{name: "enabled gate validates port range", gateEnabled: true, selector: "agents.kruise.io/sandbox-manager-peer-finder=ack-sandbox-manager", bindPort: 65536, wantErr: "--memberlist-bind-port must be in the range 1-65535"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SandboxRouteSyncGate, tt.gateEnabled)
			// All cases must return before accessing the manager or starting listeners.
			sender, err := SetupRouteSync(nil, tt.selector, tt.bindAddress, tt.bindPort, tt.security)
			assert.Nil(t, sender)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestRouteSyncLifecycle(t *testing.T) {
	tests := []struct {
		name          string
		hostname      string
		podName       string
		wantNodeName  string
		bindAddress   string
		podIP         string
		peerSelector  string
		cancelSync    bool
		setupOnly     bool
		bindPort      int
		wantBindPort  int
		lateKey       bool
		lateTLS       bool
		forbiddenKey  bool
		encryptedJoin bool
	}{
		{name: "default port", setupOnly: true, bindPort: 7946, wantBindPort: 7946},
		{name: "zero port falls back to default", setupOnly: true, wantBindPort: 7946},
		{name: "negative port falls back to default", setupOnly: true, bindPort: -1, wantBindPort: 7946},
		{name: "maximum port is valid", setupOnly: true, bindPort: 65535, wantBindPort: 65535},
		{name: "override default selector", peerSelector: "component=sandbox-manager"},
		{name: "HOSTNAME takes precedence", hostname: "controller-hostname", podName: "controller-pod", wantNodeName: "controller-hostname"},
		{name: "POD_NAME fallback", podName: "controller-route-sync-test", wantNodeName: "controller-route-sync-test"},
		{name: "UUID fallback without environment names"},
		{name: "explicit bind address overrides POD_IP", bindAddress: "127.0.0.1", podIP: "192.0.2.1"},
		{name: "Secret loading is cancelable", cancelSync: true},
		{name: "key arriving after controller startup", lateKey: true},
		{name: "TLS arriving after controller startup", lateTLS: true},
		{name: "Secret permissions do not block other controllers", forbiddenKey: true},
		{name: "memberlist accepts only matching encryption key", encryptedJoin: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SandboxRouteSyncGate, true)
			peerSelector := tt.peerSelector
			if peerSelector == "" {
				peerSelector = "agents.kruise.io/sandbox-manager-peer-finder=ack-sandbox-manager"
			}
			t.Setenv("HOSTNAME", tt.hostname)
			t.Setenv("POD_NAME", tt.podName)
			podIP := tt.podIP
			if podIP == "" {
				podIP = "127.0.0.1"
			}
			t.Setenv("POD_IP", podIP)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			requestStarted := make(chan struct{}, 1)
			allowKey := make(chan struct{})
			allowTLS := make(chan struct{})
			keyRequested := make(chan struct{}, 1)
			security, secrets := routeSyncTestTLS(t)
			// Use the real peer client, loader, and memberlist; stub only Kubernetes API HTTP responses.
			apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Empty(t, r.URL.Query().Get("watch"))
				select {
				case requestStarted <- struct{}{}:
				default:
				}
				if tt.cancelSync {
					<-r.Context().Done()
					return
				}
				var response interface{}
				if r.URL.Path == "/api/v1/namespaces/sandbox-system/pods" {
					assert.Equal(t, peerSelector, r.URL.Query().Get("labelSelector"))
					response = &corev1.PodList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"}, Items: []corev1.Pod{}}
				} else {
					assert.Empty(t, r.URL.RawQuery)
					for ref, secret := range secrets {
						if r.URL.Path != "/api/v1/namespaces/"+ref.Namespace+"/secrets/"+ref.Name {
							continue
						}
						if ref == security.PeerKeySecret {
							select {
							case keyRequested <- struct{}{}:
							default:
							}
							if tt.forbiddenKey {
								http.Error(w, "forbidden", http.StatusForbidden)
								return
							}
							if tt.lateKey {
								select {
								case <-allowKey:
								default:
									http.NotFound(w, r)
									return
								}
							}
						} else if tt.lateTLS {
							select {
							case <-allowTLS:
							default:
								http.NotFound(w, r)
								return
							}
						}
						response = secret
						break
					}
				}
				if response == nil {
					t.Errorf("unexpected API request: %s", r.URL.String())
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(response); err != nil && r.Context().Err() == nil {
					t.Errorf("failed to encode API response: %v", err)
				}
			}))
			defer apiServer.Close()
			scheme := runtime.NewScheme()
			require.NoError(t, clientgoscheme.AddToScheme(scheme))
			mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{corev1.SchemeGroupVersion})
			mapper.Add(corev1.SchemeGroupVersion.WithKind("Pod"), meta.RESTScopeNamespace)
			mapper.AddSpecific(corev1.SchemeGroupVersion.WithKind("PodList"), corev1.SchemeGroupVersion.WithResource("pods"), corev1.SchemeGroupVersion.WithResource("pod"), meta.RESTScopeNamespace)
			mapper.Add(corev1.SchemeGroupVersion.WithKind("Secret"), meta.RESTScopeNamespace)
			mapper.AddSpecific(corev1.SchemeGroupVersion.WithKind("SecretList"), corev1.SchemeGroupVersion.WithResource("secrets"), corev1.SchemeGroupVersion.WithResource("secret"), meta.RESTScopeNamespace)
			mgr, err := ctrl.NewManager(&rest.Config{Host: apiServer.URL}, ctrl.Options{
				Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
				MapperProvider: func(*rest.Config, *http.Client) (meta.RESTMapper, error) { return mapper, nil },
			})
			require.NoError(t, err)
			port := tt.bindPort
			wantPort := tt.wantBindPort
			if !tt.setupOnly {
				// Pass an available port through setup without occupying the production default.
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				require.NoError(t, err)
				port = listener.Addr().(*net.TCPAddr).Port
				require.NoError(t, listener.Close())
				wantPort = port
			}
			sender, err := SetupRouteSync(mgr, peerSelector, tt.bindAddress, port, security)
			require.NoError(t, err)
			require.NotNil(t, sender)
			assert.Equal(t, wantPort, sender.bindPort)
			assert.Equal(t, tt.bindAddress, sender.bindAddress)
			assert.NotSame(t, mgr.GetClient(), sender.peerReader)
			assert.False(t, sender.NeedLeaderElection())
			assert.Equal(t, peers.DefaultJoinRetryInterval, sender.securityRetryInterval)
			if tt.setupOnly {
				return
			}
			sender.securityRetryInterval = 10 * time.Millisecond
			done := make(chan error, 1)
			managed := tt.lateKey || tt.lateTLS || tt.forbiddenKey
			if managed {
				controllerStarted := make(chan struct{})
				require.NoError(t, mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
					close(controllerStarted)
					<-ctx.Done()
					return nil
				})))
				go func() { done <- mgr.Start(ctx) }()
				select {
				case <-controllerStarted:
				case <-ctx.Done():
					t.Fatal("security wait blocked controller startup")
				}
				select {
				case <-sender.ready:
					t.Fatal("missing security must not publish ready")
				default:
				}
				select {
				case <-keyRequested:
				case <-ctx.Done():
					t.Fatal("timed out waiting for scoped Secret request")
				}
				pendingCtx, stop := context.WithTimeout(ctx, 20*time.Millisecond)
				assert.ErrorIs(t, sender.SyncRouteWithPeers(pendingCtx, sandboxroute.Route{}), context.DeadlineExceeded)
				stop()
				assert.Empty(t, sender.peers.GetAllMembers())
				if tt.lateKey {
					close(allowKey)
				}
				if tt.lateTLS {
					close(allowTLS)
				}
			} else {
				go func() { done <- sender.Start(ctx) }()
			}
			// Cancel peer work even if an assertion fails, so background tasks cannot block test cleanup.
			defer cancel()
			if tt.forbiddenKey {
				cancel()
			} else if tt.cancelSync {
				select {
				case <-requestStarted:
				case <-ctx.Done():
					t.Fatal("timed out waiting for Secret request")
				}
				cancel()
			} else {
				select {
				case <-sender.ready:
				case err := <-done:
					t.Fatalf("component exited before becoming ready: %v", err)
				case <-ctx.Done():
					t.Fatal("timed out waiting for peer startup")
				}
				assert.Equal(t, "https", sender.outbound.Scheme())
				members := sender.peers.GetAllMembers()
				require.Len(t, members, 1)
				assert.Equal(t, "127.0.0.1", members[0].IP)
				assert.Equal(t, "127.0.0.1", sender.peers.LocalAddr().String())
				assert.Equal(t, port, sender.peers.LocalPort())
				if tt.wantNodeName != "" {
					assert.Equal(t, peers.NodePrefixSandboxController+tt.wantNodeName, members[0].Name)
				} else {
					assert.Regexp(t, "^"+peers.NodePrefixSandboxController+"[0-9a-f]{8}$", members[0].Name)
				}
				assert.Empty(t, sender.peers.GetPeers())
				var logs bytes.Buffer
				var logMu sync.Mutex
				logCtx := klog.NewContext(ctx, funcr.NewJSON(func(line string) {
					logMu.Lock()
					defer logMu.Unlock()
					logs.WriteString(line)
				}, funcr.Options{}))
				var sends sync.WaitGroup
				for range 20 {
					sends.Add(1)
					go func() {
						defer sends.Done()
						assert.NoError(t, sender.SyncRouteWithPeers(logCtx, sandboxroute.Route{}))
					}()
				}
				sends.Wait()
				assert.Equal(t, 1, strings.Count(logs.String(), "route sync skipped: no peers discovered"))
				assert.NotContains(t, logs.String(), "synced Sandbox route with peers")
				sender.lastNoPeersLog.Store(time.Now().Add(-time.Minute).UnixNano())
				require.NoError(t, sender.SyncRouteWithPeers(logCtx, sandboxroute.Route{}))
				assert.Equal(t, 2, strings.Count(logs.String(), "route sync skipped: no peers discovered"))
				canceled, stop := context.WithCancel(ctx)
				stop()
				assert.ErrorIs(t, sender.SyncRouteWithPeers(canceled, sandboxroute.Route{}), context.Canceled)
				if tt.encryptedJoin {
					for _, matching := range []bool{true, false} {
						config := memberlist.DefaultLANConfig()
						config.Name = peers.NodePrefixSandboxManager + strconv.FormatBool(matching)
						config.BindAddr, config.AdvertiseAddr = "127.0.0.1", "127.0.0.1"
						config.BindPort, config.AdvertisePort = 0, 0
						config.TCPTimeout = 200 * time.Millisecond
						config.LogOutput = io.Discard
						config.SecretKey = bytes.Repeat([]byte{'k'}, 32)
						if !matching {
							config.SecretKey = bytes.Repeat([]byte{'x'}, 32)
						}
						receiver, err := memberlist.Create(config)
						require.NoError(t, err)
						t.Cleanup(func() { assert.NoError(t, receiver.Shutdown()) })
						joined, err := receiver.Join([]string{net.JoinHostPort("127.0.0.1", strconv.Itoa(port))})
						if matching {
							require.NoError(t, err)
							assert.Equal(t, 1, joined)
							assert.Len(t, receiver.Members(), 2)
							require.Eventually(t, func() bool { return len(sender.peers.GetPeers()) == 1 }, time.Second, 10*time.Millisecond)
						} else {
							require.Error(t, err)
							assert.Zero(t, joined)
						}
					}
				}
				cancel()
			}
			select {
			case err := <-done:
				if tt.cancelSync {
					require.ErrorIs(t, err, context.Canceled)
				} else {
					require.NoError(t, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("component did not exit in time")
			}
			if tt.cancelSync || tt.forbiddenKey {
				select {
				case <-sender.ready:
					t.Fatal("canceled startup must not publish ready")
				default:
				}
				canceled, stop := context.WithCancel(t.Context())
				stop()
				assert.ErrorIs(t, sender.SyncRouteWithPeers(canceled, sandboxroute.Route{}), context.Canceled)
			}
		})
	}
}

// routeSyncTestTLS provides the three Secrets required by the shared Load function without reading cluster certificates.
func routeSyncTestTLS(t *testing.T) (peersecurity.Inputs, map[types.NamespacedName]*corev1.Secret) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "route-sync-test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	ca, err = x509.ParseCertificate(caDER)
	require.NoError(t, err)
	inputs := peersecurity.Inputs{
		PeerKeySecret:   types.NamespacedName{Namespace: "sandbox-system", Name: "peer-key"},
		TLSServerSecret: types.NamespacedName{Namespace: "peer-security", Name: "server"},
		TLSClientSecret: types.NamespacedName{Namespace: "peer-security", Name: "manager-client"},
	}
	secrets := map[types.NamespacedName]*corev1.Secret{
		inputs.PeerKeySecret: {
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: metav1.ObjectMeta{Namespace: inputs.PeerKeySecret.Namespace, Name: inputs.PeerKeySecret.Name},
			Data:       map[string][]byte{"key": bytes.Repeat([]byte{'k'}, 32)},
		},
	}
	for i, ref := range []types.NamespacedName{inputs.TLSServerSecret, inputs.TLSClientSecret} {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)
		cert := &x509.Certificate{
			SerialNumber: big.NewInt(int64(i + 2)), Subject: pkix.Name{CommonName: "sandbox-manager"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}
		if ref == inputs.TLSServerSecret {
			cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			cert.DNSNames = []string{"agentruntime.sandbox.agents.kruise.io"}
		}
		certDER, err := x509.CreateCertificate(rand.Reader, cert, ca, &key.PublicKey, caKey)
		require.NoError(t, err)
		keyDER, err := x509.MarshalECPrivateKey(key)
		require.NoError(t, err)
		secrets[ref] = &corev1.Secret{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: metav1.ObjectMeta{Namespace: ref.Namespace, Name: ref.Name},
			Data: map[string][]byte{
				"ca.crt":  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
				"tls.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}),
				"tls.key": pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
			},
		}
	}
	return inputs, secrets
}
