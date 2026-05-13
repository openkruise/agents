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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
)

// mockIdentityProvider implements identity.IdentityProvider for testing.
type mockIdentityProvider struct {
	getCABundleFunc func(ctx context.Context, req identity.GetProxyCABundleRequest) (*identity.GetProxyCABundleResponse, error)
}

func (m *mockIdentityProvider) IssueToken(ctx context.Context, req identity.TokenRequest) (*identity.TokenResponse, error) {
	return nil, nil
}

func (m *mockIdentityProvider) PropagateSecurityToken(ctx context.Context, sbx *agentsv1alpha1.Sandbox, tokenResp *identity.TokenResponse) error {
	return nil
}

func (m *mockIdentityProvider) GetProxyCABundle(ctx context.Context, req identity.GetProxyCABundleRequest) (*identity.GetProxyCABundleResponse, error) {
	if m.getCABundleFunc != nil {
		return m.getCABundleFunc(ctx, req)
	}
	return &identity.GetProxyCABundleResponse{CABundle: "test-proxy-ca-bundle"}, nil
}

func TestGatewayCACertInjector_EnsureGatewayCACert(t *testing.T) {
	const testNamespace = "test-ns"
	tests := []struct {
		name            string
		existingSecrets []client.Object
		proxyCABundle   string
		providerErr     string
		expectError     string
		checkSecret     func(t *testing.T, cli client.Client)
	}{
		{
			name: "secret already exists - no creation needed",
			existingSecrets: []client.Object{
				&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: gatewayCASecretName, Namespace: testNamespace},
					Data:       map[string][]byte{GatewayCAKey: []byte("old-gateway-ca")},
				},
			},
			proxyCABundle: "new-gateway-ca",
			checkSecret: func(t *testing.T, cli client.Client) {
				var secret corev1.Secret
				err := cli.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: gatewayCASecretName}, &secret)
				require.NoError(t, err)
				assert.Equal(t, []byte("old-gateway-ca"), secret.Data[GatewayCAKey])
			},
		},
		{
			name:          "secret does not exist - create from identity provider",
			existingSecrets: []client.Object{},
			proxyCABundle: "test-gateway-ca-content",
			checkSecret: func(t *testing.T, cli client.Client) {
				var secret corev1.Secret
				err := cli.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: gatewayCASecretName}, &secret)
				require.NoError(t, err)
				assert.Equal(t, []byte("test-gateway-ca-content"), secret.Data[GatewayCAKey])
				assert.Equal(t, "proxy", secret.Labels["agents.kruise.io/ca-type"])
			},
		},
		{
			name: "identity provider returns error - should block",
			existingSecrets: []client.Object{},
			proxyCABundle: "",
			providerErr:   "identity provider unreachable",
			expectError:   "failed to get gateway CA bundle",
		},
		{
			name:          "identity provider returns empty CA bundle - should block",
			existingSecrets: []client.Object{},
			proxyCABundle: "",
			expectError:   "empty gateway CA bundle",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Mock the identity provider's GetProxyCABundle
			originalProvider := identity.DefaultProvider
			defer func() { identity.DefaultProvider = originalProvider }()

			identity.DefaultProvider = &mockIdentityProvider{
				getCABundleFunc: func(_ context.Context, _ identity.GetProxyCABundleRequest) (*identity.GetProxyCABundleResponse, error) {
					if tt.providerErr != "" {
						return nil, assert.AnError
					}
					return &identity.GetProxyCABundleResponse{CABundle: tt.proxyCABundle}, nil
				},
			}

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(tt.existingSecrets...).
				Build()

			injector := NewGatewayCACertInjector(fakeClient)
			err := injector.EnsureGatewayCACert(context.Background(), testNamespace)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			tt.checkSecret(t, fakeClient)
		})
	}
}

