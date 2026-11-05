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
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
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

	tests := []struct {
		name          string
		createErr     error // error returned by the fake client Create; nil means success
		newStatus     *agentsv1alpha1.SandboxStatus
		expectError   string
		expectPod     bool   // whether a non-nil pod should be returned
		expectEvent   bool   // whether a Warning event should be emitted
		expectCond    bool   // whether a Ready=False condition should be set
		expectReason  string // expected condition reason
		expectMessage string // expected substring in condition message and event
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var fc client.Client
			if tt.createErr != nil {
				fc = fake.NewClientBuilder().WithScheme(scheme).
					WithInterceptorFuncs(interceptor.Funcs{
						Create: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.CreateOption) error {
							return tt.createErr
						},
					}).Build()
			} else {
				fc = fake.NewClientBuilder().WithScheme(scheme).Build()
			}

			recorder := record.NewFakeRecorder(10)
			podControl := NewPodControl(fc, recorder, simplePodGenFunc)

			box := baseBox.DeepCopy()
			args := CreatePodArgs{
				Box:       box,
				NewStatus: tt.newStatus,
			}

			pod, err := podControl.CreatePod(context.TODO(), args)

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

// TestCreatePodRuntimeInjectionProfile verifies that CreatePod detects the
// runtime injection profile of the pod a checkpoint delta was taken from and
// passes it to the generator, so the injection reproduces that profile. A
// malformed delta falls back to selecting the profile from the configuration.
func TestCreatePodRuntimeInjectionProfile(t *testing.T) {
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
		name        string
		delta       *runtime.RawExtension
		wantProfile string
	}{
		{
			name:        "no delta",
			wantProfile: "",
		},
		{
			name:        "delta without raw content",
			delta:       &runtime.RawExtension{},
			wantProfile: "",
		},
		{
			name: "all-in-one delta",
			delta: &runtime.RawExtension{Raw: []byte(
				`{"spec":{"initContainers":[{"name":"agent-runtime","image":"runtimed:aio","env":[{"name":"RUNTIMED_ENABLED_SERVICES","value":"agent-runtime"}]}]}}`)},
			wantProfile: sidecarutils.RuntimeInjectionProfileAllInOne,
		},
		{
			name: "legacy delta",
			delta: &runtime.RawExtension{Raw: []byte(
				`{"spec":{"initContainers":[{"name":"agent-runtime","image":"runtime:legacy"}]}}`)},
			wantProfile: sidecarutils.RuntimeInjectionProfileLegacy,
		},
		{
			name: "delta without the agent-runtime container",
			delta: &runtime.RawExtension{Raw: []byte(
				`{"spec":{"containers":[{"name":"main","image":"nginx"}]}}`)},
			wantProfile: "",
		},
		{
			name:        "malformed delta",
			delta:       &runtime.RawExtension{Raw: []byte(`{"spec":`)},
			wantProfile: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotProfile string
			gen := func(_ context.Context, args PodGenerateArgs) (*corev1.Pod, error) {
				gotProfile = args.RuntimeInjectionProfile
				return simplePodGenFunc(context.Background(), args)
			}
			podControl := NewPodControl(fake.NewClientBuilder().WithScheme(scheme).Build(), record.NewFakeRecorder(10), gen)

			pod, err := podControl.CreatePod(context.TODO(), CreatePodArgs{
				Box:              baseBox.DeepCopy(),
				NewStatus:        &agentsv1alpha1.SandboxStatus{},
				PodTemplateDelta: tt.delta,
			})

			require.NoError(t, err)
			require.NotNil(t, pod)
			assert.Equal(t, tt.wantProfile, gotProfile)
		})
	}
}

// TestGeneratePodFromSandboxRuntimeInjectionGate verifies that the generator
// passes the controller-owned RuntimeAIO feature gate and the resume profile
// into the injector: the gate alone selects the profile of a new pod, a resume
// keeps the profile its checkpoint delta was taken from, and resuming an
// all-in-one pod with the gate off fails instead of mixing both topologies.
func TestGeneratePodFromSandboxRuntimeInjectionGate(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	configData := map[string]string{
		sidecarutils.KEY_RUNTIME_INJECTION_CONFIG: `{"mainContainer":{},"csiSidecar":[{"name":"agent-runtime","image":"runtime:legacy"}],"volume":[]}`,
		sidecarutils.KEY_CSI_INJECTION_CONFIG:     `{"mainContainer":{},"csiSidecar":[{"name":"csi-sidecar","image":"csi:legacy"},{"name":"csi-agent-sidecar","image":"csi-agent:legacy"}],"volume":[]}`,
		sidecarutils.KeyRuntimeAIOInjectionConfig: `{"mainContainer":{},"initContainers":[{"name":"agent-runtime","image":"runtimed:aio","restartPolicy":"Always","env":[{"name":"RUNTIMED_ENABLED_SERVICES","valueFrom":{"fieldRef":{"fieldPath":"metadata.annotations['agents.kruise.io/runtimed-plugins']"}}}]}],"volume":[]}`,
	}

	legacyInit := []string{"agent-runtime=runtime:legacy", "csi-sidecar=csi:legacy", "csi-agent-sidecar=csi-agent:legacy"}
	aioInit := []string{"agent-runtime=runtimed:aio"}

	tests := []struct {
		name          string
		gate          bool
		resumeProfile string
		wantErr       error
		wantInit      []string
		wantServices  string
	}{
		{
			name:     "gate off injects the legacy profile",
			wantInit: legacyInit,
		},
		{
			name:         "gate on injects the all-in-one profile",
			gate:         true,
			wantInit:     aioInit,
			wantServices: "agent-runtime,csi,csi-agent",
		},
		{
			name:          "gate on resumes a legacy pod with the legacy profile",
			gate:          true,
			resumeProfile: sidecarutils.RuntimeInjectionProfileLegacy,
			wantInit:      legacyInit,
		},
		{
			name:          "gate off fails to resume an all-in-one pod",
			resumeProfile: sidecarutils.RuntimeInjectionProfileAllInOne,
			wantErr:       sidecarutils.ErrRuntimeAIOInjection,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, utilfeature.DefaultMutableFeatureGate.Set(fmt.Sprintf("RuntimeAIO=%t", tt.gate)))
			t.Cleanup(func() {
				_ = utilfeature.DefaultMutableFeatureGate.Set("RuntimeAIO=false")
			})

			cli := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sidecarutils.SandboxInjectionConfigName,
					Namespace: utils.DefaultSandboxDeployNamespace,
				},
				Data: configData,
			}).Build()

			sandbox := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "aio-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Runtimes: []agentsv1alpha1.RuntimeConfig{
						{Name: agentsv1alpha1.RuntimeConfigForInjectAgentRuntime},
						{Name: agentsv1alpha1.RuntimeConfigForInjectCsiMount},
					},
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "app", Image: "nginx:latest"}},
							},
						},
					},
				},
			}

			pod, err := GeneratePodFromSandbox(context.Background(), PodGenerateArgs{
				Client:                  cli,
				Box:                     sandbox,
				NewStatus:               &agentsv1alpha1.SandboxStatus{UpdateRevision: "rev-aio"},
				RuntimeInjectionProfile: tt.resumeProfile,
			})

			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				assert.Nil(t, pod)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, pod)

			var gotInit []string
			for _, c := range pod.Spec.InitContainers {
				gotInit = append(gotInit, c.Name+"="+c.Image)
			}
			assert.Equal(t, tt.wantInit, gotInit)
			assert.Equal(t, tt.wantServices, pod.Annotations[sidecarutils.AnnotationRuntimedPlugins])
		})
	}
}
