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
	"sync/atomic"

	"github.com/distribution/reference"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
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

// ImagePullFailedError indicates the target image is definitively invalid or forbidden to pull;
// backoff and transient pull failures should keep waiting instead.
type ImagePullFailedError struct {
	ContainerName string
	Reason        string
	Message       string
}

func (e *ImagePullFailedError) Error() string {
	msg := fmt.Sprintf("container %s cannot pull its image: %s", e.ContainerName, e.Reason)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

// ResizeInfeasibleError preserves the structured fact that resources cannot be realized,
// so callers can classify it without parsing Message.
type ResizeInfeasibleError struct{ Message string }

func (e *ResizeInfeasibleError) Error() string { return e.Message }

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
	// New records track the target reference directly so a rollback to the same ImageID is
	// supported; old records remain compatible with the baseline comparison.
	TargetImage string `json:"targetImage,omitempty"`
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

// UpdateMode 在单次调用中选择执行合同，零值保持既有调用方行为。
type UpdateMode uint8

const (
	CompatibilityMode UpdateMode = iota
	TargetConvergenceMode
)

func (mode UpdateMode) resourcesSatisfied(desired, actual corev1.ResourceRequirements) bool {
	if mode == TargetConvergenceMode {
		return ResourcesExactlyEqual(desired, actual)
	}
	return IsResourceSatisfied(desired, actual)
}

type InPlaceUpdateOptions struct {
	Mode     UpdateMode
	Box      *agentsv1alpha1.Sandbox
	Revision string
	Pod      *corev1.Pod
	// for future extensions of pod update behavior
	ExtensionAnnotations map[string]string
	// ResourceUpdateRequired means resource realization still needs to be observed; it does not
	// mean this call must write resources again.
	ResourceUpdateRequired bool
}

type GeneratePatchBodyFunc func(opts InPlaceUpdateOptions) (string, error)

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

func (c *InPlaceUpdateControl) generatePatchBody(opts InPlaceUpdateOptions) (string, error) {
	if c.generatePatchBodyFunc == nil {
		return DefaultGeneratePatchBodyFunc(opts)
	}
	return c.generatePatchBodyFunc(opts)
}

func (c *InPlaceUpdateControl) buildResizeContainers(opts InPlaceUpdateOptions) []corev1.Container {
	return DefaultBuildResizeContainers(opts)
}

func DefaultGeneratePatchBodyFunc(opts InPlaceUpdateOptions) (string, error) {
	box, pod, revision, extensionAnnotations := opts.Box, opts.Pod, opts.Revision, opts.ExtensionAnnotations
	state := &InPlaceUpdateState{
		Revision:              revision,
		UpdateTimestamp:       metav1.Now(),
		LastContainerStatuses: map[string]InPlaceUpdateContainerStatus{},
		UpdateResources:       opts.ResourceUpdateRequired,
	}
	if opts.Mode == TargetConvergenceMode {
		previous, err := GetPodInPlaceUpdateState(pod)
		if err != nil {
			return "", fmt.Errorf("cannot generate patch from invalid in-place state: %w", err)
		}
		// 目标收敛模式保留未完成跟踪，metadata 收尾不能清掉资源和镜像观察条件。
		if previous != nil {
			state.UpdateResources = state.UpdateResources || previous.UpdateResources
			for name, status := range previous.LastContainerStatuses {
				state.LastContainerStatuses[name] = status
			}
			state.UpdateImages = previous.UpdateImages
		}
	}
	labelsPatch := map[string]string{}
	if pod.Labels[agentsv1alpha1.PodLabelTemplateHash] != revision {
		labelsPatch[agentsv1alpha1.PodLabelTemplateHash] = revision
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
		if origin.Image == container.Image {
			continue
		}
		patchContainer := corev1.Container{Name: container.Name}
		patchContainer.Image = origin.Image
		patchSpec.Containers = append(patchSpec.Containers, patchContainer)
		state.UpdateImages = true
		imageId := originStatus[container.Name]
		lastStatus := InPlaceUpdateContainerStatus{ImageID: imageId}
		if opts.Mode == TargetConvergenceMode {
			lastStatus.TargetImage = origin.Image
		}
		state.LastContainerStatuses[container.Name] = lastStatus
	}
	annotationsPatch := map[string]string{}
	if state.UpdateImages || state.UpdateResources {
		annotationsPatch[PodAnnotationInPlaceUpdateStateKey] = utils.DumpJson(state)
	}
	for k, v := range extensionAnnotations {
		annotationsPatch[k] = v
	}

	// Propagate template annotations to the pod, mirroring the label
	// propagation above. Only annotations whose values differ from the pod's
	// current ones are patched. This is an additive strategic merge that never
	// removes system-injected annotations (e.g., CNI status, in-place state).
	if box.Spec.Template != nil {
		for k, v := range box.Spec.Template.Annotations {
			if pod.Annotations[k] != v {
				annotationsPatch[k] = v
			}
		}
	}

	if len(labelsPatch) == 0 && len(annotationsPatch) == 0 && len(patchSpec.Containers) == 0 {
		return "", nil
	}
	metadataPatch := map[string]any{}
	if opts.Mode == TargetConvergenceMode {
		metadataPatch["resourceVersion"] = pod.ResourceVersion
	}
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
	return utils.DumpJson(patch), nil
}

// buildContainerResourcesMap builds a map from template containers.
func buildContainerResourcesMap(containers []corev1.Container) map[string]corev1.Container {
	result := make(map[string]corev1.Container, len(containers))
	for _, c := range containers {
		result[c.Name] = c
	}
	return result
}

// DefaultBuildResizeContainers generates the desired container resource changes for resize.
// It compares the current pod's container resources with the sandbox's container resources,
// and returns containers containing only the fields required by the resize patch:
//   - containers[].name, containers[].resources
//
// Only regular containers are processed; init containers are not resized.
// Returns nil if no resource changes are detected.
func DefaultBuildResizeContainers(opts InPlaceUpdateOptions) []corev1.Container {
	box, pod := opts.Box, opts.Pod
	if box.Spec.Template == nil {
		return nil
	}

	originContainers := buildContainerResourcesMap(box.Spec.Template.Spec.Containers)
	resizeContainers := make([]corev1.Container, 0, len(pod.Spec.Containers))
	changed := false
	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		origin, ok := originContainers[container.Name]
		if !ok {
			continue
		}
		if opts.Mode.resourcesSatisfied(origin.Resources, container.Resources) {
			continue
		}
		resizeContainers = append(resizeContainers, corev1.Container{
			Name:      container.Name,
			Resources: origin.Resources,
		})
		changed = true
	}
	if !changed {
		return nil
	}
	return resizeContainers
}

