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
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

// Built-in gateway CA bundle constants.
//
// These mirror the historical values used by the legacy GatewayCACertInjector,
// so that existing deployments continue to mount the gateway CA at the same
// path with the same Secret/key names.
const (
	// GatewayCABundleName is the logical name of the gateway CA bundle in the
	// CABundleSpec registry. Use this constant whenever you need to reference
	// the spec by name, including:
	//   - BindCAEnabledFor(GatewayCABundleName, ...) at controller startup to
	//     wire the runtime-gating predicate.
	//   - RegisterCABundleSpec(CABundleSpec{Name: GatewayCABundleName, ...})
	//     from an enterprise inner_*.go file to override the community
	//     baseline (selector / EnvVars / Secret references) in one shot.
	GatewayCABundleName = "gateway"

	// GatewayCASecretName is the Secret name (in BOTH sandbox-system and target
	// namespaces) holding the sandbox gateway CA certificate.
	GatewayCASecretName = "sandbox-gateway-crt"

	// GatewayCAKey is the data key within the gateway CA Secret whose value is
	// the PEM-encoded CA certificate.
	GatewayCAKey = "sandbox-gateway-ca.crt"

	// gatewayCAVolumeName is the corev1.Volume / corev1.VolumeMount name used
	// to expose the gateway CA file inside the container.
	gatewayCAVolumeName = "sandbox-gateway-ca"

	// gatewayCAMountPath is the absolute file path inside the container at
	// which the gateway CA certificate is exposed.
	gatewayCAMountPath = "/etc/ssl/certs/agent-identity/gateway-ca.crt"
)

// init registers the community baseline CABundleSpec for the gateway CA.
//
// The baseline is intentionally minimal:
//   - ContainerSelector defaults to OnlyMainContainer() — the gateway CA is
//     mounted only into the agent workload container.
//   - EnvVars defaults to a single SSL_CERT_FILE entry as a representative
//     well-known trust hint; richer ecosystems (Node.js, Python requests,
//     curl, ...) are out of scope for the community baseline.
//   - EnabledFor is left nil so the identity package does not depend on
//     runtime-gating logic. The controller startup code is expected to call
//     BindCAEnabledFor(GatewayCABundleName, ...) to wire the runtime predicate.
//
// Enterprise builds replace this entire spec by calling RegisterCABundleSpec
// with the same Name from an inner_*.go init() — see ca_cert_registry.go for
// the override semantics.
func init() {
	RegisterCABundleSpec(CABundleSpec{
		Name:              GatewayCABundleName,
		SecretName:        GatewayCASecretName,
		SecretDataKey:     GatewayCAKey,
		VolumeName:        gatewayCAVolumeName,
		MountPath:         gatewayCAMountPath,
		SubPath:           GatewayCAKey,
		ReadOnly:          true,
		ContainerSelector: OnlyMainContainer(),
		EnvVars: []corev1.EnvVar{
			{Name: "SSL_CERT_FILE", Value: gatewayCAMountPath},
		},
		EnabledFor: nil, // bound at controller startup via BindCAEnabledFor
	})
}

// CA bundle ensure/inject API. Behaviour is driven entirely by CABundleSpec
// entries in the global registry, so adding a new bundle only requires a
// new RegisterCABundleSpec call. The Secret RBAC marker lives next to
// SandboxReconciler.Reconcile (the sole consumer).

// EnsureAllCACerts ensures every enabled CABundleSpec has its Secret in
// targetNamespace, copying from utils.DefaultSandboxDeployNamespace on demand.
// The first spec failure aborts and is propagated; partial progress is left
// in place since copies are idempotent. A missing source Secret is reported
// as an error to block pod creation.
func EnsureAllCACerts(ctx context.Context, cli client.Client, sbx *agentsv1alpha1.Sandbox, targetNamespace string) error {
	specs := ListCABundleSpecs()
	for i := range specs {
		spec := specs[i]
		if !specEnabled(&spec, sbx) {
			klog.V(5).InfoS("CABundleSpec disabled by EnabledFor predicate, skipping ensure",
				"name", spec.Name, "sandbox", klog.KObj(sbx))
			continue
		}
		if err := ensureCACert(ctx, cli, &spec, targetNamespace); err != nil {
			return fmt.Errorf("failed to ensure CA bundle %q: %w", spec.Name, err)
		}
	}
	return nil
}

