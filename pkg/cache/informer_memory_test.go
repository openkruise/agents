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

package cache

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	clientfeatures "k8s.io/client-go/features"
	clientfeaturestesting "k8s.io/client-go/features/testing"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	cachetesting "k8s.io/client-go/tools/cache/testing"
	"k8s.io/utils/ptr"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

func TestMemoryEfficientInformerTransform(t *testing.T) {
	newSandbox := func(name, uid, rv, payload string) *agentsv1alpha1.Sandbox {
		return &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:       "ns",
				Name:            name,
				UID:             types.UID(uid),
				ResourceVersion: rv,
				ManagedFields:   []metav1.ManagedFieldsEntry{{Manager: "test"}},
			},
			Spec: agentsv1alpha1.SandboxSpec{Paused: payload == "paused"},
		}
	}
	startInformer := func(t *testing.T, informer toolscache.SharedIndexInformer) <-chan struct{} {
		t.Helper()
		stop := make(chan struct{})
		done := make(chan struct{})
		t.Cleanup(func() {
			close(stop)
			<-done
		})
		go func() {
			defer close(done)
			informer.Run(stop)
		}()
		return stop
	}

	t.Run("same identity reuses canonical object before the base transform", func(t *testing.T) {
		canonical := newSandbox("same", "uid-1", "", "canonical")
		canonical.ManagedFields = nil
		source := cachetesting.NewFakeControllerSource()
		source.Add(canonical)
		informer := newMemoryEfficientInformer(source, &agentsv1alpha1.Sandbox{}, 0, toolscache.Indexers{})
		if err := informer.GetIndexer().Add(canonical); err != nil {
			t.Fatal(err)
		}

		var baseCalls atomic.Int32
		if err := informer.SetTransform(func(obj any) (any, error) {
			baseCalls.Add(1)
			sandbox := obj.(*agentsv1alpha1.Sandbox)
			sandbox.ManagedFields = nil
			return sandbox, nil
		}); err != nil {
			t.Fatal(err)
		}
		added := make(chan struct{}, 1)
		if _, err := informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
			AddFunc: func(obj any) {
				if obj.(*agentsv1alpha1.Sandbox).Name == "same" {
					added <- struct{}{}
				}
			},
			UpdateFunc: func(_, obj any) {
				if obj.(*agentsv1alpha1.Sandbox).Name == "same" {
					added <- struct{}{}
				}
			},
		}); err != nil {
			t.Fatal(err)
		}

		ctxStop := startInformer(t, informer)
		if !toolscache.WaitForCacheSync(ctxStop, informer.HasSynced) {
			t.Fatal("informer did not sync")
		}
		select {
		case <-added:
		case <-time.After(5 * time.Second):
			t.Fatal("same-identity event was not delivered")
		}
		got := waitForInformerObject(t, informer, "ns/same")
		if got != canonical {
			t.Fatalf("cache object = %p, want canonical %p", got, canonical)
		}
		if baseCalls.Load() != 0 {
			t.Fatalf("base transform called %d times for unchanged object, want 0", baseCalls.Load())
		}
		if len(canonical.ManagedFields) != 0 || canonical.Spec.Paused {
			t.Fatalf("canonical object was mutated: managedFields=%v paused=%v", canonical.ManagedFields, canonical.Spec.Paused)
		}
	})

	t.Run("miss and changed or incomplete identity keep the new object", func(t *testing.T) {
		cases := []struct {
			name     string
			incoming *agentsv1alpha1.Sandbox
		}{
			{name: "miss", incoming: newSandbox("missing", "uid-1", "rv-1", "new")},
			{name: "changed uid", incoming: newSandbox("same", "uid-2", "rv-1", "new")},
			{name: "changed resource version", incoming: newSandbox("same", "uid-1", "rv-2", "new")},
			{name: "empty uid", incoming: newSandbox("same", "", "rv-1", "new")},
			{name: "empty resource version", incoming: newSandbox("same", "uid-1", "", "new")},
		}
		for _, tt := range cases {
			t.Run(tt.name, func(t *testing.T) {
				canonical := newSandbox("same", "uid-1", "rv-1", "canonical")
				store := toolscache.NewIndexer(toolscache.DeletionHandlingMetaNamespaceKeyFunc, toolscache.Indexers{})
				if err := store.Add(canonical); err != nil {
					t.Fatal(err)
				}
				got, reused := reuseUnchangedObject(store, tt.incoming)
				if reused || got != tt.incoming {
					t.Fatalf("reuseUnchangedObject() = (%p, %v), want incoming %p", got, reused, tt.incoming)
				}
			})
		}
	})

	t.Run("reinstall replaces the prior transform", func(t *testing.T) {
		source := cachetesting.NewFakeControllerSource()
		informer := newMemoryEfficientInformer(source, &agentsv1alpha1.Sandbox{}, 0, toolscache.Indexers{})
		var firstCalls, secondCalls atomic.Int32
		if err := informer.SetTransform(func(obj any) (any, error) {
			firstCalls.Add(1)
			return obj, nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := informer.SetTransform(func(obj any) (any, error) {
			secondCalls.Add(1)
			return obj, nil
		}); err != nil {
			t.Fatal(err)
		}

		ctxStop := startInformer(t, informer)
		if !toolscache.WaitForCacheSync(ctxStop, informer.HasSynced) {
			t.Fatal("informer did not sync")
		}
		source.Add(newSandbox("replaced", "uid-1", "rv-1", "fresh"))
		waitForInformerObject(t, informer, "ns/replaced")
		if firstCalls.Load() != 0 || secondCalls.Load() != 1 {
			t.Fatalf("transform calls = (%d, %d), want (0, 1)", firstCalls.Load(), secondCalls.Load())
		}
	})

	t.Run("nil clears the configured transform", func(t *testing.T) {
		source := cachetesting.NewFakeControllerSource()
		informer := newMemoryEfficientInformer(source, &agentsv1alpha1.Sandbox{}, 0, toolscache.Indexers{})
		var baseCalls atomic.Int32
		if err := informer.SetTransform(func(obj any) (any, error) {
			baseCalls.Add(1)
			sandbox := obj.(*agentsv1alpha1.Sandbox)
			sandbox.Annotations = map[string]string{"base": "still-installed"}
			return sandbox, nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := informer.SetTransform(nil); err != nil {
			t.Fatal(err)
		}
		ctxStop := startInformer(t, informer)
		if !toolscache.WaitForCacheSync(ctxStop, informer.HasSynced) {
			t.Fatal("informer did not sync")
		}
		cleared := newSandbox("cleared", "uid-2", "rv-1", "fresh")
		source.Add(cleared)
		got := waitForInformerObject(t, informer, "ns/cleared").(*agentsv1alpha1.Sandbox)
		if len(got.ManagedFields) != 1 || got.Annotations["base"] != "" {
			t.Fatalf("SetTransform(nil) retained a base transform: %#v", got.ObjectMeta)
		}
		if baseCalls.Load() != 0 {
			t.Fatalf("cleared base transform called %d times, want 0", baseCalls.Load())
		}
	})

	t.Run("base transform error prevents the candidate from entering the cache", func(t *testing.T) {
		source := cachetesting.NewFakeControllerSource()
		informer := newMemoryEfficientInformer(source, &agentsv1alpha1.Sandbox{}, 0, toolscache.Indexers{})
		sentinel := errors.New("base transform failed")
		baseCalled := make(chan struct{}, 1)
		if err := informer.SetTransform(func(obj any) (any, error) {
			sandbox := obj.(*agentsv1alpha1.Sandbox)
			if sandbox.Name == "failed" {
				baseCalled <- struct{}{}
				return obj, sentinel
			}
			return obj, nil
		}); err != nil {
			t.Fatal(err)
		}
		ctxStop := startInformer(t, informer)
		if !toolscache.WaitForCacheSync(ctxStop, informer.HasSynced) {
			t.Fatal("informer did not sync")
		}
		source.Add(newSandbox("failed", "uid-1", "rv-1", "fresh"))
		select {
		case <-baseCalled:
		case <-time.After(5 * time.Second):
			t.Fatal("base transform was not called")
		}
		source.Add(newSandbox("marker", "uid-2", "rv-1", "processed"))
		waitForInformerObject(t, informer, "ns/marker")
		if _, exists, err := informer.GetIndexer().GetByKey("ns/failed"); err != nil || exists {
			t.Fatalf("failed transform candidate is cached: exists=%v err=%v", exists, err)
		}
	})
}

func TestMemoryEfficientInformerWatchListRecovery(t *testing.T) {
	clientfeaturestesting.SetFeatureDuringTest(t, clientfeatures.WatchListClient, true)
	gv := agentsv1alpha1.GroupVersion
	streams := make(chan chan watch.Event, 6)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("watch") != "true" {
			http.Error(w, "unexpected list request", http.StatusBadRequest)
			return
		}
		stream := make(chan watch.Event)
		select {
		case streams <- stream:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		encoder := json.NewEncoder(w)
		for {
			select {
			case event, ok := <-stream:
				if !ok {
					return
				}
				if err := encoder.Encode(map[string]any{"type": event.Type, "object": event.Object}); err != nil {
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
			case <-r.Context().Done():
				return
			}
		}
	}))
	t.Cleanup(server.Close)

	scheme := runtime.NewScheme()
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	codecs := serializer.NewCodecFactory(scheme).WithoutConversion()
	client, err := rest.RESTClientFor(&rest.Config{
		Host:    server.URL,
		APIPath: "/apis",
		ContentConfig: rest.ContentConfig{
			GroupVersion:         &gv,
			NegotiatedSerializer: codecs,
			ContentType:          "application/json",
			AcceptContentTypes:   "application/json",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	lw := toolscache.NewListWatchFromClient(client, "sandboxes", "ns", fields.Everything())
	informer := newMemoryEfficientInformer(lw, &agentsv1alpha1.Sandbox{}, 0, toolscache.Indexers{})
	var canonicalBaseCalls atomic.Int32
	markerStaged := make(chan struct{}, 1)
	if err := informer.SetTransform(func(obj any) (any, error) {
		sandbox := obj.(*agentsv1alpha1.Sandbox)
		if sandbox.Name == "canonical" {
			canonicalBaseCalls.Add(1)
		}
		if sandbox.Name == "marker" {
			select {
			case markerStaged <- struct{}{}:
			default:
			}
		}
		sandbox.ManagedFields = nil
		return sandbox, nil
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		<-runDone
	})
	go func() {
		defer close(runDone)
		informer.RunWithContext(ctx)
	}()
	getStream := func() chan watch.Event {
		t.Helper()
		select {
		case stream := <-streams:
			return stream
		case <-time.After(5 * time.Second):
			t.Fatal("informer did not open the expected WatchList stream")
			return nil
		}
	}
	send := func(stream chan watch.Event, event watch.Event) {
		t.Helper()
		select {
		case stream <- event:
		case <-time.After(5 * time.Second):
			t.Fatal("WatchList stream stopped accepting events")
		}
	}
	bookmark := func() watch.Event {
		return watch.Event{Type: watch.Bookmark, Object: &metav1.PartialObjectMetadata{
			TypeMeta: metav1.TypeMeta{APIVersion: gv.String(), Kind: "Sandbox"},
			ObjectMeta: metav1.ObjectMeta{
				ResourceVersion: "7",
				Annotations:     map[string]string{"k8s.io/initial-events-end": "true"},
			},
		}}
	}
	newSandbox := func(rv, payload string) *agentsv1alpha1.Sandbox {
		return &agentsv1alpha1.Sandbox{
			TypeMeta: metav1.TypeMeta{APIVersion: gv.String(), Kind: "Sandbox"},
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "ns", Name: "canonical", UID: types.UID("uid-1"), ResourceVersion: rv,
				ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "test"}},
			},
			Spec: agentsv1alpha1.SandboxSpec{Paused: payload == "changed"},
		}
	}
	statusExpired := watch.Event{Type: watch.Error, Object: &metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   metav1.StatusFailure, Code: http.StatusGone, Reason: metav1.StatusReasonExpired,
	}}

	first := getStream()
	initial := newSandbox("7", "canonical")
	send(first, watch.Event{Type: watch.Added, Object: initial})
	oldAbsent := newSandbox("7", "old-absent")
	oldAbsent.Name = "old-absent"
	oldAbsent.UID = "uid-old-absent"
	send(first, watch.Event{Type: watch.Added, Object: oldAbsent})
	send(first, bookmark())
	if !toolscache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		t.Fatal("initial WatchList did not sync at its bookmark")
	}
	canonical := waitForInformerObject(t, informer, "ns/canonical").(*agentsv1alpha1.Sandbox)
	if canonical.ResourceVersion != "7" {
		t.Fatalf("initial WatchList resourceVersion=%q, want 7", canonical.ResourceVersion)
	}
	if len(canonical.ManagedFields) != 0 {
		t.Fatal("initial object did not pass through the base transform")
	}
	waitForInformerObject(t, informer, "ns/old-absent")
	// Expire the stream after the initial bookmark, then fail a hot-reload
	// WatchList before its bookmark.
	send(first, statusExpired)
	close(first)
	second := getStream()
	failedReload := newSandbox("99", "uncommitted")
	send(second, watch.Event{Type: watch.Added, Object: failedReload})
	send(second, statusExpired)
	close(second)
	third := getStream()
	canonicalCallsAfterFailedReload := canonicalBaseCalls.Load()
	retry := newSandbox("7", "retry")
	send(third, watch.Event{Type: watch.Added, Object: retry})
	marker := newSandbox("7", "marker")
	marker.Name = "marker"
	send(third, watch.Event{Type: watch.Added, Object: marker})
	select {
	case <-markerStaged:
	case <-time.After(5 * time.Second):
		t.Fatal("retry staging did not transform its marker")
	}
	stored := waitForInformerObject(t, informer, "ns/canonical")
	if stored != canonical {
		t.Fatalf("cache changed before retry bookmark: stored=%p canonical=%p", stored, canonical)
	}
	if _, exists, err := informer.GetIndexer().GetByKey("ns/old-absent"); err != nil || !exists {
		t.Fatalf("complete replacement changed old key before retry bookmark: exists=%v err=%v", exists, err)
	}
	if canonicalBaseCalls.Load() != canonicalCallsAfterFailedReload {
		t.Fatalf("canonical base transform ran while retry was staged: before=%d after=%d", canonicalCallsAfterFailedReload, canonicalBaseCalls.Load())
	}
	send(third, bookmark())
	activeWatch := third
	waitForInformerObject(t, informer, "ns/marker")
	if _, exists, err := informer.GetIndexer().GetByKey("ns/old-absent"); err != nil || exists {
		t.Fatalf("complete WatchList replacement retained omitted old key: exists=%v err=%v", exists, err)
	}
	if got := waitForInformerObject(t, informer, "ns/canonical"); got != canonical {
		t.Fatalf("same UID/resourceVersion reload stored %p, want canonical %p", got, canonical)
	}
	if canonicalBaseCalls.Load() != canonicalCallsAfterFailedReload {
		t.Fatalf("canonical base transform calls changed from %d to %d for unchanged reload", canonicalCallsAfterFailedReload, canonicalBaseCalls.Load())
	}

	changed := newSandbox("8", "changed")
	send(activeWatch, watch.Event{Type: watch.Modified, Object: changed})
	got := waitForInformerObjectMatch(t, informer, "ns/canonical", func(obj runtime.Object) bool {
		accessor, err := meta.Accessor(obj)
		return err == nil && accessor.GetResourceVersion() == "8"
	}).(*agentsv1alpha1.Sandbox)
	if got == canonical || got.ResourceVersion != "8" || !got.Spec.Paused || len(got.ManagedFields) != 0 {
		t.Fatalf("changed object was not transformed and stored: %#v", got)
	}
	if canonicalBaseCalls.Load() != canonicalCallsAfterFailedReload+1 {
		t.Fatalf("canonical base transform called %d times after change, want %d", canonicalBaseCalls.Load(), canonicalCallsAfterFailedReload+1)
	}

	added := newSandbox("9", "new-object")
	added.Name = "added"
	send(activeWatch, watch.Event{Type: watch.Added, Object: added})
	if got := waitForInformerObject(t, informer, "ns/added").(*agentsv1alpha1.Sandbox); len(got.ManagedFields) != 0 {
		t.Fatal("new object did not pass through the base transform")
	}

	changedUID := newSandbox("8", "changed")
	changedUID.UID = types.UID("uid-2")
	send(activeWatch, watch.Event{Type: watch.Modified, Object: changedUID})
	got = waitForInformerObjectMatch(t, informer, "ns/canonical", func(obj runtime.Object) bool {
		return obj.(*agentsv1alpha1.Sandbox).UID == "uid-2"
	}).(*agentsv1alpha1.Sandbox)
	if len(got.ManagedFields) != 0 {
		t.Fatalf("changed UID did not keep and transform the new object: uid=%q managedFields=%v", got.UID, got.ManagedFields)
	}

	deleted := newSandbox("8", "changed")
	deleted.UID = types.UID("uid-2")
	send(activeWatch, watch.Event{Type: watch.Deleted, Object: deleted})
	waitForInformerObjectGone(t, informer, "ns/canonical")
	close(third)
}

