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
	"reflect"
	"strings"
	"testing"

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
	prev := ListCABundleSpecs()
	resetCABundleRegistryForTest()
	t.Cleanup(func() {
		resetCABundleRegistryForTest()
		for i := range prev {
			RegisterCABundleSpec(prev[i])
		}
	})

	RegisterCABundleSpec(CABundleSpec{Name: "a", SecretName: "old"})
	RegisterCABundleSpec(CABundleSpec{Name: "b", SecretName: "b"})
	RegisterCABundleSpec(CABundleSpec{Name: "a", SecretName: "new"})

	specs := ListCABundleSpecs()
	if len(specs) != 2 {
		t.Fatalf("duplicate Name should overwrite, not append: got %d specs, want 2", len(specs))
	}

	byName := map[string]string{}
	for _, s := range specs {
		byName[s.Name] = s.SecretName
	}
	if byName["a"] != "new" {
		t.Errorf("spec %q SecretName: got %q, want %q", "a", byName["a"], "new")
	}
	if byName["b"] != "b" {
		t.Errorf("spec %q SecretName: got %q, want %q", "b", byName["b"], "b")
	}
}

func TestBindCAEnabledFor(t *testing.T) {
	prev := ListCABundleSpecs()
	resetCABundleRegistryForTest()
	t.Cleanup(func() {
		resetCABundleRegistryForTest()
		for i := range prev {
			RegisterCABundleSpec(prev[i])
		}
	})

	RegisterCABundleSpec(CABundleSpec{Name: "a"})
	called := false
	BindCAEnabledFor("a", func(_ *agentsv1alpha1.Sandbox) bool {
		called = true
		return true
	})

	specs := ListCABundleSpecs()
	if len(specs) != 1 {
		t.Fatalf("expected exactly 1 spec, got %d", len(specs))
	}
	if specs[0].EnabledFor == nil {
		t.Fatalf("expected EnabledFor to be bound, got nil")
	}
	specs[0].EnabledFor(nil)
	if !called {
		t.Errorf("bound EnabledFor predicate was not invoked")
	}

	// no-op when name does not exist; should not panic.
	BindCAEnabledFor("missing", func(_ *agentsv1alpha1.Sandbox) bool { return true })
}

// --- selector tests --------------------------------------------------------

