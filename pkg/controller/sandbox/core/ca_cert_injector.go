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

package core

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/pkg/identity"
)

const (
	// gatewayCASecretName is the name of the Secret containing the gateway CA certificate.
	gatewayCASecretName = "sandbox-gateway-crt"

	// volumeNameGatewayCA is the volume name for the gateway CA certificate.
	volumeNameGatewayCA = "sandbox-gateway-ca"

	// GatewayCAKey is the data key for the gateway CA certificate in the Secret.
	GatewayCAKey = "sandbox-gateway-ca.crt"

	// mountPathGatewayCA is the mount path for the gateway CA certificate in the main container.
	mountPathGatewayCA = "/etc/ssl/certs/agent-identity/gateway-ca.crt"

	// subPathGatewayCA is the subPath for the gateway CA certificate volume mount.
	subPathGatewayCA = "sandbox-gateway-ca.crt"
)

// GatewayCACertInjector handles automatic gateway CA certificate injection into sandbox pods.
// It ensures each pod mounts the gateway CA certificate as a Secret volume from the namespace.
type GatewayCACertInjector struct {
	client.Client
}

// NewGatewayCACertInjector creates a new gateway CA certificate injector.
func NewGatewayCACertInjector(cli client.Client) *GatewayCACertInjector {
	return &GatewayCACertInjector{
		Client: cli,
	}
}

// EnsureGatewayCACert ensures the gateway CA certificate Secret exists in the given namespace.
// If the Secret does not exist, it creates one by fetching the CA bundle from the identity provider.
// Returns an error if Secret creation fails (blocks sandbox creation).
func (inj *GatewayCACertInjector) EnsureGatewayCACert(ctx context.Context, namespace string) error {
	if err := inj.ensureGatewayCASecret(ctx, namespace); err != nil {
		return fmt.Errorf("failed to ensure gateway CA secret: %w", err)
	}
	return nil
}

// InjectGatewayCAVolume appends the gateway CA certificate Secret volume to the pod spec.
func (inj *GatewayCACertInjector) InjectGatewayCAVolume(pod *corev1.Pod) {
	volume := corev1.Volume{
		Name: volumeNameGatewayCA,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  gatewayCASecretName,
				DefaultMode: ptrTo[int32](0644),
			},
		},
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes, volume)
}

// InjectGatewayCAVolumeMount appends the gateway CA certificate volume mount to the main container.
// The main container is identified as the first container in the pod spec,
// consistent with the sidecar injection logic in sidecarutils.
func (inj *GatewayCACertInjector) InjectGatewayCAVolumeMount(pod *corev1.Pod) {
	if len(pod.Spec.Containers) == 0 {
		klog.V(5).InfoS("no container found in pod, skipping gateway CA volume mount injection",
			"pod", klog.KObj(pod))
		return
	}

	mainContainer := &pod.Spec.Containers[0]
	inj.injectVolumeMountToContainer(mainContainer, volumeNameGatewayCA, mountPathGatewayCA, subPathGatewayCA)
}

// injectVolumeMountToContainer appends a CA certificate volume mount to the specified container.
// Skips injection if a volume mount with the same name already exists.
func (inj *GatewayCACertInjector) injectVolumeMountToContainer(container *corev1.Container, name, mountPath, subPath string) {
	if container.VolumeMounts == nil {
		container.VolumeMounts = make([]corev1.VolumeMount, 0, 1)
	}

	vm := corev1.VolumeMount{
		Name:      name,
		MountPath: mountPath,
		SubPath:   subPath,
		ReadOnly:  true,
	}

	if !findVolumeMountByName(container.VolumeMounts, vm.Name) {
		container.VolumeMounts = append(container.VolumeMounts, vm)
	}
}

// findVolumeMountByName checks if a volume mount with the given name exists.
func findVolumeMountByName(volumeMounts []corev1.VolumeMount, name string) bool {
	for _, vm := range volumeMounts {
		if vm.Name == name {
			return true
		}
	}
	return false
}

// ensureGatewayCASecret checks if the gateway CA Secret exists in the namespace.
// If not, fetches the CA bundle from the identity provider and creates the Secret.
func (inj *GatewayCACertInjector) ensureGatewayCASecret(ctx context.Context, namespace string) error {
	exists, err := inj.secretExists(ctx, namespace, gatewayCASecretName)
	if err != nil {
		return err
	}
	if exists {
		klog.V(5).InfoS("gateway CA secret already exists, skipping creation",
			"namespace", namespace, "secret", gatewayCASecretName)
		return nil
	}

	klog.InfoS("gateway CA secret not found, fetching from identity provider",
		"namespace", namespace, "secret", gatewayCASecretName)

	resp, err := identity.DefaultProvider.GetProxyCABundle(ctx, identity.GetProxyCABundleRequest{})
	if err != nil {
		return fmt.Errorf("failed to get gateway CA bundle from identity provider: %w", err)
	}
	if resp.CABundle == "" {
		return fmt.Errorf("identity provider returned empty gateway CA bundle")
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      gatewayCASecretName,
			Namespace: namespace,
			Labels: map[string]string{
				"agents.kruise.io/ca-type": "proxy",
			},
		},
		Data: map[string][]byte{
			GatewayCAKey: []byte(resp.CABundle),
		},
	}

	if err := inj.Client.Create(ctx, secret); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create gateway CA secret: %w", err)
	}
	klog.InfoS("gateway CA secret created successfully", "namespace", namespace, "secret", gatewayCASecretName)
	return nil
}

// secretExists checks if a Secret with the given name exists in the namespace.
func (inj *GatewayCACertInjector) secretExists(ctx context.Context, namespace, name string) (bool, error) {
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

func ptrTo[T any](v T) *T {
	return &v
}
