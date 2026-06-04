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

	// ContainerSelector decides, for a multi-container Pod, which containers
	// receive the CA injection (both VolumeMount and EnvVars).
	//
	// A sandbox Pod typically carries more than one container — the main agent
	// workload plus sidecars such as the traffic-proxy, runtime sidecar, CSI
	// helper, and metrics agent. Most CA bundles are only meaningful to a
	// subset of them (e.g. the gateway CA only needs to land in the main
	// container that issues outbound HTTPS calls), so callers MUST think about
	// scope when registering a spec rather than blanket-injecting.
	//
	// Built-in helpers cover the common cases:
	//   - OnlyMainContainer()              — first container in
	//                                        pod.Spec.Containers, preserves
	//                                        the historical gateway-CA
	//                                        injector behaviour.
	//   - ByContainerName(names…)          — explicit allow-list by container
	//                                        name, use this when only specific
	//                                        sidecars need the CA and the main
	//                                        container does not.
	//   - MainContainerOrByName(names…)    — main container PLUS the named
	//                                        sidecars (union of the two
	//                                        helpers above); the typical pick
	//                                        when the agent workload AND a
	//                                        sidecar (e.g. traffic-proxy)
	//                                        both need the bundle.
	//   - AllContainers()                  — every container; reserve for
	//                                        cluster-wide trust roots that
	//                                        every workload must honour.
	//
	// Custom selectors (e.g. by label, image prefix, or annotation) can be
	// supplied directly as a function literal.
	//
	// Leaving this nil falls back to OnlyMainContainer() inside the injector
	// as a defensive default, but new specs SHOULD set it explicitly so the
	// container scope is part of the spec's hard contract rather than implicit
	// behaviour.
	ContainerSelector ContainerSelector

	// EnvVars is an optional list of environment variables that the injector
	// appends to every container selected by ContainerSelector, alongside the
	// VolumeMount. It is intended for CA bundles whose consumers rely on
	// well-known env vars to discover the trusted certificate file, e.g.:
	//
	//   SSL_CERT_FILE       (OpenSSL / Go x509 SystemCertPool override)
	//   NODE_EXTRA_CA_CERTS (Node.js)
	//   REQUESTS_CA_BUNDLE  (Python requests)
	//   CURL_CA_BUNDLE      (curl / libcurl)
	//
	// Typical values point at MountPath so user-mode HTTP clients automatically
	// trust the bundle without per-application configuration.
	//
	// Note: EnvVars share the same ContainerSelector as VolumeMount above, so
	// env vars and the certificate file always land on the same set of
	// containers — a Pod's sidecars will only see them if the selector
	// explicitly opts those containers in.
	//
	// Injection is idempotent at the env-name level: if the container already
	// has an env entry whose Name matches an entry here, the existing entry is
	// preserved untouched (operator-supplied overrides win).
	EnvVars []corev1.EnvVar

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

// MainContainerOrByName returns a ContainerSelector that matches the main
// container (first entry in pod.Spec.Containers) PLUS any container whose Name
// is in the provided allow-list.
//
// This is the typical pick when a CA bundle must be visible both to the agent
// workload running in the main container AND to a sidecar that performs
// outbound HTTPS on the agent's behalf (e.g. the traffic-proxy). The first
// container is always selected regardless of its Name, so this helper does not
// require callers to know the dynamic main-container name produced by the
// pod-template renderer.
//
// Passing an empty names list degenerates to OnlyMainContainer().
func MainContainerOrByName(names ...string) ContainerSelector {
	allow := make(map[string]struct{}, len(names))
	for _, n := range names {
		allow[n] = struct{}{}
	}
	return func(c *corev1.Container, index int) bool {
		if index == 0 {
			return true
		}
		_, ok := allow[c.Name]
		return ok
	}
}

// AllContainers returns a ContainerSelector that matches every container.
func AllContainers() ContainerSelector {
	return func(_ *corev1.Container, _ int) bool { return true }
}
