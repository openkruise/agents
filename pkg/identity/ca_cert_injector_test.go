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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

func init() {
	utilruntime.Must(agentsv1alpha1.AddToScheme(scheme.Scheme))
}

const (
	testTargetNS = "user-ns"
	testCAName   = "test-ca"
	testSecret   = "test-ca-secret"
	testDataKey  = "ca.crt"
	testVolume   = "test-ca-vol"
	testMount    = "/etc/ssl/certs/test-ca.crt"
)

// withTestSpec swaps the registry with a single deterministic spec for the
// duration of one test, restoring whatever was registered before on cleanup.
func withTestSpec(t *testing.T, spec CABundleSpec) {
	t.Helper()
	prev := ListCABundleSpecs()
	resetCABundleRegistryForTest()
	RegisterCABundleSpec(spec)
	t.Cleanup(func() {
		resetCABundleRegistryForTest()
		for i := range prev {
			RegisterCABundleSpec(prev[i])
		}
	})
}

func newTestSpec() CABundleSpec {
	return CABundleSpec{
		Name:              testCAName,
		SecretName:        testSecret,
		SecretDataKey:     testDataKey,
		VolumeName:        testVolume,
		MountPath:         testMount,
		SubPath:           testDataKey,
		ReadOnly:          true,
		ContainerSelector: OnlyMainContainer(),
	}
}

func newSandbox() *agentsv1alpha1.Sandbox {
	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-1", Namespace: testTargetNS},
	}
}

// --- registry tests --------------------------------------------------------

func TestRegisterCABundleSpec_Dedupe(t *testing.T) {
	resetCABundleRegistryForTest()
	t.Cleanup(resetCABundleRegistryForTest)

	RegisterCABundleSpec(CABundleSpec{Name: "a", SecretName: "old"})
	RegisterCABundleSpec(CABundleSpec{Name: "b", SecretName: "b"})
	RegisterCABundleSpec(CABundleSpec{Name: "a", SecretName: "new"})

	specs := ListCABundleSpecs()
	require.Len(t, specs, 2, "duplicate Name should overwrite, not append")

	byName := map[string]string{}
	for _, s := range specs {
		byName[s.Name] = s.SecretName
	}
	assert.Equal(t, "new", byName["a"])
	assert.Equal(t, "b", byName["b"])
}

func TestBindCAEnabledFor(t *testing.T) {
	resetCABundleRegistryForTest()
	t.Cleanup(resetCABundleRegistryForTest)

	RegisterCABundleSpec(CABundleSpec{Name: "a"})
	called := false
	BindCAEnabledFor("a", func(_ *agentsv1alpha1.Sandbox) bool {
		called = true
		return true
	})

	specs := ListCABundleSpecs()
	require.Len(t, specs, 1)
	require.NotNil(t, specs[0].EnabledFor)
	specs[0].EnabledFor(nil)
	assert.True(t, called)

	// no-op when name does not exist; should not panic.
	BindCAEnabledFor("missing", func(_ *agentsv1alpha1.Sandbox) bool { return true })
}

// --- selector tests --------------------------------------------------------

func TestContainerSelectors(t *testing.T) {
	c0 := &corev1.Container{Name: "main"}
	c1 := &corev1.Container{Name: "sidecar"}

	t.Run("OnlyMainContainer matches index 0", func(t *testing.T) {
		sel := OnlyMainContainer()
		assert.True(t, sel(c0, 0))
		assert.False(t, sel(c1, 1))
		// Calling again must still report index-0 as the main container,
		// proving the selector is stateless across invocations.
		assert.True(t, sel(c0, 0))
	})

	t.Run("ByContainerName matches by name", func(t *testing.T) {
		sel := ByContainerName("sidecar")
		assert.False(t, sel(c0, 0))
		assert.True(t, sel(c1, 1))
	})

	t.Run("AllContainers matches every container", func(t *testing.T) {
		sel := AllContainers()
		assert.True(t, sel(c0, 0))
		assert.True(t, sel(c1, 1))
	})
}

