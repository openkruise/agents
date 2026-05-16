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

package identity

import (
	corev1 "k8s.io/api/core/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// ContainerSelector decides whether a container should receive a CA volume mount.
// Returning true means the container is included.
//
// The index parameter is the container's position in pod.Spec.Containers and
// allows stateless implementations such as OnlyMainContainer to identify the
// first container without relying on closure-shared state (which would break on
// repeated invocations).
type ContainerSelector func(container *corev1.Container, index int) bool

// SandboxPredicate decides whether a CABundleSpec applies to the given sandbox at
// injection time. When non-nil and returning false, the spec is fully skipped
// (neither ensure nor inject) for that sandbox.
type SandboxPredicate func(sbx *agentsv1alpha1.Sandbox) bool

// CABundleSpec describes one logical CA bundle to be ensured-as-Secret in the target
// namespace (by copying from the system namespace) and injected into the sandbox pod
// as a Volume + VolumeMount.
//
// The authoritative source of every CA bundle is a Secret in the system namespace
// (utils.DefaultSandboxDeployNamespace, i.e. "sandbox-system"). The injector copies
// the Secret on-demand into the target namespace and never fetches CA content from
// remote services. If the source Secret is missing in the system namespace, the
// ensure step returns an error and blocks pod creation.
type CABundleSpec struct {
	// Name is the logical identifier of the CA bundle. It is used in logs and as
	// the dedupe key when registering specs. Re-registering with the same name
	// overrides the previous spec.
	Name string

	// SecretName is the Secret name in BOTH the system namespace (authoritative
	// source) and the target namespace (replicated copy). The two Secrets share
	// the same name to keep operators' mental model simple.
	SecretName string

	// SecretDataKey is the key inside Secret.Data whose value (a PEM-encoded CA
	// certificate) should be mounted into the container at MountPath.
	SecretDataKey string

	// VolumeName is the name of the corev1.Volume appended to pod.Spec.Volumes.
	// It is also the name used by the corresponding corev1.VolumeMount entry.
	VolumeName string

	// MountPath is the absolute path inside the container where the CA file is
	// exposed.
	MountPath string

	// SubPath is the optional VolumeMount.SubPath. When non-empty, it allows a
	// single Secret data key to be mounted as a single file rather than a
	// directory.
	SubPath string

	// ReadOnly controls VolumeMount.ReadOnly. CA bundles should always be
	// read-only inside the container.
	ReadOnly bool

	// ContainerSelector decides which containers receive the volume mount.
	// nil defaults to OnlyMainContainer (the first container in pod.Spec.Containers),
	// which preserves the historical behavior of the gateway CA injector.
	ContainerSelector ContainerSelector

	// EnabledFor is a sandbox-level predicate that gates both the ensure and
	// inject steps. nil means "always enabled when the injector is invoked".
	// Typical bindings are made at controller startup via BindCAEnabledFor to
	// avoid coupling the identity package to runtime-specific concepts (e.g.
	// sidecarutils.IsRuntimeEnabled).
	EnabledFor SandboxPredicate
}

// OnlyMainContainer returns a ContainerSelector that matches only the first
// container in pod.Spec.Containers. This preserves the historical behavior of
// the gateway CA injector.
func OnlyMainContainer() ContainerSelector {
	return func(_ *corev1.Container, index int) bool {
		return index == 0
	}
}

// ByContainerName returns a ContainerSelector that matches containers whose Name
// is in the provided allow-list.
func ByContainerName(names ...string) ContainerSelector {
	allow := make(map[string]struct{}, len(names))
	for _, n := range names {
		allow[n] = struct{}{}
	}
	return func(c *corev1.Container, _ int) bool {
		_, ok := allow[c.Name]
		return ok
	}
}

// AllContainers returns a ContainerSelector that matches every container.
func AllContainers() ContainerSelector {
	return func(_ *corev1.Container, _ int) bool { return true }
}
