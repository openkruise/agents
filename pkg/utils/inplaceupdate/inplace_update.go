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
	"sync/atomic"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsapiv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

// ResizeNotSupportedError indicates that in-place pod resource resize is not
// possible on the current cluster. This is returned when both the pods/resize
// subresource (K8s 1.33+) and the direct spec patch fallback (K8s 1.27-1.32)
// fail, which typically means the InPlacePodVerticalScaling feature gate is
// not enabled.
type ResizeNotSupportedError struct {
	Err error
}

func (e *ResizeNotSupportedError) Error() string {
	return fmt.Sprintf("in-place pod resize not supported: %v", e.Err)
}

func (e *ResizeNotSupportedError) Unwrap() error {
	return e.Err
}

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
	// OnProgress is called after each successful in-place sub-operation
	// (e.g. metadata/image patch, resource resize).
	OnProgress func()
	// for future extensions of pod update behavior
	ExtensionAnnotations map[string]string
}

type GeneratePatchBodyFunc func(opts InPlaceUpdateOptions) string

type InPlaceUpdateControl struct {
	client.Client
	generatePatchBodyFunc GeneratePatchBodyFunc
	// useDirectResourcePatch is set to true after the first 404 from the pods/resize
	// subresource, indicating the cluster is K8s < 1.33 and all subsequent
	// resource resize calls should go directly through the spec patch path.
	useDirectResourcePatch atomic.Bool
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
	box, pod, revision, extensionAnnotations := opts.Box, opts.Pod, opts.Revision, opts.ExtensionAnnotations
	state := &InPlaceUpdateState{
		Revision:              revision,
		UpdateTimestamp:       metav1.Now(),
		LastContainerStatuses: map[string]InPlaceUpdateContainerStatus{},
	}
	labelsPatch := map[string]string{}
	if pod.Labels[agentsapiv1alpha1.PodLabelTemplateHash] != revision {
		labelsPatch[agentsapiv1alpha1.PodLabelTemplateHash] = revision
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

	if box.Spec.Template != nil {
		for k, v := range box.Spec.Template.Labels {
			if pod.Labels[k] != v {
				labelsPatch[k] = v
			}
		}
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
	annotationsPatch := map[string]string{}
	if state.UpdateImages || state.UpdateResources {
		annotationsPatch[PodAnnotationInPlaceUpdateStateKey] = utils.DumpJson(state)
	}
	for k, v := range extensionAnnotations {
		annotationsPatch[k] = v
	}

	if len(labelsPatch) == 0 && len(annotationsPatch) == 0 && len(patchSpec.Containers) == 0 {
		return ""
	}
	metadataPatch := map[string]any{}
	if len(labelsPatch) > 0 {
		metadataPatch["labels"] = labelsPatch
	}
	if len(annotationsPatch) > 0 {
		metadataPatch["annotations"] = annotationsPatch
	}
	patch := map[string]any{
		"metadata": metadataPatch,
	}
	if len(patchSpec.Containers) > 0 {
		patch["spec"] = map[string]any{
			"containers": patchSpec.Containers,
		}
	}
	return utils.DumpJson(patch)
}

// buildContainerResourcesMap builds a map from template containers.
func buildContainerResourcesMap(containers []corev1.Container) map[string]corev1.Container {
	result := make(map[string]corev1.Container, len(containers))
	for _, c := range containers {
		result[c.Name] = c
	}
	return result
}

// DefaultGenerateResizeSubresourceBody generates the Pod body for resize subResource update.
// It compares the current pod's container resources with the sandbox's container resources,
// and returns a minimal Pod object containing only the fields required by the resize API:
//   - metadata.name, metadata.namespace, metadata.resourceVersion
//   - spec.containers[].resources
//
// Only the main containers are processed; init containers are not resized.
// Returns nil if no resource changes are detected.
func DefaultGenerateResizeSubresourceBody(opts InPlaceUpdateOptions) *corev1.Pod {
	box, pod := opts.Box, opts.Pod
	if box.Spec.Template == nil {
		return nil
	}

	originContainers := buildContainerResourcesMap(box.Spec.Template.Spec.Containers)

	resizeBody := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            pod.Name,
			Namespace:       pod.Namespace,
			ResourceVersion: pod.ResourceVersion,
		},
		Spec: corev1.PodSpec{
			Containers: make([]corev1.Container, len(pod.Spec.Containers)),
		},
	}
	for i := range pod.Spec.Containers {
		resizeBody.Spec.Containers[i] = corev1.Container{
			Name:      pod.Spec.Containers[i].Name,
			Resources: pod.Spec.Containers[i].Resources,
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
	if !changed {
		return nil
	}
	return resizeBody
}

// buildResourcePatch converts a resize body (as produced by generateResizeSubresourceBody)
// into a strategic merge patch that can be applied directly to the pod object. This is
// the fallback path for K8s 1.27-1.32, where InPlacePodVerticalScaling is supported but
// the pods/resize subresource does not yet exist.
func buildResourcePatch(resizeBody *corev1.Pod) string {
	type containerPatch struct {
		Name      string                      `json:"name"`
		Resources corev1.ResourceRequirements `json:"resources"`
	}
	var containers []containerPatch
	for _, c := range resizeBody.Spec.Containers {
		containers = append(containers, containerPatch{
			Name:      c.Name,
			Resources: c.Resources,
		})
	}
	patch := map[string]any{
		"spec": map[string]any{
			"containers": containers,
		},
	}
	return utils.DumpJson(patch)
}

// CheckResizeQoSChange checks if applying the sandbox template resources to the pod
// would change the pod's QoS class. Returns the original and new QoS classes plus
// a boolean indicating whether a change would occur.
func CheckResizeQoSChange(box *agentsapiv1alpha1.Sandbox, pod *corev1.Pod) (orig, updated corev1.PodQOSClass, changed bool) {
	if box.Spec.Template == nil {
		return "", "", false
	}
	orig = computeQoSClass(pod)

	afterPod := pod.DeepCopy()
	templateContainers := buildContainerResourcesMap(box.Spec.Template.Spec.Containers)
	for i := range afterPod.Spec.Containers {
		if tmpl, ok := templateContainers[afterPod.Spec.Containers[i].Name]; ok {
			afterPod.Spec.Containers[i].Resources = tmpl.Resources
		}
	}
	updated = computeQoSClass(afterPod)
	return orig, updated, orig != updated
}

var zeroQuantity = resource.MustParse("0")

func isSupportedQoSComputeResource(name corev1.ResourceName) bool {
	return name == corev1.ResourceCPU || name == corev1.ResourceMemory
}

// processResourceList adds non-zero quantities for supported QoS compute
// resources from newList into list.
func processResourceList(list, newList corev1.ResourceList) {
	for name, quantity := range newList {
		if !isSupportedQoSComputeResource(name) {
			continue
		}
		if quantity.Cmp(zeroQuantity) == 1 {
			delta := quantity.DeepCopy()
			if _, exists := list[name]; !exists {
				list[name] = delta
			} else {
				delta.Add(list[name])
				list[name] = delta
			}
		}
	}
}

// getQOSResources returns the set of supported QoS resource names that have
// a quantity greater than zero.
func getQOSResources(list corev1.ResourceList) map[corev1.ResourceName]bool {
	qosResources := make(map[corev1.ResourceName]bool)
	for name, quantity := range list {
		if !isSupportedQoSComputeResource(name) {
			continue
		}
		if quantity.Cmp(zeroQuantity) == 1 {
			qosResources[name] = true
		}
	}
	return qosResources
}

// computeQoSClass determines the QoS class of a Pod following the same
// algorithm as upstream Kubernetes (qos.ComputePodQOS).
func computeQoSClass(pod *corev1.Pod) corev1.PodQOSClass {
	requests := corev1.ResourceList{}
	limits := corev1.ResourceList{}
	isGuaranteed := true

	if pod.Spec.Resources != nil {
		processResourceList(requests, pod.Spec.Resources.Requests)
		processResourceList(limits, pod.Spec.Resources.Limits)
		qosLimitResources := getQOSResources(pod.Spec.Resources.Limits)
		if !qosLimitResources[corev1.ResourceCPU] || !qosLimitResources[corev1.ResourceMemory] {
			isGuaranteed = false
		}
	} else {
		allContainers := make([]corev1.Container, 0, len(pod.Spec.Containers)+len(pod.Spec.InitContainers))
		allContainers = append(allContainers, pod.Spec.Containers...)
		allContainers = append(allContainers, pod.Spec.InitContainers...)

		for _, container := range allContainers {
			processResourceList(requests, container.Resources.Requests)
			qosLimitsFound := getQOSResources(container.Resources.Limits)
			processResourceList(limits, container.Resources.Limits)
			if !qosLimitsFound[corev1.ResourceCPU] || !qosLimitsFound[corev1.ResourceMemory] {
				isGuaranteed = false
			}
		}
	}

	if len(requests) == 0 && len(limits) == 0 {
		return corev1.PodQOSBestEffort
	}
	if isGuaranteed {
		for name, req := range requests {
			if lim, exists := limits[name]; !exists || lim.Cmp(req) != 0 {
				isGuaranteed = false
				break
			}
		}
	}
	if isGuaranteed && len(requests) == len(limits) {
		return corev1.PodQOSGuaranteed
	}
	return corev1.PodQOSBurstable
}

func (c *InPlaceUpdateControl) Update(ctx context.Context, opts InPlaceUpdateOptions) (bool, error) {
	box, pod, revision := opts.Box, opts.Pod, opts.Revision
	logger := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(box))
	progressed := false

	// perform patch
	patchBody := c.generatePatchBody(opts)
	current := pod.DeepCopy()
	if patchBody != "" {
		if err := c.Patch(ctx, current, client.RawPatch(types.StrategicMergePatchType, []byte(patchBody))); err != nil {
			logger.Error(err, "inplace update pod patch failed")
			return false, err
		}
		progressed = true
		if opts.OnProgress != nil {
			opts.OnProgress()
		}
	}

	// perform resize
	resizeBody := c.generateResizeSubresourceBody(InPlaceUpdateOptions{
		Box:      box,
		Revision: revision,
		// Use `current` (the post-patch pod) instead of the original `pod` so that
		// generateResizeSubresourceBody picks up the latest ResourceVersion set by
		// the patch response, avoiding "the object has been modified" conflicts on
		// the subsequent resize sub-resource call.
		Pod: current,
	})
	if resizeBody != nil {
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			err := c.resizePod(ctx, logger, current, resizeBody)
			if !apierrors.IsConflict(err) {
				return err
			}

			latestPod := &corev1.Pod{}
			if getErr := c.Get(ctx, client.ObjectKeyFromObject(current), latestPod); getErr != nil {
				return getErr
			}
			current = latestPod
			resizeBody = c.generateResizeSubresourceBody(InPlaceUpdateOptions{
				Box:      box,
				Revision: revision,
				Pod:      current,
			})
			if resizeBody == nil {
				return nil
			}

			logger.V(5).Info("inplace update pod resize conflict, retrying with latest resourceVersion", "resourceVersion", current.ResourceVersion)
			return err
		}); err != nil {
			return progressed, err
		}
		if opts.OnProgress != nil {
			opts.OnProgress()
		}
	}

	if patchBody == "" && resizeBody == nil {
		return false, nil
	}
	logger.Info("inplace update pod success", "revision", revision, "patchBody", patchBody, "hasResizeBody", resizeBody != nil)
	return true, nil
}

