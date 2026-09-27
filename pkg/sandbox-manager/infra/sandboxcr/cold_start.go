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

package sandboxcr

import (
	"context"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/google/uuid"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/api/v1alpha1"
	infracache "github.com/openkruise/agents/pkg/cache"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

// newColdSandbox only constructs an unpersisted candidate. The normal claim
// pipeline still owns admission, owner binding, persistence, readiness,
// runtime initialization and failure cleanup. No shared source is modified.
func newColdSandbox(ctx context.Context, opts infra.ClaimSandboxOptions, cache infracache.Provider) (*Sandbox, infra.LockType, error) {
	input := opts.ColdStart
	if input.Image == "" || opts.Namespace == "" {
		return nil, "", managererrors.NewError(managererrors.ErrorBadRequest, "cold start requires image and namespace")
	}
	requests, err := coldResources(input.ResourceRequests)
	if err != nil {
		return nil, "", err
	}
	limits, err := coldResources(input.ResourceLimits)
	if err != nil {
		return nil, "", err
	}
	container := corev1.Container{
		Name: "sandbox", Image: input.Image, Command: slices.Clone(input.Command),
		Resources: corev1.ResourceRequirements{Requests: requests, Limits: limits},
	}
	for _, name := range slices.Sorted(maps.Keys(input.EnvVars)) {
		container.Env = append(container.Env, corev1.EnvVar{Name: name, Value: input.EnvVars[name]})
	}
	volumes, mounts, err := coldVolumeMounts(ctx, opts.Namespace, input.Volumes, cache.GetClient())
	if err != nil {
		return nil, "", err
	}
	container.VolumeMounts = mounts
	podSpec := corev1.PodSpec{Containers: []corev1.Container{container}, Volumes: volumes}
	if input.OS != "" || input.Architecture != "" {
		podSpec.NodeSelector = map[string]string{}
		if input.OS != "" {
			podSpec.NodeSelector[corev1.LabelOSStable] = input.OS
		}
		if input.Architecture != "" {
			podSpec.NodeSelector[corev1.LabelArchStable] = input.Architecture
		}
	}
	var podLabels map[string]string
	runtimes := slices.Clone(opts.RuntimeConfig)
	if input.EgressPolicy != nil {
		podLabels = map[string]string{coldNetworkSchedulingGate: uuid.NewString()}
		podSpec.SchedulingGates = []corev1.PodSchedulingGate{{Name: coldNetworkSchedulingGate}}
		if !slices.ContainsFunc(runtimes, func(r v1alpha1.RuntimeConfig) bool { return r.Name == v1alpha1.RuntimeConfigForInjectTrafficProxy }) {
			runtimes = append(runtimes, v1alpha1.RuntimeConfig{Name: v1alpha1.RuntimeConfigForInjectTrafficProxy})
		}
	}
	sbx := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: opts.Namespace, GenerateName: "sandbox-",
			Annotations: map[string]string{v1alpha1.SandboxAnnotationPriority: "100"},
		},
		Spec: v1alpha1.SandboxSpec{
			Runtimes: runtimes,
			EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: podLabels}, Spec: podSpec},
			},
		},
	}
	return AsSandbox(sbx, cache), infra.LockTypeCreate, nil
}

func coldResources(values map[string]string) (corev1.ResourceList, error) {
	resources := make(corev1.ResourceList, len(values))
	for _, name := range slices.Sorted(maps.Keys(values)) {
		quantity, err := resource.ParseQuantity(values[name])
		if err != nil {
			return nil, managererrors.NewError(managererrors.ErrorBadRequest, "invalid resource quantity for %s: %v", name, err)
		}
		resources[corev1.ResourceName(name)] = quantity
	}
	return resources, nil
}

// coldVolumeMounts reads existing same-namespace claims through the cached
// client. It never creates, mutates, or takes ownership of a claim.
func coldVolumeMounts(ctx context.Context, namespace string, inputs []infra.ExistingVolumeMount, reader client.Reader) ([]corev1.Volume, []corev1.VolumeMount, error) {
	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount
	names := map[string]bool{}
	paths := map[string]bool{}
	for _, input := range inputs {
		if len(validation.IsDNS1123Label(input.Name)) != 0 || len(validation.IsDNS1123Subdomain(input.VolumeName)) != 0 {
			return nil, nil, managererrors.NewError(managererrors.ErrorBadRequest, "invalid volume or claim name")
		}
		if !path.IsAbs(input.MountPath) || path.IsAbs(input.SubPath) {
			return nil, nil, managererrors.NewError(managererrors.ErrorBadRequest, "volume mountPath must be absolute and subPath must be relative")
		}
		for _, part := range strings.Split(input.SubPath, "/") {
			if part == ".." {
				return nil, nil, managererrors.NewError(managererrors.ErrorBadRequest, "volume subPath must not contain parent traversal")
			}
		}
		if names[input.Name] || paths[input.MountPath] {
			return nil, nil, managererrors.NewError(managererrors.ErrorBadRequest, "duplicate volume name or mount path")
		}
		names[input.Name], paths[input.MountPath] = true, true
		var pvc corev1.PersistentVolumeClaim
		if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: input.VolumeName}, &pvc); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, nil, managererrors.NewError(managererrors.ErrorBadRequest, "PVC %s/%s does not exist; volume provisioning is not implemented", namespace, input.VolumeName)
			}
			return nil, nil, managererrors.WrapError(managererrors.ErrorInternal, err, "read existing PVC %s/%s", namespace, input.VolumeName)
		}
		if !pvc.DeletionTimestamp.IsZero() {
			return nil, nil, managererrors.NewError(managererrors.ErrorBadRequest, "PVC %s/%s is terminating", namespace, input.VolumeName)
		}
		volumes = append(volumes, corev1.Volume{Name: input.Name, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: input.VolumeName, ReadOnly: input.ReadOnly}}})
		mounts = append(mounts, corev1.VolumeMount{Name: input.Name, MountPath: input.MountPath, SubPath: input.SubPath, ReadOnly: input.ReadOnly})
	}
	return volumes, mounts, nil
}
