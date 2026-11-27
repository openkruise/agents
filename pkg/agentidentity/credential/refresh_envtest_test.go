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

package credential

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/openkruise/agents/pkg/identity/aead"
	"github.com/openkruise/agents/pkg/identity/delegation"
)

// rotatingTokenEndpoint behaves like GitHub's: every refresh issues a new pair
// and spends the refresh token it was given, along with the access token
// issued beside it, and a spent refresh token is answered with
// bad_refresh_token in a 200.
type rotatingTokenEndpoint struct {
	mu        sync.Mutex
	issued    int
	access    string
	refresh   string
	successes int
	refusals  int
}

func (e *rotatingTokenEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.PostForm.Get("refresh_token") != e.refresh {
		e.refusals++
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "bad_refresh_token"})
		return
	}
	e.successes++
	e.issued++
	e.access = fmt.Sprintf("ghu_access_%d", e.issued)
	e.refresh = fmt.Sprintf("ghr_refresh_%d", e.issued)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": e.access, "refresh_token": e.refresh, "token_type": "bearer", "expires_in": 28800,
	})
}

func (e *rotatingTokenEndpoint) current() (access, refresh string, successes, refusals int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.access, e.refresh, e.successes, e.refusals
}

// TestTokenAcrossReplicasAgainstARealAPIServer is section 8.3's claim that
// store versions, not singleflight, keep replicas consistent, checked where it
// can actually fail: two replicas, each with its own informer cache, against
// one API server and a provider that spends a refresh token on first use.
//
// Each round makes the delegation due for refresh and has both replicas ask
// for it at once. Exactly one refresh can succeed, and the other replica is
// told its refresh token is bad. What must hold is that the store ends every
// round holding the pair the provider now honours. If the loser's refusal
// were allowed to stand, or the winner's write were dropped against a lagging
// cache, the store would be left with a spent token and the user would have
// to authorize again for nothing.
func TestTokenAcrossReplicasAgainstARealAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set; run under make test or point it at envtest binaries")
	}
	ctrl.SetLogger(klog.Background())

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		Scheme:                scheme,
	}
	restConfig, err := env.Start()
	if err != nil {
		t.Fatalf("start the API server: %v", err)
	}
	t.Cleanup(func() {
		err := env.Stop()
		if runtime.GOOS == "windows" {
			// Stop does not reliably end the processes on Windows.
			for _, image := range []string{"kube-apiserver.exe", "etcd.exe"} {
				_ = exec.Command("taskkill", "/F", "/IM", image).Run()
			}
			return
		}
		if err != nil {
			t.Errorf("stop the API server: %v", err)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	direct, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("build a client: %v", err)
	}
	for _, name := range []string{testNamespace, storeNamespace} {
		if err := direct.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatalf("create namespace %s: %v", name, err)
		}
	}

	endpoint := &rotatingTokenEndpoint{refresh: oldRefresh}
	server := httptest.NewTLSServer(endpoint)
	t.Cleanup(server.Close)

	// Created through admission, so the provider is one the CRD accepts, then
	// marked Ready the way its controller would, since none runs here.
	provider := readyProvider(server.URL + "/login/oauth/access_token")
	status := provider.Status
	if err := direct.Create(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	provider.Status = status
	provider.Status.ObservedGeneration = provider.Generation
	provider.Status.Conditions[0].ObservedGeneration = provider.Generation
	if err := direct.Status().Update(ctx, provider); err != nil {
		t.Fatalf("mark provider ready: %v", err)
	}
	if err := direct.Create(ctx, usableSecret()); err != nil {
		t.Fatalf("create client secret: %v", err)
	}

	secret := make([]byte, aead.KeySize)
	if _, err := rand.Read(secret); err != nil {
		t.Fatalf("generate keyring secret: %v", err)
	}
	keyring, err := aead.NewKeyring(aead.Key{ID: "test-1", Secret: secret})
	if err != nil {
		t.Fatalf("build keyring: %v", err)
	}
	inspect, err := delegation.NewSecretDelegationStore(direct, keyring, storeNamespace)
	if err != nil {
		t.Fatalf("build delegation store: %v", err)
	}

	type replica struct {
		refresher *Refresher
		store     *delegation.SecretDelegationStore
	}
	replicas := make([]replica, 2)
	for i := range replicas {
		mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
			Scheme:                 scheme,
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
		})
		if err != nil {
			t.Fatalf("build replica %d's manager: %v", i, err)
		}
		managerCtx, stop := context.WithCancel(ctx)
		exited := make(chan error, 1)
		go func() { exited <- mgr.Start(managerCtx) }()
		t.Cleanup(func() {
			stop()
			select {
			case err := <-exited:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Errorf("replica %d's manager exited with %v", i, err)
				}
			case <-time.After(30 * time.Second):
				t.Errorf("replica %d's manager did not shut down", i)
			}
		})
		if !mgr.GetCache().WaitForCacheSync(managerCtx) {
			t.Fatalf("replica %d's cache did not sync", i)
		}

		store, err := delegation.NewSecretDelegationStore(mgr.GetClient(), keyring, storeNamespace)
		if err != nil {
			t.Fatalf("build replica %d's store: %v", i, err)
		}
		refresher := NewRefresher(mgr.GetClient(), store)
		// The provider's certificate is the test server's own. Redirects and
		// the timeout stay as NewRefresher set them.
		refresher.httpClient.Transport = server.Client().Transport
		replicas[i] = replica{refresher: refresher, store: store}
	}

	seeded := activeDelegation(time.Minute, oldRefresh)
	if err := inspect.Put(ctx, seeded); err != nil {
		t.Fatalf("seed delegation: %v", err)
	}

	const (
		rounds            = 10
		callersPerReplica = 4
	)
	transient := 0
	for round := 1; round <= rounds; round++ {
		due := seeded.Version
		if round > 1 {
			// Make it due again without touching its tokens, which is what
			// time passing does.
			record, err := inspect.Get(ctx, testKey)
			if err != nil {
				t.Fatalf("round %d: read delegation: %v", round, err)
			}
			record.AccessTokenExpiresAt = time.Now().Add(time.Minute)
			if err := inspect.Put(ctx, record); err != nil {
				t.Fatalf("round %d: make the delegation due: %v", round, err)
			}
			due = record.Version
		}
		// Both caches see the due record before anyone asks. Without this a
		// replica could still hold the previous round's record, which this
		// test made fresh by hand rather than by refreshing, and hand out an
		// access token the provider is about to retire.
		for i, r := range replicas {
			awaitVersion(t, ctx, r.store, due, fmt.Sprintf("round %d: replica %d", round, i))
		}

		type outcome struct {
			access string
			err    error
		}
		outcomes := make(chan outcome, len(replicas)*callersPerReplica)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, r := range replicas {
			for range callersPerReplica {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					got, err := r.refresher.Token(ctx, testKey)
					if err != nil {
						outcomes <- outcome{err: err}
						return
					}
					outcomes <- outcome{access: got.AccessToken}
				}()
			}
		}
		close(start)
		wg.Wait()
		close(outcomes)

		access, refresh, successes, _ := endpoint.current()
		if successes != round {
			t.Fatalf("round %d: %d successful refreshes in total, want exactly one per round", round, successes)
		}
		for o := range outcomes {
			switch {
			case o.err == nil:
				if o.access != access {
					t.Errorf("round %d: a caller got access token %q, want the one the provider honours, %q",
						round, o.access, access)
				}
			case errors.Is(o.err, ErrReauthorizationRequired), errors.Is(o.err, delegation.ErrConflict):
				// A caller on the replica that lost can be told this before the
				// winner's write lands. It clears on the next read, checked below.
				transient++
			default:
				t.Errorf("round %d: a caller failed with %v", round, o.err)
			}
		}

		stored, err := inspect.Get(ctx, testKey)
		if err != nil {
			t.Fatalf("round %d: read delegation: %v", round, err)
		}
		if stored.State != delegation.DelegationActive || stored.AccessToken != access || stored.RefreshToken != refresh {
			t.Fatalf("round %d: the store holds a %s delegation with access %q and refresh %q, "+
				"want the pair the provider honours, %q and %q",
				round, stored.State, stored.AccessToken, stored.RefreshToken, access, refresh)
		}
		for i, r := range replicas {
			awaitVersion(t, ctx, r.store, stored.Version, fmt.Sprintf("round %d: replica %d", round, i))
			got, err := r.refresher.Token(ctx, testKey)
			if err != nil {
				t.Fatalf("round %d: replica %d after the round: %v", round, i, err)
			}
			if got.AccessToken != access {
				t.Errorf("round %d: replica %d after the round got %q, want %q", round, i, got.AccessToken, access)
			}
		}
	}

	_, _, successes, refusals := endpoint.current()
	t.Logf("%d rounds, %d callers each: %d refreshes succeeded, %d spent refresh tokens were refused, "+
		"%d callers were told to retry or reauthorize before the winning write landed",
		rounds, len(replicas)*callersPerReplica, successes, refusals, transient)
}

// awaitVersion waits until store reads the delegation at version.
func awaitVersion(t *testing.T, ctx context.Context, store *delegation.SecretDelegationStore,
	version, who string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if record, err := store.Get(ctx, testKey); err == nil && record.Version == version {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never observed delegation version %s", who, version)
}
