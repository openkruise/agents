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
	"slices"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/api/v1alpha1"
	infracache "github.com/openkruise/agents/pkg/cache"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

// prepareColdStartOptions keeps fresh objects within the backend's watch scope.
func (i *Infra) prepareColdStartOptions(opts *infra.ClaimSandboxOptions) error {
	if opts.ColdStart == nil {
		return nil
	}
	// Fresh objects need a concrete namespace within this backend's watch scope.
	if opts.Namespace == "" {
		opts.Namespace = i.sandboxNamespace
		if opts.Namespace == "" {
			opts.Namespace = corev1.NamespaceDefault
		}
	}
	if i.sandboxNamespace != "" && opts.Namespace != i.sandboxNamespace {
		return managererrors.NewError(managererrors.ErrorNotFound, "sandbox namespace is outside the configured scope")
	}
	if i.sandboxLabelSelector != "" {
		selector, err := labels.Parse(i.sandboxLabelSelector)
		if err != nil {
			return managererrors.NewError(managererrors.ErrorInternal, "invalid backend sandbox selector: %v", err)
		}
		modifier := opts.Modifier
		opts.Modifier = func(sbx infra.Sandbox) error {
			if modifier != nil {
				if err := modifier(sbx); err != nil {
					return err
				}
			}
			// Claim adds this label after the modifier. Verify the final
			// labels so fresh objects cannot escape the backend's cache.
			finalLabels := maps.Clone(sbx.GetLabels())
			if finalLabels == nil {
				finalLabels = map[string]string{}
			}
			finalLabels[v1alpha1.LabelSandboxIsClaimed] = v1alpha1.True
			if !selector.Matches(labels.Set(finalLabels)) {
				return managererrors.NewError(managererrors.ErrorBadRequest, "cold workload does not match the configured sandbox selector")
			}
			return nil
		}
	}
	return nil
}

// pickSandboxForClaim adds cold creation without changing pool selection.
func pickSandboxForClaim(ctx context.Context, opts infra.ClaimSandboxOptions, pickCache *sync.Map, cache infracache.Provider) (*Sandbox, infra.LockType, error) {
	if opts.ColdStart != nil {
		return newColdSandbox(ctx, opts, cache)
	}
	return pickAnAvailableSandbox(ctx, opts, pickCache, cache)
}

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
	sbx := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: opts.Namespace, GenerateName: "sandbox-",
			Annotations: map[string]string{v1alpha1.SandboxAnnotationPriority: "100"},
		},
		Spec: v1alpha1.SandboxSpec{
			Runtimes: slices.Clone(opts.RuntimeConfig),
			EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{Spec: podSpec},
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
	if err := validateExistingVolumeMounts(inputs); err != nil {
		return nil, nil, err
	}
	for _, input := range inputs {
		if _, err := readExistingPVC(ctx, namespace, input.VolumeName, reader); err != nil {
			return nil, nil, err
		}
		volumes = append(volumes, corev1.Volume{Name: input.Name, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: input.VolumeName, ReadOnly: input.ReadOnly}}})
		mounts = append(mounts, corev1.VolumeMount{Name: input.Name, MountPath: input.MountPath, SubPath: input.SubPath, ReadOnly: input.ReadOnly})
	}
	return volumes, mounts, nil
}