// --- EnsureAllCACerts tests -----------------------------------------------

func TestCACertInjector_EnsureAllCACerts(t *testing.T) {
	systemNS := utils.DefaultSandboxDeployNamespace
	srcSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            testSecret,
			Namespace:       systemNS,
			Labels:          map[string]string{"foo": "bar"},
			ResourceVersion: "42",
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{testDataKey: []byte("PEM-CA")},
	}

	tests := []struct {
		name        string
		seed        []client.Object
		spec        CABundleSpec
		sandbox     *agentsv1alpha1.Sandbox
		getError    error
		expectError string
		check       func(t *testing.T, cli client.Client)
	}{
		{
			name: "target namespace already has secret - skip copy",
			seed: []client.Object{
				&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: testSecret, Namespace: testTargetNS},
					Data:       map[string][]byte{testDataKey: []byte("local")},
				},
				srcSecret,
			},
			spec:    newTestSpec(),
			sandbox: newSandbox(),
			check: func(t *testing.T, cli client.Client) {
				var got corev1.Secret
				require.NoError(t, cli.Get(context.Background(),
					client.ObjectKey{Namespace: testTargetNS, Name: testSecret}, &got))
				// Pre-existing local copy must be preserved untouched.
				assert.Equal(t, []byte("local"), got.Data[testDataKey])
				assert.NotContains(t, got.Labels, labelSourceNamespace)
			},
		},
		{
			name:    "target missing - copy from system namespace",
			seed:    []client.Object{srcSecret},
			spec:    newTestSpec(),
			sandbox: newSandbox(),
			check: func(t *testing.T, cli client.Client) {
				var got corev1.Secret
				require.NoError(t, cli.Get(context.Background(),
					client.ObjectKey{Namespace: testTargetNS, Name: testSecret}, &got))
				assert.Equal(t, []byte("PEM-CA"), got.Data[testDataKey])
				assert.Equal(t, corev1.SecretTypeOpaque, got.Type)
				assert.Equal(t, "bar", got.Labels["foo"], "source labels should be preserved")
				assert.Equal(t, systemNS, got.Labels[labelSourceNamespace])
				assert.Equal(t, "42", got.Labels[labelSourceResourceVersion])
				assert.Empty(t, got.OwnerReferences, "cross-namespace owner refs are forbidden")
			},
		},
		{
			name:        "source missing in system namespace - block with error",
			seed:        []client.Object{},
			spec:        newTestSpec(),
			sandbox:     newSandbox(),
			expectError: "source CA secret",
		},
		{
			name: "EnabledFor returns false - skip entirely",
			seed: []client.Object{}, // even with no source secret it must not error
			spec: func() CABundleSpec {
				s := newTestSpec()
				s.EnabledFor = func(_ *agentsv1alpha1.Sandbox) bool { return false }
				return s
			}(),
			sandbox: newSandbox(),
			check: func(t *testing.T, cli client.Client) {
				var got corev1.Secret
				err := cli.Get(context.Background(),
					client.ObjectKey{Namespace: testTargetNS, Name: testSecret}, &got)
				assert.True(t, apierrors.IsNotFound(err), "no secret should be created")
			},
		},
		{
			name:        "transient API error reading source - propagate",
			seed:        []client.Object{srcSecret},
			spec:        newTestSpec(),
			sandbox:     newSandbox(),
			getError:    errors.New("api boom"),
			expectError: "api boom",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withTestSpec(t, tt.spec)

			builder := fake.NewClientBuilder().
				WithScheme(scheme.Scheme).
				WithObjects(tt.seed...)
			if tt.getError != nil {
				wantErr := tt.getError
				builder = builder.WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						// Only fail when reading the source Secret in the system namespace.
						if key.Namespace == utils.DefaultSandboxDeployNamespace && key.Name == testSecret {
							if _, ok := obj.(*corev1.Secret); ok {
								return wantErr
							}
						}
						return c.Get(ctx, key, obj, opts...)
					},
				})
			}
			cli := builder.Build()

			injector := NewCACertInjector(cli)
			err := injector.EnsureAllCACerts(context.Background(), tt.sandbox, testTargetNS)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			if tt.check != nil {
				tt.check(t, cli)
			}
		})
	}
}

