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
	// CABundleSpec registry. Use this constant when calling BindCAEnabledFor
	// from controller startup code.
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

// Labels appended to replicated Secrets to record their origin. They make the
// "this Secret was copied from sandbox-system" relationship discoverable via
// kubectl, while not asserting controller ownership (no OwnerReferences).
const (
	labelSourceNamespace       = "agents.kruise.io/source-namespace"
	labelSourceResourceVersion = "agents.kruise.io/source-resource-version"
)

// init registers the community baseline CABundleSpec for the gateway CA.
//
// EnabledFor is intentionally left nil here so the identity package does not
// depend on runtime-gating logic (e.g. sidecarutils.IsRuntimeEnabled). The
// controller startup code is expected to call BindCAEnabledFor(GatewayCABundleName, ...)
// to wire the runtime predicate.
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
		EnabledFor:        nil, // bound by controller startup
	})
}

// CACertInjector ensures CA bundle Secrets exist in the target namespace (by
// copying from utils.DefaultSandboxDeployNamespace) and injects the matching
// Volume + VolumeMount entries into a sandbox Pod.
//
// All decisions are driven by the CABundleSpec entries registered in the
// global registry, so adding a new CA bundle (e.g. an enterprise-internal one)
// only requires a new RegisterCABundleSpec call and no changes to this type.
//
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create
type CACertInjector struct {
	client.Client
	// systemNamespace is the authoritative source namespace from which CA
	// Secrets are copied on demand. It defaults to utils.DefaultSandboxDeployNamespace
	// ("sandbox-system") when constructed via NewCACertInjector.
	systemNamespace string
}

// NewCACertInjector creates a CACertInjector that reads CA Secrets from
// utils.DefaultSandboxDeployNamespace ("sandbox-system") as the authoritative
// source.
func NewCACertInjector(cli client.Client) *CACertInjector {
	return &CACertInjector{
		Client:          cli,
		systemNamespace: utils.DefaultSandboxDeployNamespace,
	}
}

// EnsureAllCACerts iterates every registered CABundleSpec and ensures the
// corresponding Secret exists in targetNamespace. For each spec:
//
//  1. EnabledFor (when non-nil) gates the spec on the given sandbox.
//  2. If the Secret already exists in targetNamespace, do nothing.
//  3. Otherwise the Secret is fetched from the system namespace and copied
//     into targetNamespace.
//  4. If the source Secret is missing in the system namespace, an error is
//     returned to block pod creation.
//
// Any error stops processing and is propagated to the caller; partial
// progress (Secrets already created earlier) is left in place since copies
// are idempotent.
func (inj *CACertInjector) EnsureAllCACerts(ctx context.Context, sbx *agentsv1alpha1.Sandbox, targetNamespace string) error {
	specs := ListCABundleSpecs()
	for i := range specs {
		spec := specs[i]
		if !specEnabled(&spec, sbx) {
			klog.V(5).InfoS("CABundleSpec disabled by EnabledFor predicate, skipping ensure",
				"name", spec.Name, "sandbox", klog.KObj(sbx))
			continue
		}
		if err := inj.ensureCACert(ctx, &spec, targetNamespace); err != nil {
			return fmt.Errorf("failed to ensure CA bundle %q: %w", spec.Name, err)
		}
	}
	return nil
}

// InjectAllCAVolumes appends one corev1.Volume per enabled CABundleSpec to
// pod.Spec.Volumes. Volumes whose Name already exists on the pod are skipped
// to keep the operation idempotent.
func (inj *CACertInjector) InjectAllCAVolumes(_ context.Context, sbx *agentsv1alpha1.Sandbox, pod *corev1.Pod) {
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

// InjectAllCAVolumeMounts appends a corev1.VolumeMount for every enabled
// CABundleSpec to each container that the spec's ContainerSelector matches.
// VolumeMounts whose Name already exists on a container are preserved
// untouched to keep the operation idempotent.
func (inj *CACertInjector) InjectAllCAVolumeMounts(_ context.Context, sbx *agentsv1alpha1.Sandbox, pod *corev1.Pod) {
	if len(pod.Spec.Containers) == 0 {
		klog.V(5).InfoS("no containers in pod, skipping CA volume mount injection",
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
			if findVolumeMountByName(c.VolumeMounts, spec.VolumeName) {
				klog.V(5).InfoS("CA volume mount already present on container, skipping",
					"name", spec.Name, "container", c.Name, "volume", spec.VolumeName)
				continue
			}
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
				Name:      spec.VolumeName,
				MountPath: spec.MountPath,
				SubPath:   spec.SubPath,
				ReadOnly:  spec.ReadOnly,
			})
		}
	}
}

// ensureCACert implements the per-spec ensure algorithm:
// target ns hit -> noop; otherwise copy from system ns; missing in system ns -> error.
func (inj *CACertInjector) ensureCACert(ctx context.Context, spec *CABundleSpec, targetNamespace string) error {
	// Step 1: short-circuit when the target Secret already exists.
	exists, err := inj.secretExists(ctx, targetNamespace, spec.SecretName)
	if err != nil {
		return err
	}
	if exists {
		klog.V(5).InfoS("CA secret already exists in target namespace, skipping copy",
			"name", spec.Name, "namespace", targetNamespace, "secret", spec.SecretName)
		return nil
	}

	// Step 2: fetch the authoritative copy from the system namespace.
	var src corev1.Secret
	err = inj.Client.Get(ctx, client.ObjectKey{Namespace: inj.systemNamespace, Name: spec.SecretName}, &src)
	if errors.IsNotFound(err) {
		return fmt.Errorf("source CA secret %s/%s is missing; populate it before scheduling sandboxes",
			inj.systemNamespace, spec.SecretName)
	}
	if err != nil {
		return fmt.Errorf("failed to read source CA secret %s/%s: %w",
			inj.systemNamespace, spec.SecretName, err)
	}

	// Step 3: copy into the target namespace. AlreadyExists races are tolerated.
	dst := buildCopiedSecret(&src, targetNamespace)
	if err := inj.Client.Create(ctx, dst); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create CA secret %s/%s: %w",
			targetNamespace, spec.SecretName, err)
	}
	klog.InfoS("CA secret replicated from system namespace",
		"name", spec.Name,
		"sourceNamespace", inj.systemNamespace,
		"targetNamespace", targetNamespace,
		"secret", spec.SecretName)
	return nil
}

// secretExists reports whether the named Secret exists in the namespace.
func (inj *CACertInjector) secretExists(ctx context.Context, namespace, name string) (bool, error) {
	var secret corev1.Secret
	err := inj.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &secret)
	if err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to check secret %s/%s: %w", namespace, name, err)
	}
	return true, nil
}

// buildCopiedSecret produces a target-namespace Secret by copying Type and Data
// from the authoritative source. Source Labels are preserved, then augmented
// with provenance labels. Annotations are intentionally not copied (they tend
// to carry source-specific metadata such as last-applied-configuration).
// OwnerReferences are NOT set because cross-namespace OwnerReferences are
// invalid in Kubernetes.
func buildCopiedSecret(src *corev1.Secret, targetNamespace string) *corev1.Secret {
	labels := make(map[string]string, len(src.Labels)+2)
	for k, v := range src.Labels {
		labels[k] = v
	}
	labels[labelSourceNamespace] = src.Namespace
	labels[labelSourceResourceVersion] = src.ResourceVersion

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

func ptrTo[T any](v T) *T {
	return &v
}