// Resource writes carry only the target resources, not the other required fields of Container.
func resourceContainersPatch(resizeContainers []corev1.Container) []map[string]any {
	containers := make([]map[string]any, 0, len(resizeContainers))
	for _, c := range resizeContainers {
		// Encode only the resource fields, to avoid the required image field of Container leaking
		// into the resize request.
		containers = append(containers, map[string]any{"name": c.Name, "resources": c.Resources})
	}
	return containers
}

func buildResourcePatch(resizeContainers []corev1.Container) string {
	return utils.DumpJson(map[string]any{"spec": map[string]any{"containers": resourceContainersPatch(resizeContainers)}})
}

// CheckResizeQoSChange checks if applying the sandbox template resources to the pod
// would change the pod's QoS class. Returns the original and new QoS classes plus
// a boolean indicating whether a change would occur.
func CheckResizeQoSChange(box *agentsv1alpha1.Sandbox, pod *corev1.Pod) (orig, updated corev1.PodQOSClass, changed bool) {
	return CompatibilityMode.CheckResizeQoSChange(box, pod)
}

// CheckResizeQoSChange 的目标模式按实际资源 patch 的合并规则推演，默认模式保留原算法。
func (mode UpdateMode) CheckResizeQoSChange(box *agentsv1alpha1.Sandbox, pod *corev1.Pod) (orig, updated corev1.PodQOSClass, changed bool) {
	if box.Spec.Template == nil {
		return "", "", false
	}
	orig = computeQoSClass(pod)

	afterPod := pod.DeepCopy()
	changes := buildContainerResourcesMap(box.Spec.Template.Spec.Containers)
	if mode == TargetConvergenceMode {
		changes = buildContainerResourcesMap(DefaultBuildResizeContainers(InPlaceUpdateOptions{Box: box, Pod: pod, Mode: mode}))
	}
	for i := range afterPod.Spec.Containers {
		if change, ok := changes[afterPod.Spec.Containers[i].Name]; ok {
			if mode != TargetConvergenceMode {
				afterPod.Spec.Containers[i].Resources = change.Resources
				continue
			}
			// Match the merge semantics of the resource patch, preserving undeclared resources
			// injected by LimitRange and the like.
			resources := &afterPod.Spec.Containers[i].Resources
			if resources.Requests == nil {
				resources.Requests = corev1.ResourceList{}
			}
			if resources.Limits == nil {
				resources.Limits = corev1.ResourceList{}
			}
			for name, value := range change.Resources.Requests {
				resources.Requests[name] = value
			}
			for name, value := range change.Resources.Limits {
				resources.Limits[name] = value
			}
		}
	}
	updated = computeQoSClass(afterPod)
	return orig, updated, orig != updated
}