// resizePod applies a resource resize to the pod. It uses the pods/resize subresource
// on K8s >= 1.33 (see https://kubernetes.io/blog/2025/05/16/kubernetes-v1-33-in-place-pod-resize-beta/),
// and falls back to a direct strategic merge patch on older versions (K8s 1.27-1.32).
// The detection result is cached so the subresource is only probed once per controller lifetime.
func (c *InPlaceUpdateControl) resizePod(ctx context.Context, logger klog.Logger, pod *corev1.Pod, resizeBody *corev1.Pod) error {
	if !c.useDirectResourcePatch.Load() {
		err := c.SubResource("resize").Update(ctx, pod, client.WithSubResourceBody(resizeBody))
		if err == nil {
			logger.Info("inplace update pod resize succeeded via resize subresource (K8s >= 1.33)")
			return nil
		}
		if !apierrors.IsNotFound(err) {
			logger.Error(err, "inplace update pod resize failed via resize subresource")
			return err
		}
		// The pods/resize subresource was introduced in K8s 1.33. On K8s 1.27-1.32,
		// in-place pod vertical scaling is done by directly patching spec.containers[].resources.
		logger.Info("resize subresource not found, switching to direct resource patch (K8s < 1.33)")
		c.useDirectResourcePatch.Store(true)
	}
	return c.patchPodResources(ctx, logger, pod, resizeBody)
}