func TestContainerSelectors(t *testing.T) {
	c0 := &corev1.Container{Name: "main"}
	c1 := &corev1.Container{Name: "sidecar"}

	t.Run("OnlyMainContainer matches index 0", func(t *testing.T) {
		sel := OnlyMainContainer()
		if !sel(c0, 0) {
			t.Errorf("OnlyMainContainer should match index 0")
		}
		if sel(c1, 1) {
			t.Errorf("OnlyMainContainer must not match index 1")
		}
		// Calling again must still report index-0 as the main container,
		// proving the selector is stateless across invocations.
		if !sel(c0, 0) {
			t.Errorf("OnlyMainContainer must remain stateless across invocations")
		}
	})

	t.Run("ByContainerName matches by name", func(t *testing.T) {
		sel := ByContainerName("sidecar")
		if sel(c0, 0) {
			t.Errorf("ByContainerName(%q) must not match container %q", "sidecar", c0.Name)
		}
		if !sel(c1, 1) {
			t.Errorf("ByContainerName(%q) should match container %q", "sidecar", c1.Name)
		}
	})

	t.Run("MainContainerOrByName matches main and named sidecars", func(t *testing.T) {
		sel := MainContainerOrByName("sidecar")
		// Main container always matches regardless of its Name.
		if !sel(c0, 0) {
			t.Errorf("MainContainerOrByName should match main container at index 0")
		}
		// Named sidecar matches via the allow-list.
		if !sel(c1, 1) {
			t.Errorf("MainContainerOrByName should match allow-listed sidecar")
		}
		// Unrelated sidecar at non-zero index must not match.
		if sel(&corev1.Container{Name: "metrics"}, 2) {
			t.Errorf("MainContainerOrByName must not match unrelated sidecar")
		}
	})

	t.Run("MainContainerOrByName with empty allow-list degenerates to main only", func(t *testing.T) {
		sel := MainContainerOrByName()
		if !sel(c0, 0) {
			t.Errorf("MainContainerOrByName() should still match main container")
		}
		if sel(c1, 1) {
			t.Errorf("MainContainerOrByName() must not match non-main containers")
		}
	})

	t.Run("AllContainers matches every container", func(t *testing.T) {
		sel := AllContainers()
		if !sel(c0, 0) {
			t.Errorf("AllContainers should match index 0")
		}
		if !sel(c1, 1) {
			t.Errorf("AllContainers should match index 1")
		}
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
				if err := cli.Get(context.Background(),
					client.ObjectKey{Namespace: testTargetNS, Name: testSecret}, &got); err != nil {
					t.Fatalf("get target secret: %v", err)
				}
				// Pre-existing local copy must be preserved untouched.
				if !reflect.DeepEqual(got.Data[testDataKey], []byte("local")) {
					t.Errorf("target secret data: got %q, want %q", got.Data[testDataKey], "local")
				}
			},
		},
		{
			name:    "target missing - copy from system namespace",
			seed:    []client.Object{srcSecret},
			spec:    newTestSpec(),
			sandbox: newSandbox(),
			check: func(t *testing.T, cli client.Client) {
				var got corev1.Secret
				if err := cli.Get(context.Background(),
					client.ObjectKey{Namespace: testTargetNS, Name: testSecret}, &got); err != nil {
					t.Fatalf("get copied secret: %v", err)
				}
				if !reflect.DeepEqual(got.Data[testDataKey], []byte("PEM-CA")) {
					t.Errorf("copied data: got %q, want %q", got.Data[testDataKey], "PEM-CA")
				}
				if got.Type != corev1.SecretTypeOpaque {
					t.Errorf("copied type: got %q, want %q", got.Type, corev1.SecretTypeOpaque)
				}
				if got.Labels["foo"] != "bar" {
					t.Errorf("source labels should be preserved, got %q", got.Labels["foo"])
				}
				if len(got.OwnerReferences) != 0 {
					t.Errorf("cross-namespace owner refs are forbidden, got %d entries", len(got.OwnerReferences))
				}
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
				if !apierrors.IsNotFound(err) {
					t.Errorf("no secret should be created when EnabledFor=false, got err=%v", err)
				}
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

			err := EnsureAllCACerts(context.Background(), cli, tt.sandbox, testTargetNS)

			if tt.expectError != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.expectError)
				}
				if !strings.Contains(err.Error(), tt.expectError) {
					t.Fatalf("expected error containing %q, got %q", tt.expectError, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("EnsureAllCACerts unexpected error: %v", err)
			}
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

	injectorErr := EnsureAllCACerts(context.Background(), cli, newSandbox(), testTargetNS)
	if injectorErr != nil {
		t.Fatalf("AlreadyExists must be tolerated as a benign race, got error: %v", injectorErr)
	}
}

// --- InjectAllCAVolumes / InjectAllCAIntoContainers tests ------------------

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
			InjectAllCAVolumes(context.Background(), tt.sandbox, pod)

			gotNames := make([]string, 0, len(pod.Spec.Volumes))
			for _, v := range pod.Spec.Volumes {
				gotNames = append(gotNames, v.Name)
			}
			if tt.expectNames == nil {
				if len(gotNames) != 0 {
					t.Errorf("expected no volumes, got %v", gotNames)
				}
			} else if !reflect.DeepEqual(gotNames, tt.expectNames) {
				t.Errorf("volume names: got %v, want %v", gotNames, tt.expectNames)
			}
		})
	}
}

// TestCACertInjector_InjectAllCAIntoContainers_Mounts validates the mount
// half of the merged container-level injection: VolumeMount placement is
// driven by specEnabled + ContainerSelector, with idempotent skip when a
// mount with the same Name is already present.
func TestCACertInjector_InjectAllCAIntoContainers_Mounts(t *testing.T) {
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
			InjectAllCAIntoContainers(context.Background(), tt.sandbox, pod)

			for idx, want := range tt.expectMountsByIx {
				gotNames := make([]string, 0)
				if idx < len(pod.Spec.Containers) {
					for _, vm := range pod.Spec.Containers[idx].VolumeMounts {
						gotNames = append(gotNames, vm.Name)
					}
				}
				if want == nil {
					if len(gotNames) != 0 {
						t.Errorf("container[%d] should have no mounts, got %v", idx, gotNames)
					}
				} else if !reflect.DeepEqual(gotNames, want) {
					t.Errorf("container[%d] mounts mismatch: got %v, want %v", idx, gotNames, want)
				}
			}
		})
	}
}

// --- InjectAllCAIntoContainers env-var coverage ----------------------------