// CheckContainerMemoryDownscale returns an error when any target memory
// request or limit in the given resource lists is lower than the container's
// current memory. In-place memory downscale is not supported: a lower target
// would otherwise be silently treated as satisfied by the relaxed resource
// comparison used in the resize path.
func CheckContainerMemoryDownscale(container *corev1.Container, requests, limits corev1.ResourceList) error {
	if cur, ok := container.Resources.Requests[corev1.ResourceMemory]; ok && !cur.IsZero() {
		if target, has := requests[corev1.ResourceMemory]; has && !target.IsZero() && target.Cmp(cur) < 0 {
			return memoryDownscaleError("request", target, cur)
		}
	}
	if cur, ok := container.Resources.Limits[corev1.ResourceMemory]; ok && !cur.IsZero() {
		if target, has := limits[corev1.ResourceMemory]; has && !target.IsZero() && target.Cmp(cur) < 0 {
			return memoryDownscaleError("limit", target, cur)
		}
	}
	return nil
}

func memoryDownscaleError(kind string, target, cur resource.Quantity) error {
	return fmt.Errorf("target memory %s %s must not be lower than the current value %s: in-place memory downscale is not supported", kind, target.String(), cur.String())
}

// CheckMemoryDownscale returns an error if applying the sandbox template
// resources to the pod would lower any container's memory request or limit
// below its current value.
func CheckMemoryDownscale(box *agentsv1alpha1.Sandbox, pod *corev1.Pod) error {
	if box.Spec.Template == nil {
		return nil
	}
	templateContainers := buildContainerResourcesMap(box.Spec.Template.Spec.Containers)
	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		desired, ok := templateContainers[container.Name]
		if !ok {
			continue
		}
		if err := CheckContainerMemoryDownscale(container, desired.Resources.Requests, desired.Resources.Limits); err != nil {
			return fmt.Errorf("container %q: %w", container.Name, err)
		}
	}
	return nil
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

	current := pod.DeepCopy()
	// 两种模式均先 resize，再更新镜像与目标 hash，避免 resize 失败却提前更新 hash。
	// 目标收敛模式还会先记录资源意图，以便后续 patch 失败时继续观察；这些写入不是事务。
	if err := ctx.Err(); err != nil {
		return false, err
	}
	var state *InPlaceUpdateState
	if opts.Mode == TargetConvergenceMode {
		var err error
		state, err = GetPodInPlaceUpdateState(current)
		if err != nil {
			return false, fmt.Errorf("cannot read in-place update state: %w", err)
		}
	}

	// Step 1: perform resize (resource adjustment)
	resizeContainers := c.buildResizeContainers(InPlaceUpdateOptions{
		Box:  box,
		Pod:  current,
		Mode: opts.Mode,
	})
	resourceUpdateRequired := len(resizeContainers) > 0
	if opts.Mode == TargetConvergenceMode {
		resourceUpdateRequired = resourceUpdateRequired || opts.ResourceUpdateRequired || (state != nil && state.UpdateResources)
	}
	if len(resizeContainers) > 0 {
		// The intent must be persisted first; otherwise, if resize succeeds and a later patch fails,
		// the resource tracking would be lost.
		if opts.Mode == TargetConvergenceMode && state == nil {
			state = &InPlaceUpdateState{Revision: current.Labels[agentsv1alpha1.PodLabelTemplateHash], UpdateTimestamp: metav1.Now()}
		}
		if opts.Mode == TargetConvergenceMode && !state.UpdateResources {
			state.UpdateResources = true
			base := current.DeepCopy()
			if current.Annotations == nil {
				current.Annotations = map[string]string{}
			}
			current.Annotations[PodAnnotationInPlaceUpdateStateKey] = utils.DumpJson(state)
			if err := c.Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return false, fmt.Errorf("cannot record resize intent: %w", err)
			}
		}
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			err := c.resizeContainers(ctx, logger, current, resizeContainers, opts.Mode)
			if !apierrors.IsConflict(err) {
				return err
			}
			latestPod := &corev1.Pod{}
			if getErr := c.Get(ctx, client.ObjectKeyFromObject(current), latestPod); getErr != nil {
				return getErr
			}
			current = latestPod
			resizeContainers = c.buildResizeContainers(InPlaceUpdateOptions{
				Box:  box,
				Pod:  current,
				Mode: opts.Mode,
			})
			if opts.Mode != TargetConvergenceMode {
				resourceUpdateRequired = len(resizeContainers) > 0
			}
			if len(resizeContainers) == 0 {
				return nil
			}
			logger.V(5).Info("inplace update pod resize conflict, retrying with latest resourceVersion", "resourceVersion", current.ResourceVersion)
			return err
		}); err != nil {
			return false, err
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}

	// Step 2: perform patch (image + metadata finalization)
	patchOpts := opts
	patchOpts.Pod = current
	patchOpts.ResourceUpdateRequired = resourceUpdateRequired
	patchBody, err := c.generatePatchBody(patchOpts)
	if err != nil {
		return false, err
	}
	if patchBody != "" {
		if err := c.Patch(ctx, current, client.RawPatch(types.StrategicMergePatchType, []byte(patchBody))); err != nil {
			logger.Error(err, "inplace update pod patch failed")
			return false, err
		}
	}

	if patchBody == "" && len(resizeContainers) == 0 {
		return false, nil
	}
	logger.Info("inplace update pod success", "revision", revision, "patchBody", patchBody, "hasResizeContainers", len(resizeContainers) > 0)
	return true, nil
}