// patchPodResources applies a strategic merge patch to update pod resources directly.
func (c *InPlaceUpdateControl) patchPodResources(ctx context.Context, logger klog.Logger, pod *corev1.Pod, resizeBody *corev1.Pod) error {
	resourcePatch := buildResourcePatch(resizeBody)
	if err := c.Patch(ctx, pod, client.RawPatch(types.StrategicMergePatchType, []byte(resourcePatch))); err != nil {
		if apierrors.IsConflict(err) {
			logger.Error(err, "direct resource patch conflicted")
			return err
		}
		logger.Error(err, "direct resource patch failed, in-place resize not supported")
		return &ResizeNotSupportedError{Err: err}
	}
	logger.Info("inplace update pod resize succeeded via direct resource patch (K8s < 1.33)")
	return nil
}

// IsInplaceUpdateCompleted checks whether an in-place update has finished.
// Returns (true, nil) when all changes are applied.
// Returns (false, nil) when the update is still in progress.
// Returns (false, err) when a terminal failure is detected (e.g., resize infeasible)
// and the caller should fail fast instead of retrying.
func IsInplaceUpdateCompleted(ctx context.Context, pod *corev1.Pod) (bool, error) {
	logger := logf.FromContext(ctx).WithValues("pod", klog.KObj(pod))

	state, err := GetPodInPlaceUpdateState(pod)
	if state == nil || err != nil {
		return true, nil
	}
	if state.UpdateImages {
		if !isPodImageUpdateCompleted(pod, state) {
			logger.Info("pod container image inplace update is not completed yet")
			return false, nil
		}
	}
	if state.UpdateResources {
		if !isPodResourceResizeCompleted(pod) {
			if terminalErr := checkPodResizeInfeasible(pod); terminalErr != nil {
				return false, terminalErr
			}
			logger.Info("pod resize resources are not applied yet")
			return false, nil
		}
	}
	return true, nil
}

