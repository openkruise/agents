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

package cli

import (
	kruiseappsv1alpha1 "github.com/openkruise/kruise-api/apps/v1alpha1"
	kruiseversioned "github.com/openkruise/kruise-api/client/clientset/versioned"
	kruisescheme "github.com/openkruise/kruise-api/client/clientset/versioned/scheme"
	kruiseappsv1alpha1typed "github.com/openkruise/kruise-api/client/clientset/versioned/typed/apps/v1alpha1"
	appsv1beta1 "github.com/openkruise/kruise-api/client/clientset/versioned/typed/apps/v1beta1"
	policyv1alpha1 "github.com/openkruise/kruise-api/client/clientset/versioned/typed/policy/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/discovery"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/gentype"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/testing"
)

// fakeKruiseClientset implements kruiseversioned.Interface for unit tests.
// It is a minimal fake that only supports ContainerRecreateRequest operations;
// all other resource types return nil from their getters.
type fakeKruiseClientset struct {
	testing.Fake
}

func newFakeKruiseClientset(objects ...runtime.Object) *fakeKruiseClientset {
	o := testing.NewObjectTracker(kruisescheme.Scheme, kruisescheme.Codecs.UniversalDecoder())
	for _, obj := range objects {
		if err := o.Add(obj); err != nil {
			panic(err)
		}
	}

	cs := &fakeKruiseClientset{}
	cs.AddReactor("*", "*", testing.ObjectReaction(o))
	cs.AddWatchReactor("*", func(action testing.Action) (handled bool, ret watch.Interface, err error) {
		var opts metav1.ListOptions
		if watchAction, ok := action.(testing.WatchActionImpl); ok {
			opts = watchAction.ListOptions
		}
		gvr := action.GetResource()
		ns := action.GetNamespace()
		w, err := o.Watch(gvr, ns, opts)
		if err != nil {
			return false, nil, err
		}
		return true, w, nil
	})

	return cs
}

func (c *fakeKruiseClientset) Discovery() discovery.DiscoveryInterface {
	return &fakediscovery.FakeDiscovery{Fake: &c.Fake}
}

func (c *fakeKruiseClientset) AppsV1alpha1() kruiseappsv1alpha1typed.AppsV1alpha1Interface {
	return &fakeAppsV1alpha1{Fake: &c.Fake}
}

func (c *fakeKruiseClientset) AppsV1beta1() appsv1beta1.AppsV1beta1Interface {
	return nil
}

func (c *fakeKruiseClientset) PolicyV1alpha1() policyv1alpha1.PolicyV1alpha1Interface {
	return nil
}

// fakeAppsV1alpha1 implements kruiseappsv1alpha1typed.AppsV1alpha1Interface.
// Only ContainerRecreateRequests is backed by a real fake implementation;
// all other resource getters return nil.
type fakeAppsV1alpha1 struct {
	*testing.Fake
}

func (c *fakeAppsV1alpha1) ContainerRecreateRequests(namespace string) kruiseappsv1alpha1typed.ContainerRecreateRequestInterface {
	return gentype.NewFakeClientWithList[*kruiseappsv1alpha1.ContainerRecreateRequest, *kruiseappsv1alpha1.ContainerRecreateRequestList](
		c.Fake,
		namespace,
		kruiseappsv1alpha1.SchemeGroupVersion.WithResource("containerrecreaterequests"),
		kruiseappsv1alpha1.SchemeGroupVersion.WithKind("ContainerRecreateRequest"),
		func() *kruiseappsv1alpha1.ContainerRecreateRequest {
			return &kruiseappsv1alpha1.ContainerRecreateRequest{}
		},
		func() *kruiseappsv1alpha1.ContainerRecreateRequestList {
			return &kruiseappsv1alpha1.ContainerRecreateRequestList{}
		},
		func(dst, src *kruiseappsv1alpha1.ContainerRecreateRequestList) { dst.ListMeta = src.ListMeta },
		func(list *kruiseappsv1alpha1.ContainerRecreateRequestList) []*kruiseappsv1alpha1.ContainerRecreateRequest {
			return gentype.ToPointerSlice(list.Items)
		},
		func(list *kruiseappsv1alpha1.ContainerRecreateRequestList, items []*kruiseappsv1alpha1.ContainerRecreateRequest) {
			list.Items = gentype.FromPointerSlice(items)
		},
	)
}

// The following methods return nil because they are not used in the restart tests.

func (c *fakeAppsV1alpha1) RESTClient() rest.Interface { return nil }
func (c *fakeAppsV1alpha1) AdvancedCronJobs(string) kruiseappsv1alpha1typed.AdvancedCronJobInterface {
	return nil
}
func (c *fakeAppsV1alpha1) BroadcastJobs(string) kruiseappsv1alpha1typed.BroadcastJobInterface {
	return nil
}
func (c *fakeAppsV1alpha1) CloneSets(string) kruiseappsv1alpha1typed.CloneSetInterface   { return nil }
func (c *fakeAppsV1alpha1) DaemonSets(string) kruiseappsv1alpha1typed.DaemonSetInterface { return nil }
func (c *fakeAppsV1alpha1) EphemeralJobs(string) kruiseappsv1alpha1typed.EphemeralJobInterface {
	return nil
}
func (c *fakeAppsV1alpha1) ImageListPullJobs(string) kruiseappsv1alpha1typed.ImageListPullJobInterface {
	return nil
}
func (c *fakeAppsV1alpha1) ImagePullJobs(string) kruiseappsv1alpha1typed.ImagePullJobInterface {
	return nil
}
func (c *fakeAppsV1alpha1) NodeImages() kruiseappsv1alpha1typed.NodeImageInterface       { return nil }
func (c *fakeAppsV1alpha1) NodePodProbes() kruiseappsv1alpha1typed.NodePodProbeInterface { return nil }
func (c *fakeAppsV1alpha1) PersistentPodStates(string) kruiseappsv1alpha1typed.PersistentPodStateInterface {
	return nil
}
func (c *fakeAppsV1alpha1) PodProbeMarkers(string) kruiseappsv1alpha1typed.PodProbeMarkerInterface {
	return nil
}
func (c *fakeAppsV1alpha1) ResourceDistributions() kruiseappsv1alpha1typed.ResourceDistributionInterface {
	return nil
}
func (c *fakeAppsV1alpha1) SidecarSets() kruiseappsv1alpha1typed.SidecarSetInterface { return nil }
func (c *fakeAppsV1alpha1) StatefulSets(string) kruiseappsv1alpha1typed.StatefulSetInterface {
	return nil
}
func (c *fakeAppsV1alpha1) UnitedDeployments(string) kruiseappsv1alpha1typed.UnitedDeploymentInterface {
	return nil
}
func (c *fakeAppsV1alpha1) WorkloadSpreads(string) kruiseappsv1alpha1typed.WorkloadSpreadInterface {
	return nil
}

var (
	_ kruiseversioned.Interface = &fakeKruiseClientset{}
)
