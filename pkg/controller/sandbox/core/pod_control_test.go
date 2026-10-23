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

package core

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/sidecarutils"
)

// simplePodGenFunc is a minimal PodGenerateFunc for testing that returns a
// basic pod without requiring a full sandbox template.
func simplePodGenFunc(_ context.Context, args PodGenerateArgs) (*corev1.Pod, error) {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      args.Box.Name,
			Namespace: args.Box.Namespace,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main", Image: "nginx:latest"},
			},
		},
	}, nil
}

func TestCreatePod(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	baseBox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
		},
	}
	hookErr := errors.New("prerequisite failed")
	genErr := errors.New("generation failed")

	tests := []struct {
		name               string
		createErr          error // error returned by the fake client Create; nil means success
		newStatus          *agentsv1alpha1.SandboxStatus
		expectError        string
		expectPod          bool   // whether a non-nil pod should be returned
		expectEvent        bool   // whether a Warning event should be emitted
		expectCond         bool   // whether a Ready=False condition should be set
		expectReason       string // expected condition reason
		expectMessage      string // expected substring in condition message and event
		withHook           bool
		hookErr            error
		generateErr        error
		renderFromTemplate bool
		isResume           bool
		delta              *runtime.RawExtension
	}{
		{
			name:        "create succeeds - no event, no condition",
			createErr:   nil,
			newStatus:   &agentsv1alpha1.SandboxStatus{},
			expectError: "",
			expectPod:   true,
			expectEvent: false,
			expectCond:  false,
		},
		{
			name:          "create fails with generic error - emits event and sets condition",
			createErr:     fmt.Errorf("pvc test-pvc is invalid"),
			newStatus:     &agentsv1alpha1.SandboxStatus{},
			expectError:   "pvc test-pvc is invalid",
			expectPod:     false,
			expectEvent:   true,
			expectCond:    true,
			expectReason:  agentsv1alpha1.SandboxReadyReasonPodCreateFailed,
			expectMessage: "pvc test-pvc is invalid",
		},
		{
			name:        "create fails with AlreadyExists - no event, no condition, returns pod",
			createErr:   apierrors.NewAlreadyExists(schema.GroupResource{Resource: "pods"}, "test-sandbox"),
			newStatus:   &agentsv1alpha1.SandboxStatus{},
			expectError: "",
			expectPod:   true,
			expectEvent: false,
			expectCond:  false,
		},
		{
			name:     "before-create hook sees checkpoint delta and template-generation context",
			withHook: true, renderFromTemplate: true,
			newStatus: &agentsv1alpha1.SandboxStatus{}, expectPod: true,
			delta: &runtime.RawExtension{Raw: []byte(`{"spec":{"containers":[{"name":"main","image":"nginx:updated"}]}}`)},
		},
		{
			name:     "before-create hook works on resume with independent options",
			withHook: true, isResume: true,
			newStatus: &agentsv1alpha1.SandboxStatus{}, expectPod: true,
		},
		{
			name:      "before-create hook works with zero-value creation options",
			withHook:  true,
			newStatus: &agentsv1alpha1.SandboxStatus{}, expectPod: true,
		},
		{
			name:     "before-create hook failure aborts before expectations and API creation",
			withHook: true, hookErr: hookErr,
			newStatus: &agentsv1alpha1.SandboxStatus{}, expectError: "before-create pod hook failed",
		},
		{
			name:     "generation failure never runs the before-create hook",
			withHook: true, generateErr: genErr,
			newStatus: &agentsv1alpha1.SandboxStatus{}, expectError: "generation failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldHook := BeforeCreatePodHook
			BeforeCreatePodHook = nil
			t.Cleanup(func() { BeforeCreatePodHook = oldHook })
			var hookCalls, createCalls int
			fc := fake.NewClientBuilder().WithScheme(scheme).
				WithInterceptorFuncs(interceptor.Funcs{
					Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
						createCalls++
						if tt.withHook {
							assert.Equal(t, 1, hookCalls, "hook must finish before Client.Create")
							assert.Equal(t, "ready", obj.GetAnnotations()["test.example/prepared"])
						}
						if tt.createErr != nil {
							return tt.createErr
						}
						return c.Create(ctx, obj, opts...)
					},
				}).Build()
			box := baseBox.DeepCopy()
			key := GetControllerKey(box)
			ScaleExpectation.DeleteExpectations(key)
			t.Cleanup(func() { ScaleExpectation.DeleteExpectations(key) })

			recorder := record.NewFakeRecorder(10)
			podControl := NewPodControl(fc, recorder, func(ctx context.Context, got PodGenerateArgs) (*corev1.Pod, error) {
				assert.Equal(t, tt.renderFromTemplate, got.RenderFromTemplate)
				assert.Equal(t, tt.isResume, got.IsResume)
				if tt.generateErr != nil {
					return nil, tt.generateErr
				}
				return simplePodGenFunc(ctx, got)
			})
			if tt.withHook {
				BeforeCreatePodHook = func(_ context.Context, c *PodControl, got CreatePodArgs, p *corev1.Pod) error {
					hookCalls++
					assert.Same(t, podControl, c)
					assert.Same(t, box, got.Box)
					assert.Same(t, tt.newStatus, got.NewStatus)
					assert.Equal(t, tt.renderFromTemplate, got.RenderFromTemplate)
					assert.Equal(t, tt.isResume, got.IsResume)
					assert.Equal(t, tt.delta, got.PodTemplateDelta)
					satisfied, _, _ := ScaleExpectation.SatisfiedExpectations(key)
					assert.True(t, satisfied, "no creation expectation before hook completion")
					if tt.delta != nil {
						assert.Equal(t, "nginx:updated", p.Spec.Containers[0].Image)
					}
					if p.Annotations == nil {
						p.Annotations = map[string]string{}
					}
					p.Annotations["test.example/prepared"] = "ready"
					return tt.hookErr
				}
			}
			args := CreatePodArgs{
				Box: box, NewStatus: tt.newStatus, PodTemplateDelta: tt.delta,
				RenderFromTemplate: tt.renderFromTemplate, IsResume: tt.isResume,
			}

			pod, err := podControl.CreatePod(context.TODO(), args)
			wantHookCalls := 0
			if tt.withHook && tt.generateErr == nil {
				wantHookCalls = 1
			}
			assert.Equal(t, wantHookCalls, hookCalls)
			if tt.hookErr != nil || tt.generateErr != nil {
				assert.Zero(t, createCalls)
				satisfied, _, _ := ScaleExpectation.SatisfiedExpectations(key)
				assert.True(t, satisfied, "hook/generation failure must leave no creation expectation")
				if tt.hookErr != nil {
					assert.True(t, errors.Is(err, tt.hookErr))
				}
			} else {
				assert.Equal(t, 1, createCalls)
			}

			// Error assertion
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				require.NoError(t, err)
			}

			// Pod assertion
			if tt.expectPod {
				assert.NotNil(t, pod)
			} else {
				assert.Nil(t, pod)
			}

			// Event assertion
			if tt.expectEvent {
				select {
				case event := <-recorder.Events:
					assert.Contains(t, event, "PodCreateFailed")
					if tt.expectMessage != "" {
						assert.Contains(t, event, tt.expectMessage)
					}
				default:
					t.Error("expected a Warning event to be recorded")
				}
			} else {
				select {
				case <-recorder.Events:
					t.Error("did not expect any event to be recorded")
				default:
					// expected - no event
				}
			}

			// Condition assertion
			if tt.expectCond {
				require.NotNil(t, tt.newStatus, "NewStatus should not be nil when expecting condition")
				cond := utils.GetSandboxCondition(tt.newStatus, string(agentsv1alpha1.SandboxConditionReady))
				require.NotNil(t, cond, "expected Ready condition to be set")
				assert.Equal(t, metav1.ConditionFalse, cond.Status)
				assert.Equal(t, tt.expectReason, cond.Reason)
				if tt.expectMessage != "" {
					assert.Contains(t, cond.Message, tt.expectMessage)
				}
			} else {
				cond := utils.GetSandboxCondition(tt.newStatus, string(agentsv1alpha1.SandboxConditionReady))
				assert.Nil(t, cond, "did not expect Ready condition to be set")
			}
		})
	}
}

