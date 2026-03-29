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

package inplaceupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	agentsapiv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// PodAnnotationInPlaceUpdateStateKey records the state of inplace-update.
	// The value of annotation is InPlaceUpdateState.
	PodAnnotationInPlaceUpdateStateKey string = "agents.kruise.io/inplace-update-state"
)

// InPlaceUpdateState records latest inplace-update state, including old statuses of containers.
type InPlaceUpdateState struct {
	// Revision is the updated revision hash.
	Revision string `json:"revision"`

	// UpdateTimestamp is the start time when the in-place update happens.
	UpdateTimestamp metav1.Time `json:"updateTimestamp"`

	// LastContainerStatuses records the before-in-place-update container statuses. It is a map from ContainerName
	// to InPlaceUpdateContainerStatus
	LastContainerStatuses map[string]InPlaceUpdateContainerStatus `json:"lastContainerStatuses"`

	// UpdateImages indicates there are images that should be in-place update.
	UpdateImages bool `json:"updateImages,omitempty"`

	// UpdateResources indicates there are resources that should be in-place update.
	UpdateResources bool `json:"updateResources,omitempty"`
}

// InPlaceUpdateContainerStatus records the statuses of the container that are mainly used
// to determine whether the InPlaceUpdate is completed.
type InPlaceUpdateContainerStatus struct {
	ImageID string `json:"imageID,omitempty"`
}

func GetPodInPlaceUpdateState(pod *corev1.Pod) (*InPlaceUpdateState, error) {
	logger := logf.FromContext(context.TODO()).WithValues("pod", klog.KObj(pod))

	if stateStr, ok := pod.Annotations[PodAnnotationInPlaceUpdateStateKey]; ok && stateStr != "" {
		state := &InPlaceUpdateState{}
		if err := json.Unmarshal([]byte(stateStr), state); err != nil {
			logger.Error(err, "Unmarshal pod annotation failed", "annotation", PodAnnotationInPlaceUpdateStateKey)
			return nil, err
		}
		return state, nil
	}
	return nil, nil
}

type InPlaceUpdateOptions struct {
	Box      *agentsapiv1alpha1.Sandbox
	Revision string
	Pod      *corev1.Pod
}

type GeneratePatchBodyFunc func(opts InPlaceUpdateOptions) string

type InPlaceUpdateControl struct {
	client.Client
	generatePatchBodyFunc GeneratePatchBodyFunc
}

func NewInPlaceUpdateControl(c client.Client, patchFunc GeneratePatchBodyFunc) *InPlaceUpdateControl {
	control := &InPlaceUpdateControl{
		Client:                c,
		generatePatchBodyFunc: patchFunc,
	}
	return control
}

func (c *InPlaceUpdateControl) generatePatchBody(opts InPlaceUpdateOptions) string {
	if c.generatePatchBodyFunc == nil {
		return DefaultGeneratePatchBodyFunc(opts)
	}
	return c.generatePatchBodyFunc(opts)
}

func (c *InPlaceUpdateControl) generateResizeSubresourceBody(opts InPlaceUpdateOptions) *corev1.Pod {
	return DefaultGenerateResizeSubresourceBody(opts)
}

func DefaultGeneratePatchBodyFunc(opts InPlaceUpdateOptions) string {
	box, pod, revision := opts.Box, opts.Pod, opts.Revision
	state := &InPlaceUpdateState{
		Revision:              revision,
		UpdateTimestamp:       metav1.Now(),
		LastContainerStatuses: map[string]InPlaceUpdateContainerStatus{},
	}
	// container.name -> container
	originContainers := map[string]corev1.Container{}
	for i := range box.Spec.Template.Spec.Containers {
		obj := box.Spec.Template.Spec.Containers[i]
		originContainers[obj.Name] = obj
	}
	// container.name -> imageId
	originStatus := map[string]string{}
	for _, status := range pod.Status.ContainerStatuses {
		originStatus[status.Name] = status.ImageID
	}

	patchSpec := corev1.PodSpec{}
	for i := range pod.Spec.Containers {
		container := pod.Spec.Containers[i]
		origin, ok := originContainers[container.Name]
		if !ok {
			continue
		}
		imageChanged := origin.Image != container.Image
		resourceChanged := !reflect.DeepEqual(origin.Resources, container.Resources)
		if !imageChanged && !resourceChanged {
			continue
		}
		patchContainer := corev1.Container{
			Name: container.Name,
		}
		if imageChanged {
			patchContainer.Image = origin.Image
			patchSpec.Containers = append(patchSpec.Containers, patchContainer)
			state.UpdateImages = true
			imageId := originStatus[container.Name]
			state.LastContainerStatuses[container.Name] = InPlaceUpdateContainerStatus{
				ImageID: imageId,
			}
		}
		if resourceChanged {
			state.UpdateResources = true
		}
	}
	if !state.UpdateImages && !state.UpdateResources {
		return ""
	}

	annotations := map[string]string{
		PodAnnotationInPlaceUpdateStateKey: utils.DumpJson(state),
	}
	labels := map[string]string{
		agentsapiv1alpha1.PodLabelTemplateHash: revision,
	}
	if len(patchSpec.Containers) == 0 {
		return fmt.Sprintf(`{"metadata":{"annotations":%s,"labels":%s}}`, utils.DumpJson(annotations), utils.DumpJson(labels))
	}
	return fmt.Sprintf(`{"metadata":{"annotations":%s,"labels":%s},"spec":%s}`, utils.DumpJson(annotations), utils.DumpJson(labels), utils.DumpJson(patchSpec))
}