// TestCACertInjector_InjectAllCAIntoContainers_EnvVars validates the env-var
// half of the merged container-level injection: EnvVars are appended only to
// containers that the selector matches, are skipped when the spec declares
// none, and operator-supplied entries are preserved across repeated calls.
func TestCACertInjector_InjectAllCAIntoContainers_EnvVars(t *testing.T) {
	envFile := corev1.EnvVar{Name: "SSL_CERT_FILE", Value: testMount}
	envExtra := corev1.EnvVar{Name: "REQUESTS_CA_BUNDLE", Value: testMount}

	tests := []struct {
		name          string
		spec          CABundleSpec
		containers    []corev1.Container
		sandbox       *agentsv1alpha1.Sandbox
		expectEnvByIx map[int][]string // container index -> ordered env-var names
	}{
		{
			name: "no EnvVars on spec - skip injection",
			spec: func() CABundleSpec {
				s := newTestSpec()
				s.EnvVars = nil
				return s
			}(),
			containers: []corev1.Container{{Name: "main"}, {Name: "sidecar"}},
			sandbox:    newSandbox(),
			expectEnvByIx: map[int][]string{
				0: nil,
				1: nil,
			},
		},
		{
			name: "OnlyMainContainer injects on first container only",
			spec: func() CABundleSpec {
				s := newTestSpec()
				s.EnvVars = []corev1.EnvVar{envFile, envExtra}
				return s
			}(),
			containers: []corev1.Container{{Name: "main"}, {Name: "sidecar"}},
			sandbox:    newSandbox(),
			expectEnvByIx: map[int][]string{
				0: {"SSL_CERT_FILE", "REQUESTS_CA_BUNDLE"},
				1: nil,
			},
		},
		{
			name: "MainContainerOrByName injects on main and named sidecar",
			spec: func() CABundleSpec {
				s := newTestSpec()
				s.ContainerSelector = MainContainerOrByName("sidecar")
				s.EnvVars = []corev1.EnvVar{envFile}
				return s
			}(),
			containers: []corev1.Container{{Name: "main"}, {Name: "sidecar"}, {Name: "metrics"}},
			sandbox:    newSandbox(),
			expectEnvByIx: map[int][]string{
				0: {"SSL_CERT_FILE"},
				1: {"SSL_CERT_FILE"},
				2: nil,
			},
		},
		{
			name: "operator-supplied env name is preserved (idempotent)",
			spec: func() CABundleSpec {
				s := newTestSpec()
				s.EnvVars = []corev1.EnvVar{envFile, envExtra}
				return s
			}(),
			containers: []corev1.Container{{
				Name: "main",
				Env: []corev1.EnvVar{
					{Name: "SSL_CERT_FILE", Value: "/operator/own.crt"},
				},
			}},
			sandbox: newSandbox(),
			// Existing SSL_CERT_FILE keeps the operator value (still appears
			// first in the slice); REQUESTS_CA_BUNDLE is appended.
			expectEnvByIx: map[int][]string{
				0: {"SSL_CERT_FILE", "REQUESTS_CA_BUNDLE"},
			},
		},
		{
			name: "EnabledFor false - no env injection",
			spec: func() CABundleSpec {
				s := newTestSpec()
				s.EnvVars = []corev1.EnvVar{envFile}
				s.EnabledFor = func(_ *agentsv1alpha1.Sandbox) bool { return false }
				return s
			}(),
			containers: []corev1.Container{{Name: "main"}},
			sandbox:    newSandbox(),
			expectEnvByIx: map[int][]string{
				0: nil,
			},
		},
		{
			name:          "no containers - graceful skip",
			spec:          newTestSpec(),
			containers:    nil,
			sandbox:       newSandbox(),
			expectEnvByIx: map[int][]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withTestSpec(t, tt.spec)

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: testTargetNS},
				Spec:       corev1.PodSpec{Containers: tt.containers},
			}
			InjectAllCAIntoContainers(context.Background(), tt.sandbox, pod)

			for idx, want := range tt.expectEnvByIx {
				gotNames := make([]string, 0)
				if idx < len(pod.Spec.Containers) {
					for _, ev := range pod.Spec.Containers[idx].Env {
						gotNames = append(gotNames, ev.Name)
					}
				}
				if want == nil {
					if len(gotNames) != 0 {
						t.Errorf("container[%d] should have no env vars, got %v", idx, gotNames)
					}
				} else if !reflect.DeepEqual(gotNames, want) {
					t.Errorf("container[%d] env vars mismatch: got %v, want %v", idx, gotNames, want)
				}
			}

			// Idempotence: a second invocation must not duplicate env vars.
			InjectAllCAIntoContainers(context.Background(), tt.sandbox, pod)
			for idx, want := range tt.expectEnvByIx {
				if idx >= len(pod.Spec.Containers) {
					continue
				}
				gotNames := make([]string, 0)
				for _, ev := range pod.Spec.Containers[idx].Env {
					gotNames = append(gotNames, ev.Name)
				}
				if want == nil {
					if len(gotNames) != 0 {
						t.Errorf("container[%d] should remain empty after re-injection, got %v", idx, gotNames)
					}
				} else if !reflect.DeepEqual(gotNames, want) {
					t.Errorf("container[%d] env vars must not duplicate after re-injection: got %v, want %v", idx, gotNames, want)
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

	if dst.Name != "src" {
		t.Errorf("dst.Name: got %q, want %q", dst.Name, "src")
	}
	if dst.Namespace != "dst-ns" {
		t.Errorf("dst.Namespace: got %q, want %q", dst.Namespace, "dst-ns")
	}
	if dst.Type != corev1.SecretTypeOpaque {
		t.Errorf("dst.Type: got %q, want %q", dst.Type, corev1.SecretTypeOpaque)
	}
	if !reflect.DeepEqual(dst.Data["a"], []byte("b")) {
		t.Errorf("dst.Data[a]: got %q, want %q", dst.Data["a"], []byte("b"))
	}
	if dst.Labels["k"] != "v" {
		t.Errorf("dst.Labels[k]: got %q, want %q", dst.Labels["k"], "v")
	}
	if len(dst.Annotations) != 0 {
		t.Errorf("annotations must not be copied, got %v", dst.Annotations)
	}
	if len(dst.OwnerReferences) != 0 {
		t.Errorf("owner references must not be carried across namespaces, got %d entries", len(dst.OwnerReferences))
	}

	// Mutating dst.Data must not affect src.
	dst.Data["a"][0] = 'X'
	if src.Data["a"][0] != 'b' {
		t.Errorf("src.Data[a][0] should remain 'b', got %q", src.Data["a"][0])
	}
}

// --- baseline / field-precision tests --------------------------------------

// TestGatewayBaselineSpec pins down the community baseline registered by
// init(). It is the contract that enterprise inner_*.go overrides and
// historical deployments rely on: any drift in Name / SecretName / SubPath /
// MountPath / ReadOnly / ContainerSelector / EnvVars / EnabledFor breaks
// either upgrade compatibility or the inner override convention.
func TestGatewayBaselineSpec(t *testing.T) {
	prev := ListCABundleSpecs()
	// Defensively restore on exit even though we do not mutate the registry,
	// so a failed assertion below never leaves orphan state for sibling tests.
	t.Cleanup(func() {
		resetCABundleRegistryForTest()
		for i := range prev {
			RegisterCABundleSpec(prev[i])
		}
	})

	var gateway *CABundleSpec
	for i := range prev {
		if prev[i].Name == GatewayCABundleName {
			gateway = &prev[i]
			break
		}
	}
	if gateway == nil {
		t.Fatalf("init() must register the gateway baseline spec under name %q", GatewayCABundleName)
	}

	if gateway.SecretName != GatewayCASecretName {
		t.Errorf("SecretName: got %q, want %q", gateway.SecretName, GatewayCASecretName)
	}
	if gateway.SecretDataKey != GatewayCAKey {
		t.Errorf("SecretDataKey: got %q, want %q", gateway.SecretDataKey, GatewayCAKey)
	}
	if gateway.VolumeName != gatewayCAVolumeName {
		t.Errorf("VolumeName: got %q, want %q", gateway.VolumeName, gatewayCAVolumeName)
	}
	if gateway.MountPath != gatewayCAMountPath {
		t.Errorf("MountPath: got %q, want %q", gateway.MountPath, gatewayCAMountPath)
	}
	if gateway.SubPath != GatewayCAKey {
		t.Errorf("SubPath: got %q, want %q", gateway.SubPath, GatewayCAKey)
	}
	if !gateway.ReadOnly {
		t.Errorf("ReadOnly: got false, want true (gateway CA must never be writable from the workload)")
	}

	// EnabledFor must remain nil in the community baseline. Controller startup
	// is the sole place allowed to bind the runtime predicate via
	// BindCAEnabledFor; binding it here would couple identity/ to controller-
	// only feature gates.
	if gateway.EnabledFor != nil {
		t.Errorf("EnabledFor: got non-nil, want nil (must be bound at controller startup, not in init)")
	}

	// ContainerSelector must behave as OnlyMainContainer(): match index 0,
	// reject every other index. We compare behaviourally because function
	// values are not comparable in Go.
	if gateway.ContainerSelector == nil {
		t.Fatalf("ContainerSelector: got nil, want OnlyMainContainer()")
	}
	main := &corev1.Container{Name: "main"}
	side := &corev1.Container{Name: "sidecar"}
	if !gateway.ContainerSelector(main, 0) {
		t.Errorf("ContainerSelector must match main container at index 0")
	}
	if gateway.ContainerSelector(side, 1) {
		t.Errorf("ContainerSelector must not match index 1 (baseline is OnlyMainContainer)")
	}

	// EnvVars: exactly one SSL_CERT_FILE entry pointing at the mount path.
	// The well-known hint is what makes the gateway CA discoverable to
	// libraries that honour SSL_CERT_FILE; richer ecosystem env vars are
	// out of scope for the community baseline.
	wantEnv := []corev1.EnvVar{{Name: "SSL_CERT_FILE", Value: gatewayCAMountPath}}
	if !reflect.DeepEqual(gateway.EnvVars, wantEnv) {
		t.Errorf("EnvVars: got %+v, want %+v", gateway.EnvVars, wantEnv)
	}
}

// TestCACertInjector_InjectAllCAVolumes_Fields asserts that every field of
// the appended Volume is sourced from the spec, not just its Name. A
// regression on SecretName would mount the wrong CA; a regression on
// DefaultMode would change the file permission bits and silently break
// non-root workloads that read the bundle.
func TestCACertInjector_InjectAllCAVolumes_Fields(t *testing.T) {
	withTestSpec(t, newTestSpec())

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: testTargetNS},
	}
	InjectAllCAVolumes(context.Background(), newSandbox(), pod)

	if len(pod.Spec.Volumes) != 1 {
		t.Fatalf("expected exactly 1 volume, got %d", len(pod.Spec.Volumes))
	}
	v := pod.Spec.Volumes[0]
	if v.Name != testVolume {
		t.Errorf("Volume.Name: got %q, want %q", v.Name, testVolume)
	}
	if v.VolumeSource.Secret == nil {
		t.Fatalf("Volume.VolumeSource.Secret: got nil, want SecretVolumeSource")
	}
	if v.VolumeSource.Secret.SecretName != testSecret {
		t.Errorf("Volume.VolumeSource.Secret.SecretName: got %q, want %q",
			v.VolumeSource.Secret.SecretName, testSecret)
	}
	if v.VolumeSource.Secret.DefaultMode == nil {
		t.Fatalf("Volume.VolumeSource.Secret.DefaultMode: got nil, want *int32(0644)")
	}
	if *v.VolumeSource.Secret.DefaultMode != 0644 {
		t.Errorf("Volume.VolumeSource.Secret.DefaultMode: got %#o, want %#o",
			*v.VolumeSource.Secret.DefaultMode, 0644)
	}
}

