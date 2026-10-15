/*
Copyright 2025 The Kruise Authors.

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

package discovery

import (
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	clientdiscovery "k8s.io/client-go/discovery"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/client"
)

var (
	internalScheme = runtime.NewScheme()

	errKindNotFound = fmt.Errorf("kind not found in group version resources")
	backOff         = wait.Backoff{
		Steps:    4,
		Duration: 500 * time.Millisecond,
		Factor:   5.0,
		Jitter:   0.1,
	}
)

func init() {
	utilruntime.Must(v1alpha1.AddToScheme(internalScheme))
}

func DiscoverGVK(gvk schema.GroupVersionKind) bool {
	genericClient := client.GetGenericClient()
	if genericClient == nil {
		return false
	}
	return discoverGVKWithClient(genericClient.DiscoveryClient, gvk)
}

func discoverGVKWithClient(discoveryClient clientdiscovery.DiscoveryInterface, gvk schema.GroupVersionKind) bool {
	startTime := time.Now()
	// errKindNotFound means discovery answered and the kind is absent, which no
	// amount of retrying can change. Retry only the errors that reaching the API
	// server can still resolve, so an uninstalled optional CRD is reported at
	// once instead of after the whole backoff.
	err := retry.OnError(backOff, func(err error) bool { return !errors.Is(err, errKindNotFound) }, func() error {
		resourceList, err := discoveryClient.ServerResourcesForGroupVersion(gvk.GroupVersion().String())
		if err != nil {
			return err
		}
		for _, r := range resourceList.APIResources {
			if r.Kind == gvk.Kind {
				return nil
			}
		}
		return errKindNotFound
	})

	if err != nil {
		if errors.Is(err, errKindNotFound) {
			klog.InfoS("Not found kind in group version", "kind", gvk.Kind, "groupVersion", gvk.GroupVersion().String(), "cost", time.Since(startTime))
			return false
		}

		// This might be caused by abnormal apiserver or etcd, ignore it
		klog.ErrorS(err, "Failed to find resources in group version", "groupVersion", gvk.GroupVersion().String(), "cost", time.Since(startTime))
	}

	return true
}
