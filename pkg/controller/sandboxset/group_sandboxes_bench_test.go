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

package sandboxset

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils/fieldindex"
)

func init() {
	// Without an API server the reflector cannot receive the bookmark that the
	// streaming list waits for, so fall back to a plain list for this benchmark.
	_ = os.Setenv("KUBE_FEATURE_WatchListClient", "false")
}

// staticListerWatcher serves a fixed set of Sandboxes to an informer.
type staticListerWatcher struct{ items []agentsv1alpha1.Sandbox }

func (s staticListerWatcher) List(metav1.ListOptions) (runtime.Object, error) {
	return &agentsv1alpha1.SandboxList{ListMeta: metav1.ListMeta{ResourceVersion: "1"}, Items: s.items}, nil
}

func (s staticListerWatcher) Watch(metav1.ListOptions) (watch.Interface, error) {
	return watch.NewFake(), nil
}

// BenchmarkGroupAllSandboxes runs the real groupAllSandboxes against a
// controller-runtime informer cache (with the ownerRef index) and a production
// style zap logger, so the cost of the per-sandbox logging is visible.
func BenchmarkGroupAllSandboxes(b *testing.B) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(agentsv1alpha1.GroupVersion.WithKind("Sandbox"), meta.RESTScopeNamespace)
	cfg := &rest.Config{Host: "http://127.0.0.1:1"}
	sbs := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default", UID: types.UID("sbs-uid")},
	}

	for _, n := range []int{100, 1000, 5000} {
		items := make([]agentsv1alpha1.Sandbox, n)
		for i := range items {
			items[i] = agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:            fmt.Sprintf("pool-%d", i),
					Namespace:       "default",
					ResourceVersion: "1",
					Labels:          map[string]string{agentsv1alpha1.LabelSandboxPool: "pool"},
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: agentsv1alpha1.SandboxSetControllerKind.GroupVersion().String(),
						Kind:       agentsv1alpha1.SandboxSetControllerKind.Kind,
						Name:       sbs.Name,
						UID:        sbs.UID,
						Controller: ptr.To(true),
					}},
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue},
					},
				},
			}
		}
		informerCache, err := ctrlcache.New(cfg, ctrlcache.Options{
			Scheme:     scheme,
			Mapper:     mapper,
			HTTPClient: http.DefaultClient,
			NewInformer: func(_ toolscache.ListerWatcher, obj runtime.Object, resync time.Duration,
				indexers toolscache.Indexers) toolscache.SharedIndexInformer {
				return toolscache.NewSharedIndexInformer(staticListerWatcher{items: items}, obj, resync, indexers)
			},
		})
		if err != nil {
			b.Fatal(err)
		}
		if err := informerCache.IndexField(context.Background(), &agentsv1alpha1.Sandbox{},
			fieldindex.IndexNameForOwnerRefUID, fieldindex.OwnerIndexFunc); err != nil {
			b.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		if _, err := informerCache.GetInformer(ctx, &agentsv1alpha1.Sandbox{}); err != nil {
			cancel()
			b.Fatal(err)
		}
		go func() { _ = informerCache.Start(ctx) }()
		if !informerCache.WaitForCacheSync(ctx) {
			cancel()
			b.Fatal("informer cache did not sync")
		}
		cl, err := ctrlclient.New(cfg, ctrlclient.Options{
			Scheme:     scheme,
			Mapper:     mapper,
			HTTPClient: http.DefaultClient,
			Cache:      &ctrlclient.CacheOptions{Reader: informerCache},
		})
		if err != nil {
			cancel()
			b.Fatal(err)
		}
		r := &Reconciler{Client: cl}
		logger := zap.New(zap.WriteTo(io.Discard)).WithValues("sandboxset", "default/pool")
		lctx := logf.IntoContext(ctx, logger)

		b.Run(fmt.Sprintf("sandboxes=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				groups, err := r.groupAllSandboxes(lctx, sbs)
				if err != nil || len(groups.Available) != n {
					b.Fatalf("unexpected result: err=%v available=%d", err, len(groups.Available))
				}
			}
		})
		cancel()
	}
}
