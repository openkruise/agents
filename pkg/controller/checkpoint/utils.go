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

package checkpoint

import (
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/strategicpatch"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils/inplaceupdate"
)

const patchDirectiveMarker = "$patch"

func getTemplateContainers(box *agentsv1alpha1.Sandbox) []corev1.Container {
	if box.Spec.Template != nil {
		return box.Spec.Template.Spec.Containers
	}
	return nil
}

func getTemplateInitContainers(box *agentsv1alpha1.Sandbox) []corev1.Container {
	if box.Spec.Template != nil {
		return box.Spec.Template.Spec.InitContainers
	}
	return nil
}

// toPatchMap converts a typed API object into the map form required by a
// Strategic Merge Patch so that directive elements can be mixed into list
// values.
func toPatchMap(obj any) (map[string]any, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal patch element: %w", err)
	}
	out := map[string]any{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("failed to unmarshal patch element: %w", err)
	}
	return out, nil
}

// buildReplaceList encodes a list so that applying the patch replaces the
// whole base list instead of merging it element by element. The leading
// replace directive only works when the enclosing element matches the base
// element by name: Strategic Merge Patch appends unmatched elements verbatim
// without interpreting nested directives, so this helper must only be used
// for fields of containers present in the base Pod at resume time.
func buildReplaceList[T any](items []T) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(items)+1)
	out = append(out, map[string]any{patchDirectiveMarker: "replace"})
	for i := range items {
		m, err := toPatchMap(&items[i])
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// buildTemplateContainerDelta returns template-defined containers carrying the
// fields that drifted from the sandbox template definition:
//  1. resources changed by VPA or similar components
//  2. volumeMounts injected into the container after Pod creation (csi-mount,
//     agent-runtime, CA bundle injection, admission webhooks)
//  3. env vars injected or mutated the same way
//
// Mounts and env are captured only on drift and replace the whole list, so the
// resumed Pod keeps the pause-time injection even if the runtime injection
// configuration changed while the sandbox was paused.
func buildTemplateContainerDelta(pod *corev1.Pod, box *agentsv1alpha1.Sandbox) ([]map[string]any, error) {
	templateContainers := getTemplateContainers(box)
	templateMap := make(map[string]corev1.Container, len(templateContainers))
	for _, c := range templateContainers {
		templateMap[c.Name] = c
	}

	var result []map[string]any
	for _, pc := range pod.Spec.Containers {
		tc, isTemplate := templateMap[pc.Name]
		if !isTemplate {
			continue
		}
		delta := map[string]any{"name": pc.Name}
		if !inplaceupdate.ResourcesExactlyEqual(tc.Resources, pc.Resources) {
			delta["resources"] = *pc.Resources.DeepCopy()
		}
		if !apiequality.Semantic.DeepEqual(tc.VolumeMounts, pc.VolumeMounts) {
			mounts, err := buildReplaceList(pc.VolumeMounts)
			if err != nil {
				return nil, err
			}
			delta["volumeMounts"] = mounts
		}
		if !apiequality.Semantic.DeepEqual(tc.Env, pc.Env) {
			env, err := buildReplaceList(pc.Env)
			if err != nil {
				return nil, err
			}
			delta["env"] = env
		}
		if len(delta) > 1 {
			result = append(result, delta)
		}
	}
	return result, nil
}

// buildVolumeDelta captures every volume of the live Pod. Each volume is
// encoded as a "$patch": "delete" entry followed by the full pause-time
// definition, which replaces the base volume with the same name even when its
// source type changed (merging a volume that names a different source would
// leave two sources set and produce an invalid Pod). Volumes present only in
// the resume base are deliberately left untouched: a sidecar injected after
// the pause may mount them.
func buildVolumeDelta(pod *corev1.Pod) ([]map[string]any, error) {
	if len(pod.Spec.Volumes) == 0 {
		return nil, nil
	}
	out := make([]map[string]any, 0, 2*len(pod.Spec.Volumes))
	for i := range pod.Spec.Volumes {
		out = append(out, map[string]any{
			"name":               pod.Spec.Volumes[i].Name,
			patchDirectiveMarker: "delete",
		})
	}
	for i := range pod.Spec.Volumes {
		m, err := toPatchMap(&pod.Spec.Volumes[i])
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// containersToPatchMaps converts full container copies into patch maps.
func containersToPatchMaps(cs []corev1.Container) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(cs))
	for i := range cs {
		m, err := toPatchMap(&cs[i])
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// buildInjectedContainerDelta returns containers from the live pod that are not
// defined in the sandbox template (runtime-injected and webhook-injected).
func buildInjectedContainerDelta(pod *corev1.Pod, box *agentsv1alpha1.Sandbox) (containers []corev1.Container, initContainers []corev1.Container) {
	templateNames := make(map[string]struct{})
	for _, c := range getTemplateContainers(box) {
		templateNames[c.Name] = struct{}{}
	}
	templateInitNames := make(map[string]struct{})
	for _, c := range getTemplateInitContainers(box) {
		templateInitNames[c.Name] = struct{}{}
	}

	for _, pc := range pod.Spec.Containers {
		if _, isTemplate := templateNames[pc.Name]; !isTemplate {
			containers = append(containers, *pc.DeepCopy())
		}
	}
	for _, ic := range pod.Spec.InitContainers {
		if _, isTemplate := templateInitNames[ic.Name]; !isTemplate {
			initContainers = append(initContainers, *ic.DeepCopy())
		}
	}
	return
}

// buildContainerNameOrder returns the partial-key list used by the
// strategic-merge-patch "$setElementOrder/<list>" directive. Including this
// directive in the delta forces the resumed Pod's container slice to follow
// the order observed at pause time, which protects the resume path from
// drift caused by InjectSandboxRuntimes producing a different injection
// order (e.g. csi-mount and agent-runtime swapping positions).
func buildContainerNameOrder(cs []corev1.Container) []map[string]string {
	if len(cs) == 0 {
		return nil
	}
	out := make([]map[string]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, map[string]string{"name": c.Name})
	}
	return out
}

// BuildPodTemplateDelta assembles a Strategic Merge Patch from the
// spec-level delta components captured at pause time:
//  1. Template containers: resource changes (e.g. VPA), plus volumeMounts and
//     env injected after Pod creation
//  2. Injected containers: runtime-injected and webhook-injected containers
//  3. Volumes: every volume of the live Pod, so resume keeps the pause-time
//     volume definitions even if the template or injection configuration
//     changes while the sandbox is paused
//
// Pod metadata (whitelisted labels/annotations) is deliberately excluded:
// the same whitelist is already carried by Sandbox Status.PodInfo and
// injected via InjectResumedPod on resume paths that need it, while a
// pause-time metadata snapshot would replay stale runtime-binding
// annotations (e.g. instance-id) onto a Pod whose underlying instance was
// released with the checkpoint, permanently sticking the resume in Pending.
func BuildPodTemplateDelta(pod *corev1.Pod, box *agentsv1alpha1.Sandbox) (runtime.RawExtension, error) {
	containers, err := buildTemplateContainerDelta(pod, box)
	if err != nil {
		return runtime.RawExtension{}, fmt.Errorf("failed to build template container delta: %w", err)
	}
	injected, injectedInit := buildInjectedContainerDelta(pod, box)
	injectedMaps, err := containersToPatchMaps(injected)
	if err != nil {
		return runtime.RawExtension{}, err
	}
	injectedInitMaps, err := containersToPatchMaps(injectedInit)
	if err != nil {
		return runtime.RawExtension{}, err
	}
	containers = append(containers, injectedMaps...)
	volumes, err := buildVolumeDelta(pod)
	if err != nil {
		return runtime.RawExtension{}, fmt.Errorf("failed to build volume delta: %w", err)
	}

	if len(containers) == 0 && len(injectedInitMaps) == 0 && len(volumes) == 0 {
		return runtime.RawExtension{}, nil
	}

	patch := map[string]any{}
	spec := map[string]any{}
	if len(containers) > 0 {
		spec["containers"] = containers
		// Force resumed Pod's container order to match the live Pod at
		// pause time. Without this directive Strategic Merge Patch keeps
		// the base Pod's order, so a wrong injection order during
		// resume (e.g. agent-runtime before csi-mount) would not be
		// corrected.
		spec["$setElementOrder/containers"] = buildContainerNameOrder(pod.Spec.Containers)
	}
	if len(injectedInitMaps) > 0 {
		spec["initContainers"] = injectedInitMaps
		spec["$setElementOrder/initContainers"] = buildContainerNameOrder(pod.Spec.InitContainers)
	}
	if len(volumes) > 0 {
		spec["volumes"] = volumes
	}
	patch["spec"] = spec

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return runtime.RawExtension{}, fmt.Errorf("failed to marshal pod delta: %w", err)
	}
	return runtime.RawExtension{Raw: patchBytes}, nil
}

// ApplyPodTemplateDelta applies a Strategic Merge Patch (from the Checkpoint CR)
// to the generated base Pod at resume time.
func ApplyPodTemplateDelta(pod *corev1.Pod, podTemplateDelta runtime.RawExtension) error {
	if len(podTemplateDelta.Raw) == 0 {
		return nil
	}

	podJSON, err := json.Marshal(pod)
	if err != nil {
		return fmt.Errorf("failed to marshal pod: %w", err)
	}

	patchedJSON, err := strategicpatch.StrategicMergePatch(podJSON, podTemplateDelta.Raw, &corev1.Pod{})
	if err != nil {
		return fmt.Errorf("failed to apply strategic merge patch: %w", err)
	}

	// Decode into a fresh Pod: encoding/json reuses existing slice elements
	// when decoding into a pre-populated value, so fields absent from the
	// patched JSON would survive from the old element. A replaced volume that
	// changed its source would then keep the previous source field and become
	// invalid.
	patched := &corev1.Pod{}
	if err := json.Unmarshal(patchedJSON, patched); err != nil {
		return fmt.Errorf("failed to unmarshal patched pod: %w", err)
	}
	*pod = *patched

	return nil
}