// isPodImageUpdateCompleted checks whether image updates have been applied by comparing
// each container's current ImageID against the ImageID recorded before the update.
// Returns true if all containers have a new (different) ImageID.
func isPodImageUpdateCompleted(pod *corev1.Pod, state *InPlaceUpdateState) bool {
	currentImageIDs := make(map[string]string, len(pod.Status.ContainerStatuses))
	for _, status := range pod.Status.ContainerStatuses {
		currentImageIDs[status.Name] = status.ImageID
	}
	for name, lastStatus := range state.LastContainerStatuses {
		if curID, ok := currentImageIDs[name]; !ok || curID == lastStatus.ImageID {
			return false
		}
	}
	return true
}

// isPodResourceResizeCompleted checks whether the in-place resource resize has been
// fully applied by the kubelet. It compares each container's spec resources against
// the actual resources reported in status.containerStatuses[].resources, returning
// true only when all containers' status resources match their spec.
func isPodResourceResizeCompleted(pod *corev1.Pod) bool {
	// container name -> container status
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

// checkPodResizeInfeasible inspects the pod's resize-related conditions and
// the (deprecated) Resize status field to detect terminal resize failures.
// It returns a non-nil error describing the failure when:
//   - PodResizePending condition is True with reason Infeasible or Deferred
//   - PodResizeInProgress condition is True with reason Error
//   - pod.Status.Resize is Infeasible or Deferred (deprecated field, kept for compat)
func checkPodResizeInfeasible(pod *corev1.Pod) error {
	for _, cond := range pod.Status.Conditions {
		if cond.Status != corev1.ConditionTrue {
			continue
		}
		switch cond.Type {
		case corev1.PodResizePending:
			if cond.Reason == corev1.PodReasonInfeasible {
				return fmt.Errorf("pod resize is infeasible: %s", cond.Message)
			}
			if cond.Reason == corev1.PodReasonDeferred {
				// Deferred means the resize fits the node in theory but cannot
				// be granted right now (e.g. other pods are consuming the
				// resources). For sandbox workloads we treat this as a terminal
				// failure so the caller can react quickly (reschedule, alert,
				// etc.) instead of waiting indefinitely for resources to free up.
				return fmt.Errorf("pod resize is deferred: %s", cond.Message)
			}
		case corev1.PodResizeInProgress:
			if cond.Reason == corev1.PodReasonError {
				return fmt.Errorf("pod resize error: %s", cond.Message)
			}
		}
	}
	// Fallback compatibility check for older clusters (K8s 1.27-1.32) that
	// still rely on the deprecated pod.Status.Resize field.
	if pod.Status.Resize == corev1.PodResizeStatusInfeasible {
		return fmt.Errorf("pod resize is infeasible (status.resize)")
	}
	if pod.Status.Resize == corev1.PodResizeStatusDeferred {
		return fmt.Errorf("pod resize is deferred (status.resize)")
	}
	return nil
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