// TestGeneratePodFromSandboxInjectionOptions exercises the real generator-to-
// injection path rather than the lightweight generator used by CreatePod tests.
func TestGeneratePodFromSandboxInjectionOptions(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, agentsv1alpha1.AddToScheme(scheme))
	for _, tt := range []struct {
		name     string
		render   bool
		isResume bool
	}{
		{name: "zero-value options"},
		{name: "current template", render: true},
		{name: "resume does not reselect", isResume: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			box := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "options-sandbox", Namespace: "default"},
				Spec: agentsv1alpha1.SandboxSpec{
					Runtimes: []agentsv1alpha1.RuntimeConfig{{Name: sidecarutils.KEY_RUNTIME_INJECTION_CONFIG}},
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "nginx"}}}},
					},
				},
			}
			defaults := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: sidecarutils.SandboxInjectionConfigName, Namespace: utils.DefaultSandboxDeployNamespace},
				Data:       map[string]string{sidecarutils.KEY_RUNTIME_INJECTION_CONFIG: "default"},
			}
			user := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: sidecarutils.SandboxInjectionUserConfigName, Namespace: utils.DefaultSandboxDeployNamespace},
				Data:       map[string]string{sidecarutils.KEY_RUNTIME_INJECTION_CONFIG: "user"},
			}
			fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(defaults, user).Build()
			oldHook := sidecarutils.RuntimeInjectionHook
			t.Cleanup(func() { sidecarutils.RuntimeInjectionHook = oldHook })
			calls := 0
			sidecarutils.RuntimeInjectionHook = func(_ context.Context, sb *agentsv1alpha1.Sandbox, _ *corev1.Pod, defaultConfigs, userConfigs map[string]string, opts sidecarutils.InjectionOptions) ([]string, error) {
				calls++
				assert.Same(t, box, sb)
				assert.Equal(t, tt.render, opts.RenderFromTemplate)
				assert.Equal(t, defaults.Data, defaultConfigs)
				assert.Equal(t, user.Data, userConfigs)
				return []string{sidecarutils.KEY_RUNTIME_INJECTION_CONFIG}, nil
			}
			pod, err := GeneratePodFromSandbox(context.Background(), PodGenerateArgs{
				Client: fc, Box: box, IsResume: tt.isResume, RenderFromTemplate: tt.render,
			})
			require.NoError(t, err)
			require.NotNil(t, pod)
			assert.Equal(t, 1, calls)
		})
	}
}