// TestCACertInjector_InjectAllCAIntoContainers_Fields asserts that every
// VolumeMount and EnvVar field appended to the matched container is sourced
// from the spec. The Mounts/EnvVars table tests above only verify Name; this
// closes the gap so that drift on MountPath / SubPath / ReadOnly / EnvVar
// values cannot slip through unnoticed.
func TestCACertInjector_InjectAllCAIntoContainers_Fields(t *testing.T) {
	spec := newTestSpec()
	spec.EnvVars = []corev1.EnvVar{{Name: "SSL_CERT_FILE", Value: testMount}}
	withTestSpec(t, spec)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: testTargetNS},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main"}},
		},
	}
	InjectAllCAIntoContainers(context.Background(), newSandbox(), pod)

	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("expected exactly 1 container, got %d", len(pod.Spec.Containers))
	}
	c := pod.Spec.Containers[0]

	if len(c.VolumeMounts) != 1 {
		t.Fatalf("expected exactly 1 volume mount, got %d", len(c.VolumeMounts))
	}
	vm := c.VolumeMounts[0]
	wantVM := corev1.VolumeMount{
		Name:      testVolume,
		MountPath: testMount,
		SubPath:   testDataKey,
		ReadOnly:  true,
	}
	if !reflect.DeepEqual(vm, wantVM) {
		t.Errorf("VolumeMount: got %+v, want %+v", vm, wantVM)
	}

	if len(c.Env) != 1 {
		t.Fatalf("expected exactly 1 env var, got %d", len(c.Env))
	}
	wantEnv := corev1.EnvVar{Name: "SSL_CERT_FILE", Value: testMount}
	if !reflect.DeepEqual(c.Env[0], wantEnv) {
		t.Errorf("EnvVar: got %+v, want %+v", c.Env[0], wantEnv)
	}
}