// InjectAllCAVolumes appends one corev1.Volume per enabled CABundleSpec to
// pod.Spec.Volumes. Volumes whose Name already exists on the pod are skipped
// to keep the operation idempotent.
//
// Volume is a pod-level resource, so this function does not consult
// ContainerSelector — every selected spec yields exactly one Volume regardless
// of how many containers will eventually mount it via InjectAllCAIntoContainers.
func InjectAllCAVolumes(_ context.Context, sbx *agentsv1alpha1.Sandbox, pod *corev1.Pod) {
	specs := ListCABundleSpecs()
	for i := range specs {
		spec := specs[i]
		if !specEnabled(&spec, sbx) {
			continue
		}
		if findVolumeByName(pod.Spec.Volumes, spec.VolumeName) {
			klog.V(5).InfoS("CA volume already present on pod, skipping",
				"name", spec.Name, "volume", spec.VolumeName, "pod", klog.KObj(pod))
			continue
		}
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: spec.VolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  spec.SecretName,
					DefaultMode: ptrTo[int32](0644),
				},
			},
		})
	}
}

// InjectAllCAIntoContainers walks every enabled CABundleSpec and, for each
// container that the spec's ContainerSelector matches, appends the spec's
// VolumeMount and EnvVars to that container in a single pass.
//
// VolumeMount and EnvVar entries whose Name already exists on the container
// are preserved untouched, keeping the operation idempotent and avoiding
// clobbering operator-supplied overrides.
//
// VolumeMounts are always injected when the spec is enabled and the container
// is selected; EnvVars are only injected when the spec declares any. The
// container-level gating (specEnabled + ContainerSelector) is shared between
// the two artefacts because both target the same set of containers, so a
// single iteration keeps the selection logic in lock-step.
func InjectAllCAIntoContainers(_ context.Context, sbx *agentsv1alpha1.Sandbox, pod *corev1.Pod) {
	if len(pod.Spec.Containers) == 0 {
		klog.V(5).InfoS("no containers in pod, skipping CA container injection",
			"pod", klog.KObj(pod))
		return
	}
	specs := ListCABundleSpecs()
	for i := range specs {
		spec := specs[i]
		if !specEnabled(&spec, sbx) {
			continue
		}
		selector := spec.ContainerSelector
		if selector == nil {
			selector = OnlyMainContainer()
		}
		for idx := range pod.Spec.Containers {
			c := &pod.Spec.Containers[idx]
			if !selector(c, idx) {
				continue
			}
			injectCAVolumeMount(&spec, c)
			injectCAEnvVars(&spec, c)
		}
	}
}

// injectCAVolumeMount appends spec's VolumeMount to the container, skipping
// when an entry with the same Name already exists.
func injectCAVolumeMount(spec *CABundleSpec, c *corev1.Container) {
	if findVolumeMountByName(c.VolumeMounts, spec.VolumeName) {
		klog.V(5).InfoS("CA volume mount already present on container, skipping",
			"name", spec.Name, "container", c.Name, "volume", spec.VolumeName)
		return
	}
	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
		Name:      spec.VolumeName,
		MountPath: spec.MountPath,
		SubPath:   spec.SubPath,
		ReadOnly:  spec.ReadOnly,
	})
}

// injectCAEnvVars appends spec's EnvVars to the container, skipping any whose
// Name already exists. No-op when the spec declares no EnvVars.
func injectCAEnvVars(spec *CABundleSpec, c *corev1.Container) {
	for _, ev := range spec.EnvVars {
		if findEnvVarByName(c.Env, ev.Name) {
			klog.V(5).InfoS("CA env var already present on container, skipping",
				"name", spec.Name, "container", c.Name, "env", ev.Name)
			continue
		}
		c.Env = append(c.Env, ev)
	}
}