func TestCreatePodCheckpointAnnotation(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	baseBox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
		},
	}

	tests := []struct {
		name             string
		checkpointID     string
		annotationKey    string // empty means not configured (default)
		expectAnnotation bool
		expectKey        string
		expectValue      string
	}{
		{
			name:             "annotation key not configured - no annotation set",
			checkpointID:     "cp-123",
			annotationKey:    "",
			expectAnnotation: false,
		},
		{
			name:             "annotation key configured with checkpoint ID - annotation set",
			checkpointID:     "cp-123",
			annotationKey:    "agents.kruise.io/checkpoint-id",
			expectAnnotation: true,
			expectKey:        "agents.kruise.io/checkpoint-id",
			expectValue:      "cp-123",
		},
		{
			name:             "annotation key configured but checkpoint ID empty - no annotation set",
			checkpointID:     "",
			annotationKey:    "agents.kruise.io/checkpoint-id",
			expectAnnotation: false,
		},
		{
			name:             "custom annotation key - annotation set with custom key",
			checkpointID:     "cp-456",
			annotationKey:    "custom.io/my-checkpoint",
			expectAnnotation: true,
			expectKey:        "custom.io/my-checkpoint",
			expectValue:      "cp-456",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := fake.NewClientBuilder().WithScheme(scheme).Build()
			recorder := record.NewFakeRecorder(10)
			podControl := NewPodControl(fc, recorder, simplePodGenFunc)
			if tt.annotationKey != "" {
				podControl.SetCheckpointIDAnnotationKey(tt.annotationKey)
			}

			box := baseBox.DeepCopy()
			args := CreatePodArgs{
				Box:          box,
				NewStatus:    &agentsv1alpha1.SandboxStatus{},
				CheckpointID: tt.checkpointID,
			}

			pod, err := podControl.CreatePod(context.TODO(), args)
			require.NoError(t, err)
			require.NotNil(t, pod)

			if tt.expectAnnotation {
				assert.Equal(t, tt.expectValue, pod.Annotations[tt.expectKey],
					"checkpoint annotation should be set with key %q", tt.expectKey)
			} else {
				// Verify no checkpoint-related annotation exists.
				// The pod may still have the CreatedBy annotation from generation.
				for k := range pod.Annotations {
					assert.NotContains(t, k, "checkpoint", "unexpected checkpoint annotation: %s", k)
				}
			}
		})
	}
}

