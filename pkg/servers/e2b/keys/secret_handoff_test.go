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

package keys

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/pkg/configreload"
)

// TestSecretReloadObservationGuard keeps the guard itself under test: an
// unparsable resource version must fail instead of publishing, and a reload
// whose cache never catches up must stay cancellable. The bootstrap handoff
// itself is covered against a real informer below.
func TestSecretReloadObservationGuard(t *testing.T) {
	storage, api := newSecretStorageForTestWithKey(t, reloadTestAdminA, nil)
	_, err := storage.Reload(t.Context(), map[string]string{configreload.AdminKey: reloadTestAdminA})
	require.NoError(t, err)
	current := getSecretForTest(t, api)
	stale := &staleSecretReadClient{Client: api}
	storage.Client, storage.Cache = stale, newStubCache()
	for _, version := range []string{"", "opaque"} {
		invalid := current.DeepCopy()
		invalid.ResourceVersion = version
		stale.SetSecret(invalid)
		require.Error(t, storage.refresh(t.Context(), stale))
	}
	// Reload must remain cancellable when its cache cannot catch up.
	stale.SetSecret(current)
	storage.Run()
	t.Cleanup(storage.Stop)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err = storage.Reload(ctx, map[string]string{configreload.AdminKey: reloadTestAdminB})
	require.Error(t, err)
}

type handoffCachedClient struct {
	client.Client
	reader client.Reader
	reads  chan struct{}
}

func (c handoffCachedClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := c.reader.Get(ctx, key, obj, opts...)
	select {
	case c.reads <- struct{}{}:
	default:
	}
	return err
}

func TestSecretReloadBootstrapRealInformer(t *testing.T) {
	storage, api := newSecretStorageForTestWithKey(t, reloadTestAdminA, nil)
	_, err := storage.Reload(t.Context(), map[string]string{configreload.AdminKey: reloadTestAdminA})
	require.NoError(t, err)
	previous := getSecretForTest(t, api)
	_, err = storage.Reload(t.Context(), map[string]string{configreload.AdminKey: reloadTestAdminB})
	require.NoError(t, err)
	updates := make(chan watch.Event)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("sendInitialEvents") == "true" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Reason: metav1.StatusReasonBadRequest, Code: 400, Message: "watch-list unsupported"})
			return
		}
		if r.URL.Query().Get("watch") == "true" {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			for {
				select {
				case next := <-updates:
					_ = json.NewEncoder(w).Encode(map[string]any{"type": next.Type, "object": next.Object})
					w.(http.Flusher).Flush()
				case <-r.Context().Done():
					return
				}
			}
		}
		_ = json.NewEncoder(w).Encode(corev1.SecretList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "SecretList"}, ListMeta: metav1.ListMeta{ResourceVersion: previous.ResourceVersion}, Items: []corev1.Secret{*previous}})
	}))
	defer server.Close()
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{corev1.SchemeGroupVersion})
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Secret"), meta.RESTScopeNamespace)
	observed, err := ctrlcache.New(&rest.Config{Host: server.URL}, ctrlcache.Options{Scheme: newTestScheme(t), Mapper: mapper, DefaultNamespaces: map[string]ctrlcache.Config{"default": {}}})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cacheDone := make(chan error, 1)
	go func() { cacheDone <- observed.Start(ctx) }()
	defer func() { cancel(); require.NoError(t, <-cacheDone) }()
	reads := make(chan struct{}, 1)
	storage.Client, storage.Cache = handoffCachedClient{Client: api, reader: observed, reads: reads}, observed
	storage.Run()
	defer storage.Stop()
	storage.triggerRefresh()
	select {
	case <-reads:
	case <-time.After(time.Second):
		t.Fatal("informer refresh did not run")
	}
	storage.refreshMu.Lock()
	storage.refreshMu.Unlock()
	_, found := storage.LoadByKey(t.Context(), reloadTestAdminA)
	require.False(t, found, "real informer initial LIST must not undo Accepted B")
	_, found = storage.LoadByKey(t.Context(), reloadTestAdminB)
	require.True(t, found)

	// Skip B in the watch stream; a genuine newer store edit back to A must
	// still converge. Large revisions also exercise arbitrary-width versions.
	for _, update := range []struct {
		typ    watch.EventType
		secret *corev1.Secret
	}{
		{watch.Modified, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: KeySecretName, Namespace: "default", ResourceVersion: "1000000000000000000000000000000"}, Data: previous.Data}},
		{watch.Deleted, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: KeySecretName, Namespace: "default", ResourceVersion: "1000000000000000000000000000001"}, Data: previous.Data}},
		{watch.Added, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: KeySecretName, Namespace: "default", UID: "recreated", ResourceVersion: "1000000000000000000000000000002"}, Data: map[string][]byte{}}},
	} {
		next := update.secret
		next.APIVersion, next.Kind = "v1", "Secret"
		select {
		case updates <- watch.Event{Type: update.typ, Object: next}:
		case <-time.After(time.Second):
			t.Fatal("watch update was not consumed")
		}
		wantA := len(next.Data) > 0
		require.Eventually(t, func() bool {
			_, a := storage.LoadByKey(t.Context(), reloadTestAdminA)
			_, b := storage.LoadByKey(t.Context(), reloadTestAdminB)
			return a == wantA && !b
		}, time.Second, time.Millisecond)
	}
	require.Equal(t, reloadTestAdminB, storage.AdminKey)
	// Refreshes only observed the simulated server; no ensure-admin write.
	require.True(t, adminKeyStateSatisfied(getSecretForTest(t, api), reloadTestAdminB))
}
