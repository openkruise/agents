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

package delegation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/openkruise/agents/pkg/identity/aead"
)

// TestKeyRotationAgainstARealAPIServer walks the rotation the encryption key
// guide describes, against a real kube-apiserver: records sealed under the
// old key, the new key made active with the old one retained, a sweep, and
// the old key dropped. Another writer keeps updating some of the delegations
// while the sweep runs, which is where compare-and-swap has to hold, since
// the fake client only emulates it.
func TestKeyRotationAgainstARealAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set; run under make test or point it at envtest binaries")
	}
	env := &envtest.Environment{Scheme: testScheme(t)}
	cfg, err := env.Start()
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

	ctx := context.Background()
	c, err := client.New(cfg, client.Options{Scheme: testScheme(t)})
	if err != nil {
		t.Fatalf("build a client: %v", err)
	}
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: storeNamespace}}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	previous := aead.Key{ID: "2026-05", Secret: bytes.Repeat([]byte{0x22}, aead.KeySize)}
	current := aead.Key{ID: "2026-11", Secret: bytes.Repeat([]byte{0x11}, aead.KeySize)}
	storeUnder := func(active aead.Key, retained ...aead.Key) *SecretDelegationStore {
		t.Helper()
		keyring, err := aead.NewKeyring(active, retained...)
		if err != nil {
			t.Fatalf("build keyring: %v", err)
		}
		store, err := NewSecretDelegationStore(c, keyring, storeNamespace)
		if err != nil {
			t.Fatalf("NewSecretDelegationStore: %v", err)
		}
		return store
	}

	const delegations = 30
	keys := make([]DelegationKey, delegations)
	before := storeUnder(previous)
	for i := range keys {
		keys[i] = testDelegationKey()
		keys[i].AgentIdentity = fmt.Sprintf("agent-%02d", i)
		record := testDelegation(time.Now())
		record.Key = keys[i]
		record.AccessToken = "gho_sealed_before_rotation"
		if err := before.Put(ctx, record); err != nil {
			t.Fatalf("seed %s: %v", keys[i].AgentIdentity, err)
		}
	}

	rotated := storeUnder(current, previous)

	// Every third delegation is rewritten through the rotated store while the
	// sweep runs, the way a refresh would, and must keep what it wrote.
	written := map[int]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < delegations; i += 3 {
			for {
				record, err := rotated.Get(ctx, keys[i])
				if err != nil {
					t.Errorf("writer read %s: %v", keys[i].AgentIdentity, err)
					return
				}
				record.AccessToken = fmt.Sprintf("gho_refreshed_during_sweep_%02d", i)
				err = rotated.Put(ctx, record)
				if errors.Is(err, ErrConflict) {
					continue
				}
				if err != nil {
					t.Errorf("writer put %s: %v", keys[i].AgentIdentity, err)
					return
				}
				mu.Lock()
				written[i] = record.AccessToken
				mu.Unlock()
				break
			}
		}
	}()

	// Sweep until nothing is left. A record the writer moved under the sweep
	// is skipped by that pass and, sealed under the active key already by the
	// writer, needs nothing from the next.
	total := 0
	for pass := 1; ; pass++ {
		resealed, err := rotated.ResealDelegations(ctx)
		if err != nil {
			t.Fatalf("sweep %d: %v", pass, err)
		}
		total += resealed
		if resealed == 0 && pass > 1 {
			break
		}
		if pass > 10 {
			t.Fatal("the sweep never settled")
		}
	}
	wg.Wait()

	afterDrop := storeUnder(current)
	for i, key := range keys {
		got, err := afterDrop.Get(ctx, key)
		if err != nil {
			t.Errorf("%s does not open once the old key is dropped: %v", key.AgentIdentity, err)
			continue
		}
		want := "gho_sealed_before_rotation"
		if w, ok := written[i]; ok {
			want = w
		}
		if got.AccessToken != want {
			t.Errorf("%s access token: got %q, want %q", key.AgentIdentity, got.AccessToken, want)
		}
	}
	t.Logf("%d delegations, %d rewritten during the sweep, %d re-sealed by it", delegations, len(written), total)
}