func TestGatewayCACertInjector_InjectGatewayCAVolume(t *testing.T) {
	tests := []struct {
		name           string
		initialVolumes []corev1.Volume
		expectCount    int
		checkVolume    func(t *testing.T, volumes []corev1.Volume)
	}{
		{
			name:           "inject into pod with no existing volumes",
			initialVolumes: nil,
			expectCount:    1,
			checkVolume: func(t *testing.T, volumes []corev1.Volume) {
				assert.Equal(t, volumeNameGatewayCA, volumes[0].Name)
				assert.NotNil(t, volumes[0].Secret)
				assert.Equal(t, gatewayCASecretName, volumes[0].Secret.SecretName)
			},
		},
		{
			name: "inject into pod with existing volumes",
			initialVolumes: []corev1.Volume{
				{Name: "existing-vol", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
			expectCount: 2,
			checkVolume: func(t *testing.T, volumes []corev1.Volume) {
				assert.Equal(t, "existing-vol", volumes[0].Name)
				assert.Equal(t, volumeNameGatewayCA, volumes[1].Name)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			injector := NewGatewayCACertInjector(nil)

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
				Spec: corev1.PodSpec{
					Volumes: tt.initialVolumes,
				},
			}

			injector.InjectGatewayCAVolume(pod)

			assert.Len(t, pod.Spec.Volumes, tt.expectCount)
			tt.checkVolume(t, pod.Spec.Volumes)
		})
	}
}

func TestGatewayCACertInjector_InjectGatewayCAVolumeMount(t *testing.T) {
	tests := []struct {
		name              string
		initialContainers []corev1.Container
		expectMountCount  int
		checkVolumeMounts func(t *testing.T, mounts []corev1.VolumeMount)
	}{
		{
			name:              "inject into pod with no existing volume mounts",
			initialContainers: []corev1.Container{{Name: "main", Image: "nginx"}},
			expectMountCount:  1,
			checkVolumeMounts: func(t *testing.T, mounts []corev1.VolumeMount) {
				assert.Equal(t, volumeNameGatewayCA, mounts[0].Name)
				assert.Equal(t, mountPathGatewayCA, mounts[0].MountPath)
				assert.Equal(t, subPathGatewayCA, mounts[0].SubPath)
				assert.True(t, mounts[0].ReadOnly)
			},
		},
		{
			name: "inject into pod with existing volume mounts",
			initialContainers: []corev1.Container{
				{
					Name:  "main",
					Image: "nginx",
					VolumeMounts: []corev1.VolumeMount{
						{Name: "existing-mount", MountPath: "/data"},
					},
				},
			},
			expectMountCount: 2,
			checkVolumeMounts: func(t *testing.T, mounts []corev1.VolumeMount) {
				assert.Equal(t, "existing-mount", mounts[0].Name)
				assert.Equal(t, volumeNameGatewayCA, mounts[1].Name)
			},
		},
		{
			name: "skip injection when volume mount already exists",
			initialContainers: []corev1.Container{
				{
					Name:  "main",
					Image: "nginx",
					VolumeMounts: []corev1.VolumeMount{
						{Name: volumeNameGatewayCA, MountPath: "/old/path"},
					},
				},
			},
			expectMountCount: 1,
			checkVolumeMounts: func(t *testing.T, mounts []corev1.VolumeMount) {
				// Existing mount should be preserved (not overwritten)
				assert.Equal(t, volumeNameGatewayCA, mounts[0].Name)
				assert.Equal(t, "/old/path", mounts[0].MountPath)
			},
		},
		{
			name:              "no containers - skip injection",
			initialContainers: []corev1.Container{},
			expectMountCount:  0,
			checkVolumeMounts: func(t *testing.T, mounts []corev1.VolumeMount) {
				assert.Nil(t, mounts)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			injector := NewGatewayCACertInjector(nil)

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
				Spec: corev1.PodSpec{
					Containers: tt.initialContainers,
				},
			}

			injector.InjectGatewayCAVolumeMount(pod)

			if len(tt.initialContainers) > 0 {
				assert.Len(t, pod.Spec.Containers[0].VolumeMounts, tt.expectMountCount)
				tt.checkVolumeMounts(t, pod.Spec.Containers[0].VolumeMounts)
			} else {
				tt.checkVolumeMounts(t, nil)
			}
		})
	}
}

func TestGatewayCACertInjector_secretExists(t *testing.T) {
	tests := []struct {
		name            string
		existingSecrets []client.Object
		secretName      string
		expectExists    bool
		expectError     string
	}{
		{
			name: "secret exists",
			existingSecrets: []client.Object{
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "my-secret", Namespace: "default"}},
			},
			secretName:   "my-secret",
			expectExists: true,
		},
		{
			name:            "secret does not exist",
			existingSecrets: []client.Object{},
			secretName:      "missing-secret",
			expectExists:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(tt.existingSecrets...).
				Build()

			injector := NewGatewayCACertInjector(fakeClient)
			exists, err := injector.secretExists(context.Background(), "default", tt.secretName)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expectExists, exists)
		})
	}
}

func TestGatewayCACertInjector_EnsureGatewayCACert_ConcurrentCreate(t *testing.T) {
	// Test that concurrent creation attempts don't cause errors
	// (IsAlreadyExists is handled gracefully)
	// Mock the identity provider
	originalProvider := identity.DefaultProvider
	defer func() { identity.DefaultProvider = originalProvider }()
	identity.DefaultProvider = &mockIdentityProvider{
		getCABundleFunc: func(_ context.Context, _ identity.GetProxyCABundleRequest) (*identity.GetProxyCABundleResponse, error) {
			return &identity.GetProxyCABundleResponse{CABundle: "test-gateway-ca"}, nil
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: gatewayCASecretName, Namespace: "default"}},
		).
		Build()

	injector := NewGatewayCACertInjector(fakeClient)
	err := injector.EnsureGatewayCACert(context.Background(), "default")
	require.NoError(t, err)
}