// resizeContainers applies a resource resize to the pod. It patches the pods/resize subresource
// on K8s >= 1.33 (see https://kubernetes.io/blog/2025/05/16/kubernetes-v1-33-in-place-pod-resize-beta/),
// and falls back to a direct strategic merge patch on older versions (K8s 1.27-1.32).
// The detection result is cached so the subresource is only probed once per controller lifetime.
func (c *InPlaceUpdateControl) resizeContainers(ctx context.Context, logger klog.Logger, pod *corev1.Pod, resizeContainers []corev1.Container, mode UpdateMode) error {
	resourcePatch := buildResourcePatch(resizeContainers)
	if mode == TargetConvergenceMode {
		resourcePatch = resourcePatchWithVersion(pod, resizeContainers)
	}
	if !c.useDirectResourcePatch.Load() {
		err := c.SubResource("resize").Patch(ctx, pod, client.RawPatch(types.StrategicMergePatchType, []byte(resourcePatch)))
		if err == nil {
			logger.Info("inplace update pod resize succeeded via resize subresource patch (K8s >= 1.33)")
			return nil
		}
		if !apierrors.IsNotFound(err) {
			logger.Error(err, "inplace update pod resize failed via resize subresource patch")
			return err
		}
		// The pods/resize subresource was introduced in K8s 1.33. On K8s 1.27-1.32,
		// in-place pod vertical scaling is done by directly patching spec.containers[].resources.
		logger.Info("resize subresource not found, switching to direct resource patch (K8s < 1.33)")
		c.useDirectResourcePatch.Store(true)
	}
	return c.patchPodResources(ctx, logger, pod, resizeContainers, mode)
}

// patchPodResources applies a strategic merge patch to update pod resources directly.
func (c *InPlaceUpdateControl) patchPodResources(ctx context.Context, logger klog.Logger, pod *corev1.Pod, resizeContainers []corev1.Container, mode UpdateMode) error {
	resourcePatch := buildResourcePatch(resizeContainers)
	if mode == TargetConvergenceMode {
		resourcePatch = resourcePatchWithVersion(pod, resizeContainers)
	}
	if err := c.Patch(ctx, pod, client.RawPatch(types.StrategicMergePatchType, []byte(resourcePatch))); err != nil {
		if apierrors.IsConflict(err) {
			logger.Error(err, "direct resource patch conflicted")
			return err
		}
		// 目标模式保留网络、限流和未知写入结果的原始错误；兼容模式保留原有包装合同。
		if mode != TargetConvergenceMode || apierrors.IsInvalid(err) || apierrors.IsMethodNotSupported(err) {
			return &ResizeNotSupportedError{Err: err}
		}
		return err
	}
	logger.Info("inplace update pod resize succeeded via direct resource patch (K8s < 1.33)")
	return nil
}