// buildContainerResourcesMap builds a map from template containers.
func buildContainerResourcesMap(containers []corev1.Container) map[string]corev1.Container {
	result := make(map[string]corev1.Container, len(containers))
	for _, c := range containers {
		result[c.Name] = c
	}
	return result
}

// DefaultGenerateResizeSubresourceBody generates the Pod body for resize subresource update.
// It compares the current pod's container resources with the sandbox's container resources,
// and returns a minimal Pod object containing only the fields required by the resize API:
//   - metadata.name, metadata.namespace, metadata.resourceVersion
//   - spec.containers[].resources
//   - spec.initContainers[].resources
//
// Returns nil if no resource changes are detected.
func DefaultGenerateResizeSubresourceBody(opts InPlaceUpdateOptions) *corev1.Pod {
	box, pod := opts.Box, opts.Pod
	if box.Spec.Template == nil {
		return nil
	}

	originContainers := buildContainerResourcesMap(box.Spec.Template.Spec.Containers)
	originInitContainers := buildContainerResourcesMap(box.Spec.Template.Spec.InitContainers)

	resizeBody := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            pod.Name,
			Namespace:       pod.Namespace,
			ResourceVersion: pod.ResourceVersion,
		},
		Spec: corev1.PodSpec{
			Containers:     make([]corev1.Container, len(pod.Spec.Containers)),
			InitContainers: make([]corev1.Container, len(pod.Spec.InitContainers)),
		},
	}
	for i := range pod.Spec.Containers {
		resizeBody.Spec.Containers[i] = corev1.Container{
			Name:      pod.Spec.Containers[i].Name,
			Resources: pod.Spec.Containers[i].Resources,
		}
	}
	for i := range pod.Spec.InitContainers {
		resizeBody.Spec.InitContainers[i] = corev1.Container{
			Name:      pod.Spec.InitContainers[i].Name,
			Resources: pod.Spec.InitContainers[i].Resources,
		}
	}

	changed := false
	for i := range resizeBody.Spec.Containers {
		container := &resizeBody.Spec.Containers[i]
		origin, ok := originContainers[container.Name]
		if !ok {
			continue
		}
		if reflect.DeepEqual(origin.Resources, container.Resources) {
			continue
		}
		container.Resources = origin.Resources
		changed = true
	}
	for i := range resizeBody.Spec.InitContainers {
		container := &resizeBody.Spec.InitContainers[i]
		origin, ok := originInitContainers[container.Name]
		if !ok {
			continue
		}
		if reflect.DeepEqual(origin.Resources, container.Resources) {
			continue
		}
		container.Resources = origin.Resources
		changed = true
	}
	if !changed {
		return nil
	}
	return resizeBody
}

func (c *InPlaceUpdateControl) Update(ctx context.Context, opts InPlaceUpdateOptions) (bool, error) {
	box, pod, revision := opts.Box, opts.Pod, opts.Revision
	logger := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(box))

	patchBody := c.generatePatchBody(opts)
	resizeBody := c.generateResizeSubresourceBody(opts)
	if patchBody == "" && resizeBody == nil {
		return false, nil
	}

	current := pod.DeepCopy()
	if patchBody != "" {
		if err := c.Patch(ctx, current, client.RawPatch(types.StrategicMergePatchType, []byte(patchBody))); err != nil {
			logger.Error(err, "inplace update pod patch failed")
			return false, err
		}
	}
	if resizeBody != nil {
		if err := c.SubResource("resize").Update(ctx, current, client.WithSubResourceBody(resizeBody)); err != nil {
			logger.Error(err, "inplace update pod resize failed")
			return false, err
		}
	}
	logger.Info("inplace update pod success", "revision", revision, "patchBody", patchBody, "hasResizeBody", resizeBody != nil)
	return true, nil
}

func IsInplaceUpdateCompleted(ctx context.Context, pod *corev1.Pod) bool {
	logger := logf.FromContext(ctx).WithValues("pod", klog.KObj(pod))

	state, err := GetPodInPlaceUpdateState(pod)
	if state == nil || err != nil {
		return true
	}
	// container.Name -> imageId
	cStatus := map[string]string{}
	for _, status := range pod.Status.ContainerStatuses {
		cStatus[status.Name] = status.ImageID
	}
	for name, status := range state.LastContainerStatuses {
		if old, ok := cStatus[name]; !ok || old == status.ImageID {
			logger.Info("pod container inplace update is incompleted", "container", name, "old imageId", old, "cur imageId", status.ImageID)
			return false
		}
	}
	if state.UpdateResources {
		if isPodResourceResizeApplied(pod) {
			return true
		}
		logger.Info("pod resize resources are not applied yet")
		return false
	}
	return true
}

func isPodResourceResizeApplied(pod *corev1.Pod) bool {
	if len(pod.Spec.Containers) == 0 {
		return false
	}
	statusMap := make(map[string]*corev1.ContainerStatus, len(pod.Status.ContainerStatuses))
	for i := range pod.Status.ContainerStatuses {
		statusMap[pod.Status.ContainerStatuses[i].Name] = &pod.Status.ContainerStatuses[i]
	}
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		status, ok := statusMap[c.Name]
		if !ok || status.Resources == nil {
			return false
		}
		if !isResourceListCovered(status.Resources.Requests, c.Resources.Requests) {
			return false
		}
		if !isResourceListCovered(status.Resources.Limits, c.Resources.Limits) {
			return false
		}
	}
	return true
}

func isResourceListCovered(actual, expected corev1.ResourceList) bool {
	for name, expectedQ := range expected {
		actualQ, ok := actual[name]
		if !ok || actualQ.Cmp(expectedQ) != 0 {
			return false
		}
	}
	return true
}