// ensureCACert implements the per-spec ensure algorithm:
// target ns hit -> noop; otherwise copy from system ns; missing in system ns -> error.
func ensureCACert(ctx context.Context, cli client.Client, spec *CABundleSpec, targetNamespace string) error {
	// Step 1: short-circuit when the target Secret already exists.
	exists, err := secretExists(ctx, cli, targetNamespace, spec.SecretName)
	if err != nil {
		return err
	}
	if exists {
		klog.V(5).InfoS("CA secret already exists in target namespace, skipping copy",
			"name", spec.Name, "namespace", targetNamespace, "secret", spec.SecretName)
		return nil
	}

	// Step 2: fetch the authoritative copy from the system namespace.
	systemNamespace := utils.DefaultSandboxDeployNamespace
	var src corev1.Secret
	err = cli.Get(ctx, client.ObjectKey{Namespace: systemNamespace, Name: spec.SecretName}, &src)
	if errors.IsNotFound(err) {
		return fmt.Errorf("source CA secret %s/%s is missing; populate it before scheduling sandboxes",
			systemNamespace, spec.SecretName)
	}
	if err != nil {
		return fmt.Errorf("failed to read source CA secret %s/%s: %w",
			systemNamespace, spec.SecretName, err)
	}

	// Step 3: copy into the target namespace. AlreadyExists races are tolerated.
	dst := buildCopiedSecret(&src, targetNamespace)
	if err := cli.Create(ctx, dst); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create CA secret %s/%s: %w",
			targetNamespace, spec.SecretName, err)
	}
	klog.InfoS("CA secret replicated from system namespace",
		"name", spec.Name,
		"sourceNamespace", systemNamespace,
		"targetNamespace", targetNamespace,
		"secret", spec.SecretName)
	return nil
}

// secretExists reports whether the named Secret exists in the namespace.
func secretExists(ctx context.Context, cli client.Client, namespace, name string) (bool, error) {
	var secret corev1.Secret
	err := cli.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &secret)
	if err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to check secret %s/%s: %w", namespace, name, err)
	}
	return true, nil
}

// buildCopiedSecret produces a target-namespace Secret by copying Type and Data
// from the authoritative source. Source Labels are preserved as-is.
// Annotations are intentionally not copied (they tend to carry source-specific
// metadata such as last-applied-configuration). OwnerReferences are NOT set
// because cross-namespace OwnerReferences are invalid in Kubernetes.
func buildCopiedSecret(src *corev1.Secret, targetNamespace string) *corev1.Secret {
	labels := make(map[string]string, len(src.Labels))
	for k, v := range src.Labels {
		labels[k] = v
	}

	data := make(map[string][]byte, len(src.Data))
	for k, v := range src.Data {
		cp := make([]byte, len(v))
		copy(cp, v)
		data[k] = cp
	}

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      src.Name,
			Namespace: targetNamespace,
			Labels:    labels,
		},
		Type: src.Type,
		Data: data,
	}
}

// specEnabled returns true when the spec has no EnabledFor predicate, or when
// the predicate accepts the given sandbox.
func specEnabled(spec *CABundleSpec, sbx *agentsv1alpha1.Sandbox) bool {
	if spec.EnabledFor == nil {
		return true
	}
	return spec.EnabledFor(sbx)
}

// findVolumeByName reports whether a Volume with the given name already exists.
func findVolumeByName(volumes []corev1.Volume, name string) bool {
	for i := range volumes {
		if volumes[i].Name == name {
			return true
		}
	}
	return false
}

// findVolumeMountByName reports whether a VolumeMount with the given name
// already exists.
func findVolumeMountByName(volumeMounts []corev1.VolumeMount, name string) bool {
	for i := range volumeMounts {
		if volumeMounts[i].Name == name {
			return true
		}
	}
	return false
}

// findEnvVarByName reports whether an EnvVar with the given name already
// exists. Used to keep CA env-var injection idempotent and to avoid clobbering
// operator-supplied overrides.
func findEnvVarByName(envs []corev1.EnvVar, name string) bool {
	for i := range envs {
		if envs[i].Name == name {
			return true
		}
	}
	return false
}

func ptrTo[T any](v T) *T {
	return &v
}