// TestCreatePodStampsRuntimeTLSAnnotation verifies the write-once runtime TLS
// stamp performed by CreatePod: when the controller is configured with runtime
// client TLS material, the call site opts in via AdvertiseRuntimeTLS and the
// sandbox declares the agent-runtime runtime, the canonical TLS port is
// persisted onto the sandbox before the pod is created; an already stamped
// value is never overwritten; an opted-out call site (resume path), a
// controller without TLS material or a sandbox without the agent-runtime
// runtime leaves the sandbox untouched; and a stamp failure aborts pod
// creation.
func TestCreatePodStampsRuntimeTLSAnnotation(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	tests := []struct {
		name           string
		tlsConfigured  bool // controller holds runtime client TLS material
		advertise      bool // CreatePodArgs.AdvertiseRuntimeTLS
		withoutRuntime bool // sandbox does not declare the agent-runtime runtime
		boxAnnotations map[string]string
		patchErr       error // error returned by the fake client Patch
		expectError    string
		wantValue      string // expected sandbox annotation value after CreatePod, "" means absent
		wantPodCreated bool
	}{
		{
			name:           "tls configured and call site opted in, sandbox gets stamped before create",
			tlsConfigured:  true,
			advertise:      true,
			wantValue:      "49984",
			wantPodCreated: true,
		},
		{
			name:           "already stamped value is kept (write-once)",
			tlsConfigured:  true,
			advertise:      true,
			boxAnnotations: map[string]string{agentsv1alpha1.AnnotationRuntimeTLSPort: "50000"},
			wantValue:      "50000",
			wantPodCreated: true,
		},
		{
			name:           "call site not opted in (resume path) leaves sandbox untouched",
			tlsConfigured:  true,
			advertise:      false,
			wantValue:      "",
			wantPodCreated: true,
		},
		{
			name:           "controller without runtime client TLS material leaves sandbox untouched",
			tlsConfigured:  false,
			advertise:      true,
			wantValue:      "",
			wantPodCreated: true,
		},
		{
			name:           "sandbox without agent-runtime runtime is not stamped",
			tlsConfigured:  true,
			advertise:      true,
			withoutRuntime: true,
			wantValue:      "",
			wantPodCreated: true,
		},
		{
			name:           "stamp failure aborts pod creation",
			tlsConfigured:  true,
			advertise:      true,
			patchErr:       fmt.Errorf("patch denied"),
			expectError:    "failed to stamp runtime TLS annotation",
			wantValue:      "", // persisted sandbox must stay unpolluted
			wantPodCreated: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			box := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "stamp-sbx",
					Namespace:   "default",
					Annotations: tt.boxAnnotations,
				},
			}
			if !tt.withoutRuntime {
				box.Spec.Runtimes = []agentsv1alpha1.RuntimeConfig{
					{Name: agentsv1alpha1.RuntimeConfigForInjectAgentRuntime},
				}
			}
			builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(box)
			if tt.patchErr != nil {
				builder = builder.WithInterceptorFuncs(interceptor.Funcs{
					Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
						return tt.patchErr
					},
				})
			}
			fc := builder.Build()

			podControl := NewPodControl(fc, record.NewFakeRecorder(10), simplePodGenFunc)
			podControl.SetAdvertiseRuntimeTLS(tt.tlsConfigured)
			oldHook := BeforeCreatePodHook
			t.Cleanup(func() { BeforeCreatePodHook = oldHook })
			hookCalls := 0
			BeforeCreatePodHook = func(ctx context.Context, c *PodControl, got CreatePodArgs, _ *corev1.Pod) error {
				hookCalls++
				persisted := &agentsv1alpha1.Sandbox{}
				require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(got.Box), persisted))
				assert.Equal(t, tt.wantValue, persisted.Annotations[agentsv1alpha1.AnnotationRuntimeTLSPort], "TLS stamp must precede the hook")
				return nil
			}

			pod, err := podControl.CreatePod(context.TODO(), CreatePodArgs{
				Box:                 box,
				NewStatus:           &agentsv1alpha1.SandboxStatus{},
				AdvertiseRuntimeTLS: tt.advertise,
			})

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				assert.Nil(t, pod)
			} else {
				require.NoError(t, err)
				require.NotNil(t, pod)
			}

			if tt.patchErr != nil {
				assert.Zero(t, hookCalls, "TLS stamp failure must bypass the hook")
			} else {
				assert.Equal(t, 1, hookCalls)
			}

			// The persisted sandbox is the consumer-visible view; on the success
			// path the in-memory sandbox must agree with it so later status
			// writes in the same reconcile observe the stamp. On a stamp failure
			// the reconcile aborts and the mutated in-memory object is discarded,
			// so only the persisted view is asserted.
			if tt.patchErr == nil {
				assert.Equal(t, tt.wantValue, box.Annotations[agentsv1alpha1.AnnotationRuntimeTLSPort])
			}
			persisted := &agentsv1alpha1.Sandbox{}
			require.NoError(t, fc.Get(context.TODO(),
				types.NamespacedName{Namespace: "default", Name: "stamp-sbx"}, persisted))
			assert.Equal(t, tt.wantValue, persisted.Annotations[agentsv1alpha1.AnnotationRuntimeTLSPort])

			createdPod := &corev1.Pod{}
			getErr := fc.Get(context.TODO(), types.NamespacedName{Namespace: "default", Name: "stamp-sbx"}, createdPod)
			if tt.wantPodCreated {
				assert.NoError(t, getErr, "expected the pod to be created")
			} else {
				assert.True(t, apierrors.IsNotFound(getErr), "expected pod creation to be aborted")
			}
		})
	}
}