// Each resource write binds the observed version, to avoid a stale request overwriting a newer
// resource target.
func resourcePatchWithVersion(pod *corev1.Pod, containers []corev1.Container) string {
	return fmt.Sprintf(`{"metadata":{"resourceVersion":%q},"spec":%s}`, pod.ResourceVersion,
		utils.DumpJson(map[string]any{"containers": resourceContainersPatch(containers)}))
}

// IsInplaceUpdateCompleted 使用调用方已解析的状态检查更新是否完成，避免重复解析。
// 无记录时视为已完成；Ready 仍由 adapter 判断。
func IsInplaceUpdateCompleted(ctx context.Context, pod *corev1.Pod, state *InPlaceUpdateState) (bool, error) {
	return CompatibilityMode.IsInplaceUpdateCompleted(ctx, pod, state)
}

// IsInplaceUpdateCompleted 显式使用本次执行模式，避免新目标规则影响旧调用方。
func (mode UpdateMode) IsInplaceUpdateCompleted(ctx context.Context, pod *corev1.Pod, state *InPlaceUpdateState) (bool, error) {
	logger := logf.FromContext(ctx).WithValues("pod", klog.KObj(pod))

	if state == nil {
		return true, nil
	}
	if state.UpdateImages {
		if !mode.isPodImageUpdateCompleted(pod, state) {
			if mode == TargetConvergenceMode {
				if terminalErr := checkPodImagePullFailed(pod, state); terminalErr != nil {
					return false, terminalErr
				}
			}
			logger.Info("pod container image inplace update is not completed yet")
			return false, nil
		}
	}
	if state.UpdateResources {
		if !mode.isPodResourceResizeCompleted(pod) {
			if terminalErr := mode.checkPodResizeInfeasible(pod); terminalErr != nil {
				return false, terminalErr
			}
			logger.Info("pod resize resources are not applied yet")
			return false, nil
		}
	}
	return true, nil
}

// New records use the running target image as the completion criterion; a rollback to the
// original image no longer requires the ImageID to change.
// Historical records without TargetImage keep the old baseline comparison, to avoid a false
// completion during a rolling upgrade.
func (mode UpdateMode) isPodImageUpdateCompleted(pod *corev1.Pod, state *InPlaceUpdateState) bool {
	statuses := make(map[string]corev1.ContainerStatus, len(pod.Status.ContainerStatuses))
	for _, status := range pod.Status.ContainerStatuses {
		statuses[status.Name] = status
	}
	for name, previous := range state.LastContainerStatuses {
		current, ok := statuses[name]
		if mode != TargetConvergenceMode {
			if !ok || current.ImageID == previous.ImageID {
				return false
			}
			continue
		}
		if !ok || current.ImageID == "" || current.State.Waiting != nil {
			return false
		}
		if previous.TargetImage != "" {
			if current.State.Running == nil || !sameImageReference(current.Image, previous.TargetImage) {
				return false
			}
		} else if current.ImageID == previous.ImageID {
			return false
		}
	}
	return true
}

func sameImageReference(a, b string) bool {
	if a == b {
		return true
	}
	first, err := reference.ParseNormalizedNamed(a)
	if err != nil {
		return false
	}
	second, err := reference.ParseNormalizedNamed(b)
	return err == nil && reference.TagNameOnly(first).String() == reference.TagNameOnly(second).String()
}

// Only check deterministic errors for the containers tracked in this round; ErrImagePull/
// ImagePullBackOff are left to the caller's budget.
func checkPodImagePullFailed(pod *corev1.Pod, state *InPlaceUpdateState) error {
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		target, tracked := state.LastContainerStatuses[cs.Name]
		if !tracked {
			continue
		}
		// After a new target is dispatched, the old image's failure state may not have refreshed
		// yet, so it must not end the new round.
		if target.TargetImage != "" && !sameImageReference(cs.Image, target.TargetImage) {
			continue
		}
		if cs.State.Waiting == nil {
			continue
		}
		if cs.State.Waiting.Reason == "InvalidImageName" || cs.State.Waiting.Reason == "ErrImageNeverPull" {
			return &ImagePullFailedError{
				ContainerName: cs.Name,
				Reason:        cs.State.Waiting.Reason,
				Message:       cs.State.Waiting.Message,
			}
		}
	}
	return nil
}