func TestCACertInjector_EnsureAllCACerts_AlreadyExistsTolerated(t *testing.T) {
	systemNS := utils.DefaultSandboxDeployNamespace
	withTestSpec(t, newTestSpec())

	src := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testSecret, Namespace: systemNS},
		Data:       map[string][]byte{testDataKey: []byte("PEM")},
	}
	cli := fake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(src).
		WithInterceptorFuncs(interceptor.Funcs{
			// Simulate a concurrent creator winning the race in target ns.
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if s, ok := obj.(*corev1.Secret); ok && s.Namespace == testTargetNS && s.Name == testSecret {
					return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, testSecret)
				}
				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()

	injector := NewCACertInjector(cli)
	err := injector.EnsureAllCACerts(context.Background(), newSandbox(), testTargetNS)
	require.NoError(t, err, "AlreadyExists must be tolerated as a benign race")
}

// --- InjectAllCAVolumes / InjectAllCAVolumeMounts tests --------------------

func TestCACertInjector_InjectAllCAVolumes(t *testing.T) {
	tests := []struct {
		name        string
		spec        CABundleSpec
		initial     []corev1.Volume
		sandbox     *agentsv1alpha1.Sandbox
		expectNames []string
	}{
		{
			name:        "inject into empty volumes",
			spec:        newTestSpec(),
			sandbox:     newSandbox(),
			expectNames: []string{testVolume},
		},
		{
			name: "preserve existing volumes and append once",
			spec: newTestSpec(),
			initial: []corev1.Volume{
				{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
			sandbox:     newSandbox(),
			expectNames: []string{"data", testVolume},
		},
		{
			name: "skip when volume name already present",
			spec: newTestSpec(),
			initial: []corev1.Volume{
				{Name: testVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
			sandbox:     newSandbox(),
			expectNames: []string{testVolume},
		},
		{
			name: "EnabledFor false - no injection",
			spec: func() CABundleSpec {
				s := newTestSpec()
				s.EnabledFor = func(_ *agentsv1alpha1.Sandbox) bool { return false }
				return s
			}(),
			sandbox:     newSandbox(),
			expectNames: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withTestSpec(t, tt.spec)

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: testTargetNS},
				Spec:       corev1.PodSpec{Volumes: tt.initial},
			}
			injector := NewCACertInjector(fake.NewClientBuilder().WithScheme(scheme.Scheme).Build())
			injector.InjectAllCAVolumes(context.Background(), tt.sandbox, pod)

			gotNames := make([]string, 0, len(pod.Spec.Volumes))
			for _, v := range pod.Spec.Volumes {
				gotNames = append(gotNames, v.Name)
			}
			if tt.expectNames == nil {
				assert.Empty(t, gotNames)
			} else {
				assert.Equal(t, tt.expectNames, gotNames)
			}
		})
	}
}

func TestCACertInjector_InjectAllCAVolumeMounts(t *testing.T) {
	tests := []struct {
		name             string
		spec             CABundleSpec
		containers       []corev1.Container
		sandbox          *agentsv1alpha1.Sandbox
		expectMountsByIx map[int][]string
	}{
		{
			name:       "OnlyMainContainer mounts only on first container",
			spec:       newTestSpec(),
			containers: []corev1.Container{{Name: "main"}, {Name: "sidecar"}},
			sandbox:    newSandbox(),
			expectMountsByIx: map[int][]string{
				0: {testVolume},
				1: nil,
			},
		},
		{
			name: "AllContainers mounts on every container",
			spec: func() CABundleSpec {
				s := newTestSpec()
				s.ContainerSelector = AllContainers()
				return s
			}(),
			containers: []corev1.Container{{Name: "main"}, {Name: "sidecar"}},
			sandbox:    newSandbox(),
			expectMountsByIx: map[int][]string{
				0: {testVolume},
				1: {testVolume},
			},
		},
		{
			name: "ByContainerName targets specific container",
			spec: func() CABundleSpec {
				s := newTestSpec()
				s.ContainerSelector = ByContainerName("sidecar")
				return s
			}(),
			containers: []corev1.Container{{Name: "main"}, {Name: "sidecar"}},
			sandbox:    newSandbox(),
			expectMountsByIx: map[int][]string{
				0: nil,
				1: {testVolume},
			},
		},
		{
			name: "skip container whose mount name already exists",
			spec: newTestSpec(),
			containers: []corev1.Container{
				{
					Name: "main",
					VolumeMounts: []corev1.VolumeMount{
						{Name: testVolume, MountPath: "/old"},
					},
				},
			},
			sandbox: newSandbox(),
			expectMountsByIx: map[int][]string{
				0: {testVolume},
			},
		},
		{
			name: "EnabledFor false - no mounts at all",
			spec: func() CABundleSpec {
				s := newTestSpec()
				s.EnabledFor = func(_ *agentsv1alpha1.Sandbox) bool { return false }
				return s
			}(),
			containers: []corev1.Container{{Name: "main"}},
			sandbox:    newSandbox(),
			expectMountsByIx: map[int][]string{
				0: nil,
			},
		},
		{
			name:             "no containers - graceful skip",
			spec:             newTestSpec(),
			containers:       nil,
			sandbox:          newSandbox(),
			expectMountsByIx: map[int][]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withTestSpec(t, tt.spec)

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: testTargetNS},
				Spec:       corev1.PodSpec{Containers: tt.containers},
			}
			injector := NewCACertInjector(fake.NewClientBuilder().WithScheme(scheme.Scheme).Build())
			injector.InjectAllCAVolumeMounts(context.Background(), tt.sandbox, pod)

			for idx, want := range tt.expectMountsByIx {
				gotNames := make([]string, 0)
				if idx < len(pod.Spec.Containers) {
					for _, vm := range pod.Spec.Containers[idx].VolumeMounts {
						gotNames = append(gotNames, vm.Name)
					}
				}
				if want == nil {
					assert.Empty(t, gotNames, "container[%d] should have no expected mounts", idx)
				} else {
					assert.Equal(t, want, gotNames, "container[%d] mounts mismatch", idx)
				}
			}
		})
	}
}