func waitForInformerObject(t *testing.T, informer toolscache.SharedIndexInformer, key string) runtime.Object {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if obj, exists, err := informer.GetIndexer().GetByKey(key); err == nil && exists {
			return obj.(runtime.Object)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("informer object %q did not appear", key)
	return nil
}

func waitForInformerObjectMatch(t *testing.T, informer toolscache.SharedIndexInformer, key string, match func(runtime.Object) bool) runtime.Object {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if obj, exists, err := informer.GetIndexer().GetByKey(key); err == nil && exists {
			if candidate, ok := obj.(runtime.Object); ok && match(candidate) {
				return candidate
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("informer object %q did not satisfy the condition", key)
	return nil
}

func waitForInformerObjectGone(t *testing.T, informer toolscache.SharedIndexInformer, key string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, exists, err := informer.GetIndexer().GetByKey(key); err == nil && !exists {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("informer object %q did not disappear", key)
}

func TestControllerManagerCacheOptionsStripManagedFieldsPerNamespace(t *testing.T) {
	clientfeaturestesting.SetFeatureDuringTest(t, clientfeatures.WatchListClient, false)
	gv := agentsv1alpha1.GroupVersion
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{gv})
	mapper.AddSpecific(gv.WithKind("Sandbox"), gv.WithResource("sandboxes"), gv.WithResource("sandbox"), meta.RESTScopeNamespace)
	mapper.AddSpecific(gv.WithKind("Checkpoint"), gv.WithResource("checkpoints"), gv.WithResource("checkpoint"), meta.RESTScopeNamespace)

	objectFor := func(kind, namespace string) runtime.Object {
		metadata := metav1.ObjectMeta{
			Namespace:       namespace,
			Name:            "shared-name",
			UID:             types.UID("shared-uid"),
			ResourceVersion: "7",
			ManagedFields:   []metav1.ManagedFieldsEntry{{Manager: "test"}},
			Labels:          map[string]string{"payload": namespace},
		}
		switch kind {
		case "Sandbox":
			return &agentsv1alpha1.Sandbox{
				TypeMeta:   metav1.TypeMeta{APIVersion: gv.String(), Kind: kind},
				ObjectMeta: metadata,
				Spec:       agentsv1alpha1.SandboxSpec{Paused: namespace == "ns-a"},
				Status:     agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxRunning, Message: namespace},
			}
		default:
			return &agentsv1alpha1.Checkpoint{
				TypeMeta:   metav1.TypeMeta{APIVersion: gv.String(), Kind: kind},
				ObjectMeta: metadata,
				Spec:       agentsv1alpha1.CheckpointSpec{SandboxName: &namespace},
				Status:     agentsv1alpha1.CheckpointStatus{Phase: agentsv1alpha1.CheckpointSucceeded, Message: namespace},
			}
		}
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) < 6 || parts[0] != "apis" || parts[1] != gv.Group || parts[2] != gv.Version || parts[3] != "namespaces" {
			http.NotFound(w, r)
			return
		}
		namespace := parts[4]
		kind, listKind, resource := "Sandbox", "SandboxList", "sandboxes"
		if parts[5] == "checkpoints" {
			kind, listKind, resource = "Checkpoint", "CheckpointList", "checkpoints"
		}
		if parts[5] != resource {
			http.NotFound(w, r)
			return
		}
		obj := objectFor(kind, namespace)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("watch") == "true" {
			w.WriteHeader(http.StatusOK)
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusOK)
		list := map[string]any{
			"apiVersion": gv.String(), "kind": listKind,
			"metadata": map[string]any{"resourceVersion": "7"},
			"items":    []runtime.Object{obj},
		}
		if err := json.NewEncoder(w).Encode(list); err != nil {
			t.Errorf("encode %s list: %v", kind, err)
		}
	}))
	t.Cleanup(server.Close)

	scheme := runtime.NewScheme()
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	namespaces := map[string]ctrlcache.Config{"ns-a": {}, "ns-b": {}}
	byObject := map[ctrlclient.Object]ctrlcache.ByObject{
		&agentsv1alpha1.Sandbox{}:    {Namespaces: namespaces},
		&agentsv1alpha1.Checkpoint{}: {Namespaces: namespaces},
	}
	options := controllerManagerCacheOptions(byObject, nil)
	options.Scheme = scheme
	options.Mapper = mapper
	options.DefaultEnableWatchBookmarks = ptr.To(true)
	nativeFactory := options.NewInformer
	var informerMu sync.Mutex
	var createdInformers []toolscache.SharedIndexInformer
	options.NewInformer = func(lw toolscache.ListerWatcher, obj runtime.Object, resync time.Duration, indexers toolscache.Indexers) toolscache.SharedIndexInformer {
		informer := nativeFactory(lw, obj, resync, indexers)
		informerMu.Lock()
		createdInformers = append(createdInformers, informer)
		informerMu.Unlock()
		return informer
	}

	cache, err := ctrlcache.New(&rest.Config{Host: server.URL}, options)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	startDone := make(chan error, 1)
	go func() { startDone <- cache.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-startDone; err != nil {
			t.Errorf("controller-runtime cache stopped with error: %v", err)
		}
	})
	if !cache.WaitForCacheSync(ctx) {
		t.Fatal("controller-runtime cache did not sync")
	}
	for _, kind := range []string{"Sandbox", "Checkpoint"} {
		for _, namespace := range []string{"ns-a", "ns-b"} {
			t.Run(kind+"/"+namespace, func(t *testing.T) {
				key := ctrlclient.ObjectKey{Namespace: namespace, Name: "shared-name"}
				var got ctrlclient.Object
				switch kind {
				case "Sandbox":
					got = &agentsv1alpha1.Sandbox{}
				case "Checkpoint":
					got = &agentsv1alpha1.Checkpoint{}
				}
				if err := cache.Get(ctx, key, got); err != nil {
					t.Fatal(err)
				}
				if len(got.GetManagedFields()) != 0 {
					t.Fatalf("managedFields were not stripped from %s: %#v", kind, got.GetManagedFields())
				}
				if got.GetLabels()["payload"] != namespace {
					t.Fatalf("namespace payload = %q, want %q", got.GetLabels()["payload"], namespace)
				}
				if got.GetUID() != types.UID("shared-uid") || got.GetResourceVersion() != "7" {
					t.Fatalf("identity metadata changed: uid=%q rv=%q", got.GetUID(), got.GetResourceVersion())
				}
				switch object := got.(type) {
				case *agentsv1alpha1.Sandbox:
					if object.Spec.Paused != (namespace == "ns-a") {
						t.Fatalf("sandbox spec.paused=%v for %s", object.Spec.Paused, namespace)
					}
					if object.Status.Phase != agentsv1alpha1.SandboxRunning || object.Status.Message != namespace {
						t.Fatalf("sandbox status changed: phase=%q message=%q", object.Status.Phase, object.Status.Message)
					}
				case *agentsv1alpha1.Checkpoint:
					if object.Spec.SandboxName == nil || *object.Spec.SandboxName != namespace {
						t.Fatalf("checkpoint spec.sandboxName=%v for %s", object.Spec.SandboxName, namespace)
					}
					if object.Status.Phase != agentsv1alpha1.CheckpointSucceeded || object.Status.Message != namespace {
						t.Fatalf("checkpoint status changed: phase=%q message=%q", object.Status.Phase, object.Status.Message)
					}
				}
			})
		}
	}
	informerMu.Lock()
	created := append([]toolscache.SharedIndexInformer(nil), createdInformers...)
	informerMu.Unlock()
	if len(created) != 4 {
		t.Fatalf("NewInformer called %d times, want one informer per resource and namespace (4)", len(created))
	}
	for i, informer := range created {
		if _, ok := informer.(*memoryEfficientInformer); !ok {
			t.Fatalf("NewInformer result %d has type %T, want *memoryEfficientInformer", i, informer)
		}
		for j := 0; j < i; j++ {
			if informer.GetIndexer() == created[j].GetIndexer() {
				t.Fatalf("namespace informer stores %d and %d share an indexer", i, j)
			}
		}
	}
}

func TestControllerManagerCacheOptionsWiresWatchErrorHealth(t *testing.T) {
	if got := controllerManagerCacheOptions(nil, nil).DefaultWatchErrorHandler; got != nil {
		t.Fatalf("nil health installed DefaultWatchErrorHandler %p, want nil", got)
	}
	if got := controllerManagerCacheOptions(nil, NewInformerHealth()).DefaultWatchErrorHandler; got == nil {
		t.Fatal("controllerManagerCacheOptions must install DefaultWatchErrorHandler when health is provided")
	}
}