// isPodResourceResizeCompleted checks whether the in-place resource resize has been
// fully applied by the kubelet. It compares each container's spec resources against
// the actual resources reported in status.containerStatuses[].resources, returning
// true only when all containers' status resources match their spec.
func isPodResourceResizeCompleted(pod *corev1.Pod) bool {
	return CompatibilityMode.isPodResourceResizeCompleted(pod)
}

func (mode UpdateMode) isPodResourceResizeCompleted(pod *corev1.Pod) bool {
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
		// 目标模式等待降配实际收敛；兼容模式继续接受至少满足目标的资源。
		if !mode.resourcesSatisfied(c.Resources, *status.Resources) {
			return false
		}
	}
	return true
}

// 默认模式将 Infeasible/Error/Deferred 视为失败，并兼容旧版 status.resize。
// 目标模式仅将 Infeasible/Error 视为失败，Deferred 继续等待。
func checkPodResizeInfeasible(pod *corev1.Pod) error {
	return CompatibilityMode.checkPodResizeInfeasible(pod)
}

func (mode UpdateMode) checkPodResizeInfeasible(pod *corev1.Pod) error {
	failure := func(message string) error {
		// 两种模式都保留结构化终态错误，供上层通过 errors.As 分类。
		return &ResizeInfeasibleError{Message: message}
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Status != corev1.ConditionTrue {
			continue
		}
		switch cond.Type {
		case corev1.PodResizePending:
			if cond.Reason == corev1.PodReasonInfeasible {
				return failure(fmt.Sprintf("pod resize is infeasible: %s", cond.Message))
			}
			if mode != TargetConvergenceMode && cond.Reason == corev1.PodReasonDeferred {
				return failure(fmt.Sprintf("pod resize is deferred: %s", cond.Message))
			}
		case corev1.PodResizeInProgress:
			if cond.Reason == corev1.PodReasonError {
				return failure(fmt.Sprintf("pod resize error: %s", cond.Message))
			}
		}
	}
	// Fallback compatibility check for older clusters (K8s 1.27-1.32) that still rely on the
	// deprecated pod.Status.Resize field.
	if pod.Status.Resize == corev1.PodResizeStatusInfeasible {
		return failure("pod resize is infeasible (status.resize)")
	}
	if mode != TargetConvergenceMode && pod.Status.Resize == corev1.PodResizeStatusDeferred {
		return failure("pod resize is deferred (status.resize)")
	}
	return nil
}

// IsResourceSatisfied checks whether the actual resources satisfy the desired resources.
// It only checks resources specified in 'desired' (the sandbox spec), ignoring extra resources
// in 'actual' (the pod) injected by the system (e.g., ephemeral-storage).
// It uses Quantity.Cmp() to handle different unit representations (e.g., "1" vs "1000m").
func IsResourceSatisfied(desired, actual corev1.ResourceRequirements) bool {
	if !isResourceListCovered(actual.Limits, desired.Limits) {
		return false
	}
	if !isResourceListCovered(actual.Requests, desired.Requests) {
		return false
	}
	return true
}

// ResourcesExactlyEqual checks whether the actual resources exactly match the desired resources.
// Unlike IsResourceSatisfied, this requires exact equality (not >=) and is used by callers
// that need to detect any resource drift, including when actual exceeds desired (e.g., VPA modifications).
func ResourcesExactlyEqual(desired, actual corev1.ResourceRequirements) bool {
	return isResourceListExactlyEqual(actual.Limits, desired.Limits) &&
		isResourceListExactlyEqual(actual.Requests, desired.Requests)
}

func isResourceListExactlyEqual(actual, expected corev1.ResourceList) bool {
	for name, expectedQ := range expected {
		actualQ, ok := actual[name]
		if !ok || actualQ.Cmp(expectedQ) != 0 {
			return false
		}
	}
	return true
}

// isResourceListCovered checks whether every resource in 'expected' is satisfied by 'actual'.
// It uses Cmp instead of equality because some environments round up or adjust container
// resources (e.g., Kubernetes CPU/memory normalization), so an exact match is too strict.
// A resource is considered covered when actualQ >= expectedQ.
func isResourceListCovered(actual, expected corev1.ResourceList) bool {
	for name, expectedQ := range expected {
		actualQ, ok := actual[name]
		if !ok || actualQ.Cmp(expectedQ) < 0 {
			return false
		}
	}
	return true
}