// --- buildCopiedSecret unit test -------------------------------------------

func TestBuildCopiedSecret(t *testing.T) {
	src := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "src",
			Namespace:       "src-ns",
			Labels:          map[string]string{"k": "v"},
			Annotations:     map[string]string{"ignored": "yes"},
			ResourceVersion: "7",
			OwnerReferences: []metav1.OwnerReference{{Name: "owner"}},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"a": []byte("b")},
	}

	dst := buildCopiedSecret(src, "dst-ns")

	assert.Equal(t, "src", dst.Name)
	assert.Equal(t, "dst-ns", dst.Namespace)
	assert.Equal(t, corev1.SecretTypeOpaque, dst.Type)
	assert.Equal(t, []byte("b"), dst.Data["a"])
	assert.Equal(t, "v", dst.Labels["k"])
	assert.Equal(t, "src-ns", dst.Labels[labelSourceNamespace])
	assert.Equal(t, "7", dst.Labels[labelSourceResourceVersion])
	assert.Empty(t, dst.Annotations, "annotations must not be copied")
	assert.Empty(t, dst.OwnerReferences, "owner references must not be carried across namespaces")

	// Mutating dst.Data must not affect src.
	dst.Data["a"][0] = 'X'
	assert.Equal(t, byte('b'), src.Data["a"][0])
}
