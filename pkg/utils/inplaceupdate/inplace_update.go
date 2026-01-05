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

type InPlaceUpdateControl struct {
	client.Client
}

func (c *InPlaceUpdateControl) Update(ctx context.Context, opts InPlaceUpdateOptions) error {
	box, pod, revision := opts.Box, opts.Pod, opts.Revision
	logger := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(box))

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
			return fmt.Errorf("container %s not found in the sandbox", container.Name)
		}
		if origin.Image == container.Image {
			continue
		}
		patchContainer := corev1.Container{
			Name:  container.Name,
			Image: origin.Image,
		}
		patchSpec.Containers = append(patchSpec.Containers, patchContainer)
		state.UpdateImages = true
		imageId := originStatus[container.Name]
		state.LastContainerStatuses[container.Name] = InPlaceUpdateContainerStatus{
			ImageID: imageId,
		}
	}
	if len(patchSpec.Containers) == 0 {
		logger.Info("Pod container images has not been modified")
		return nil
	}

	annotations := map[string]string{
		PodAnnotationInPlaceUpdateStateKey: utils.DumpJson(state),
	}
	labels := map[string]string{
		agentsapiv1alpha1.PodLabelTemplateHash: revision,
	}
	clone := pod.DeepCopy()
	patchBody := fmt.Sprintf(`{"metadata":{"annotations":%s,"labels":%s},"spec":%s}`, utils.DumpJson(annotations), utils.DumpJson(labels), utils.DumpJson(patchSpec))
	if err := c.Patch(ctx, clone, client.RawPatch(types.StrategicMergePatchType, []byte(patchBody))); err != nil {
		logger.Error(err, "inplace update pod failed")
		return err
	}
	logger.Info("inplace update pod success", "revision", revision, "patchBody", patchBody)
	return nil
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
	return true
}
