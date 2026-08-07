/*
Copyright 2025.

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

package validating

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/features"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
)

func init() {
	_ = v1alpha1.AddToScheme(scheme.Scheme)
}

func newTestHandler(objs ...runtime.Object) *SandboxUpdateOpsValidatingHandler {
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithRuntimeObjects(objs...).
		Build()
	return &SandboxUpdateOpsValidatingHandler{
		Client:  fakeClient,
		Decoder: admission.NewDecoder(scheme.Scheme),
	}
}

func makeCreateRequest(t *testing.T, obj *v1alpha1.SandboxUpdateOps) admission.Request {
	raw, err := json.Marshal(obj)
	require.NoError(t, err)
	return admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Create,
			Object:    runtime.RawExtension{Raw: raw},
		},
	}
}

func makeUpdateRequest(t *testing.T, oldObj, newObj *v1alpha1.SandboxUpdateOps) admission.Request {
	oldRaw, err := json.Marshal(oldObj)
	require.NoError(t, err)
	newRaw, err := json.Marshal(newObj)
	require.NoError(t, err)
	return admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Update,
			Object:    runtime.RawExtension{Raw: newRaw},
			OldObject: runtime.RawExtension{Raw: oldRaw},
		},
	}
}

func mustMarshalPatch(tmpl corev1.PodTemplateSpec) runtime.RawExtension {
	data, err := json.Marshal(tmpl)
	if err != nil {
		panic(err)
	}
	return runtime.RawExtension{Raw: data}
}

func validOps() *v1alpha1.SandboxUpdateOps {
	return &v1alpha1.SandboxUpdateOps{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ops",
			Namespace: "default",
		},
		Spec: v1alpha1.SandboxUpdateOpsSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
		},
	}
}

func TestCreate_ValidOps(t *testing.T) {
	h := newTestHandler()
	resp := h.Handle(context.TODO(), makeCreateRequest(t, validOps()))
	require.True(t, resp.Allowed, "expected allowed, got: %s", resp.Result)
}

func TestCreate_SelectorNil(t *testing.T) {
	obj := validOps()
	obj.Spec.Selector = nil
	h := newTestHandler()
	resp := h.Handle(context.TODO(), makeCreateRequest(t, obj))
	require.False(t, resp.Allowed)
	require.Contains(t, resp.Result.Message, "selector")
}

func TestCreate_SelectorInvalid(t *testing.T) {
	obj := validOps()
	obj.Spec.Selector = &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "app", Operator: "InvalidOp", Values: []string{"v"}},
		},
	}
	h := newTestHandler()
	resp := h.Handle(context.TODO(), makeCreateRequest(t, obj))
	require.False(t, resp.Allowed)
	require.Contains(t, resp.Result.Message, "selector")
}

func TestCreate_MaxUnavailableInvalidFormat(t *testing.T) {
	obj := validOps()
	obj.Spec.UpdateStrategy.MaxUnavailable = &intstr.IntOrString{Type: intstr.String, StrVal: "abc"}
	h := newTestHandler()
	resp := h.Handle(context.TODO(), makeCreateRequest(t, obj))
	require.False(t, resp.Allowed)
	require.Contains(t, resp.Result.Message, "maxUnavailable is invalid")
}

func TestCreate_MaxUnavailableValidPercent(t *testing.T) {
	obj := validOps()
	mu := intstr.FromString("50%")
	obj.Spec.UpdateStrategy.MaxUnavailable = &mu
	h := newTestHandler()
	resp := h.Handle(context.TODO(), makeCreateRequest(t, obj))
	require.True(t, resp.Allowed)
}

func TestCreate_LifecyclePreUpgradeExecNil(t *testing.T) {
	obj := validOps()
	obj.Spec.Lifecycle = &v1alpha1.SandboxLifecycle{
		PreUpgrade: &v1alpha1.UpgradeAction{
			// Exec is nil
		},
	}
	h := newTestHandler()
	resp := h.Handle(context.TODO(), makeCreateRequest(t, obj))
	require.False(t, resp.Allowed)
	require.Contains(t, resp.Result.Message, "exec is required")
}

func TestCreate_ActiveOpsExists_Rejected(t *testing.T) {
	existing := &v1alpha1.SandboxUpdateOps{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "existing-ops",
			Namespace: "default",
		},
		Spec: v1alpha1.SandboxUpdateOpsSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "old"},
			},
		},
		Status: v1alpha1.SandboxUpdateOpsStatus{
			Phase: v1alpha1.SandboxUpdateOpsUpdating,
		},
	}
	h := newTestHandler(existing)
	resp := h.Handle(context.TODO(), makeCreateRequest(t, validOps()))
	require.False(t, resp.Allowed)
	require.Contains(t, resp.Result.Message, "active SandboxUpdateOps")
}

func TestCreate_CompletedOpsExists_Allowed(t *testing.T) {
	existing := &v1alpha1.SandboxUpdateOps{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "completed-ops",
			Namespace: "default",
		},
		Spec: v1alpha1.SandboxUpdateOpsSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "old"},
			},
		},
		Status: v1alpha1.SandboxUpdateOpsStatus{
			Phase: v1alpha1.SandboxUpdateOpsCompleted,
		},
	}
	h := newTestHandler(existing)
	resp := h.Handle(context.TODO(), makeCreateRequest(t, validOps()))
	require.True(t, resp.Allowed)
}

func TestCreate_FailedOpsExists_Allowed(t *testing.T) {
	existing := &v1alpha1.SandboxUpdateOps{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "failed-ops",
			Namespace: "default",
		},
		Spec: v1alpha1.SandboxUpdateOpsSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "old"},
			},
		},
		Status: v1alpha1.SandboxUpdateOpsStatus{
			Phase: v1alpha1.SandboxUpdateOpsFailed,
		},
	}
	h := newTestHandler(existing)
	resp := h.Handle(context.TODO(), makeCreateRequest(t, validOps()))
	require.True(t, resp.Allowed)
}

func TestUpdate_OnlyPaused_Allowed(t *testing.T) {
	oldObj := validOps()
	newObj := oldObj.DeepCopy()
	newObj.Spec.Paused = true
	h := newTestHandler()
	resp := h.Handle(context.TODO(), makeUpdateRequest(t, oldObj, newObj))
	require.True(t, resp.Allowed)
}

func TestUpdate_OnlyUpdateStrategy_Allowed(t *testing.T) {
	oldObj := validOps()
	newObj := oldObj.DeepCopy()
	mu := intstr.FromInt(3)
	newObj.Spec.UpdateStrategy.MaxUnavailable = &mu
	h := newTestHandler()
	resp := h.Handle(context.TODO(), makeUpdateRequest(t, oldObj, newObj))
	require.True(t, resp.Allowed)
}

func TestUpdate_ChangeSelector_Rejected(t *testing.T) {
	oldObj := validOps()
	newObj := oldObj.DeepCopy()
	newObj.Spec.Selector = &metav1.LabelSelector{
		MatchLabels: map[string]string{"app": "changed"},
	}
	h := newTestHandler()
	resp := h.Handle(context.TODO(), makeUpdateRequest(t, oldObj, newObj))
	require.False(t, resp.Allowed)
	require.Contains(t, resp.Result.Message, "selector")
}

func TestUpdate_ChangePatch_Rejected(t *testing.T) {
	oldObj := validOps()
	newObj := oldObj.DeepCopy()
	newObj.Spec.Patch = mustMarshalPatch(corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "new", Image: "nginx"}},
		},
	})
	h := newTestHandler()
	resp := h.Handle(context.TODO(), makeUpdateRequest(t, oldObj, newObj))
	require.False(t, resp.Allowed)
	require.Contains(t, resp.Result.Message, "patch")
}

func TestUpdate_ChangeLifecycle_Rejected(t *testing.T) {
	oldObj := validOps()
	newObj := oldObj.DeepCopy()
	newObj.Spec.Lifecycle = &v1alpha1.SandboxLifecycle{
		PreUpgrade: &v1alpha1.UpgradeAction{
			Exec: &corev1.ExecAction{Command: []string{"/bin/bash", "-c", "echo hi"}},
		},
	}
	h := newTestHandler()
	resp := h.Handle(context.TODO(), makeUpdateRequest(t, oldObj, newObj))
	require.False(t, resp.Allowed)
	require.Contains(t, resp.Result.Message, "lifecycle")
}

func TestHandle_DecodeFailure(t *testing.T) {
	h := newTestHandler()
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Create,
			Object:    runtime.RawExtension{Raw: []byte(`{invalid-json}`)},
		},
	}
	resp := h.Handle(context.TODO(), req)
	require.False(t, resp.Allowed)
	require.Equal(t, int32(400), resp.Result.Code)
}

func TestValidateInplacePatchAllowedFields(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		wantEmpty bool
		wantSub   string
	}{
		{name: "invalid JSON", raw: "{bad", wantSub: "failed to parse patch"},
		{name: "top-level null skipped", raw: `{"spec":null}`, wantEmpty: true},
		{name: "metadata null skipped", raw: `{"metadata":null}`, wantEmpty: true},
		{name: "metadata not object", raw: `{"metadata":"x"}`, wantSub: "metadata must be a JSON object"},
		{name: "spec not object", raw: `{"spec":"x"}`, wantSub: "spec must be a JSON object"},
		{name: "spec.containers not array", raw: `{"spec":{"containers":"x"}}`, wantSub: "spec.containers must be a JSON array"},
		{name: "container item not object", raw: `{"spec":{"containers":["x"]}}`, wantSub: "spec.containers[0] must be a JSON object"},
		{name: "container field null skipped", raw: `{"spec":{"containers":[{"image":null}]}}`, wantEmpty: true},
		{name: "allowed image patch", raw: `{"spec":{"containers":[{"name":"main","image":"v2"}]}}`, wantEmpty: true},
		{name: "forbidden env field", raw: `{"spec":{"containers":[{"name":"main","env":[]}]}}`, wantSub: "does not support modifying spec.containers[0].env"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := validateInplacePatchAllowedFields([]byte(tt.raw))
			if tt.wantEmpty {
				require.Empty(t, msg, "expected no violation for %q", tt.name)
				return
			}
			require.Contains(t, msg, tt.wantSub, "unexpected violation for %q", tt.name)
		})
	}
}

func TestHandle_DeleteOperation_Allowed(t *testing.T) {
	obj := validOps()
	raw, err := json.Marshal(obj)
	require.NoError(t, err)
	h := newTestHandler()
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Delete,
			Object:    runtime.RawExtension{Raw: raw},
		},
	}
	resp := h.Handle(context.TODO(), req)
	require.True(t, resp.Allowed)
}

func TestHandle_ConnectOperation_Allowed(t *testing.T) {
	obj := validOps()
	raw, err := json.Marshal(obj)
	require.NoError(t, err)
	h := newTestHandler()
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Connect,
			Object:    runtime.RawExtension{Raw: raw},
		},
	}
	resp := h.Handle(context.TODO(), req)
	require.True(t, resp.Allowed)
}

func TestCreate_PostUpgradeExecNil(t *testing.T) {
	obj := validOps()
	obj.Spec.Lifecycle = &v1alpha1.SandboxLifecycle{
		PostUpgrade: &v1alpha1.UpgradeAction{
			// Exec is nil
		},
	}
	h := newTestHandler()
	resp := h.Handle(context.TODO(), makeCreateRequest(t, obj))
	require.False(t, resp.Allowed)
	require.Contains(t, resp.Result.Message, "exec is required")
}

func TestCreate_LifecycleValidExec_Allowed(t *testing.T) {
	obj := validOps()
	obj.Spec.Lifecycle = &v1alpha1.SandboxLifecycle{
		PreUpgrade: &v1alpha1.UpgradeAction{
			Exec: &corev1.ExecAction{Command: []string{"/bin/sh", "-c", "echo pre"}},
		},
		PostUpgrade: &v1alpha1.UpgradeAction{
			Exec: &corev1.ExecAction{Command: []string{"/bin/sh", "-c", "echo post"}},
		},
	}
	h := newTestHandler()
	resp := h.Handle(context.TODO(), makeCreateRequest(t, obj))
	require.True(t, resp.Allowed)
}

func TestCreate_SameNameOpsSkipped_Allowed(t *testing.T) {
	// An existing ops with the same name should be skipped in active-check
	existing := &v1alpha1.SandboxUpdateOps{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ops",
			Namespace: "default",
		},
		Spec: v1alpha1.SandboxUpdateOpsSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
		},
		Status: v1alpha1.SandboxUpdateOpsStatus{
			Phase: v1alpha1.SandboxUpdateOpsUpdating,
		},
	}
	h := newTestHandler(existing)
	resp := h.Handle(context.TODO(), makeCreateRequest(t, validOps()))
	require.True(t, resp.Allowed)
}

func TestUpdate_DecodeOldObjectFailure(t *testing.T) {
	newObj := validOps()
	newRaw, err := json.Marshal(newObj)
	require.NoError(t, err)
	h := newTestHandler()
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Update,
			Object:    runtime.RawExtension{Raw: newRaw},
			OldObject: runtime.RawExtension{Raw: []byte(`{bad-json}`)},
		},
	}
	resp := h.Handle(context.TODO(), req)
	require.False(t, resp.Allowed)
	require.Equal(t, int32(400), resp.Result.Code)
}

func TestCreate_MultipleErrors(t *testing.T) {
	// Trigger multiple validation errors at once: nil selector + invalid lifecycle
	obj := validOps()
	obj.Spec.Selector = nil
	obj.Spec.Lifecycle = &v1alpha1.SandboxLifecycle{
		PreUpgrade:  &v1alpha1.UpgradeAction{},
		PostUpgrade: &v1alpha1.UpgradeAction{},
	}
	h := newTestHandler()
	resp := h.Handle(context.TODO(), makeCreateRequest(t, obj))
	require.False(t, resp.Allowed)
	require.Contains(t, resp.Result.Message, "selector")
	require.Contains(t, resp.Result.Message, "exec is required")
}

func TestUpdate_MultipleImmutableChanges(t *testing.T) {
	oldObj := validOps()
	newObj := oldObj.DeepCopy()
	newObj.Spec.Selector = &metav1.LabelSelector{
		MatchLabels: map[string]string{"app": "changed"},
	}
	newObj.Spec.Patch = mustMarshalPatch(corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c", Image: "img"}},
		},
	})
	newObj.Spec.Lifecycle = &v1alpha1.SandboxLifecycle{
		PreUpgrade: &v1alpha1.UpgradeAction{
			Exec: &corev1.ExecAction{Command: []string{"echo"}},
		},
	}
	h := newTestHandler()
	resp := h.Handle(context.TODO(), makeUpdateRequest(t, oldObj, newObj))
	require.False(t, resp.Allowed)
	require.Contains(t, resp.Result.Message, "selector")
	require.Contains(t, resp.Result.Message, "patch")
	require.Contains(t, resp.Result.Message, "lifecycle")
}

func TestPathAndEnabled(t *testing.T) {
	h := newTestHandler()
	require.Equal(t, "/validate-sandboxupdateops", h.Path())
	require.True(t, h.Enabled())
}

func TestCreate_CheckpointRestoreWithImageChange_Rejected(t *testing.T) {
	obj := validOps()
	obj.Spec.UpdateStrategy.Type = v1alpha1.SandboxUpdateOpsStrategyCheckpointRestore
	obj.Spec.Patch = mustMarshalPatch(corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: "nginx:1.22"}},
		},
	})
	h := newTestHandler()
	resp := h.Handle(context.TODO(), makeCreateRequest(t, obj))
	require.False(t, resp.Allowed)
	require.Contains(t, resp.Result.Message, "CheckpointRestore strategy does not support modifying container images")
}

func TestCreate_CheckpointRestoreWithInitImageChange_Rejected(t *testing.T) {
	obj := validOps()
	obj.Spec.UpdateStrategy.Type = v1alpha1.SandboxUpdateOpsStrategyCheckpointRestore
	obj.Spec.Patch = mustMarshalPatch(corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "init", Image: "busybox:1.28"}},
		},
	})
	h := newTestHandler()
	resp := h.Handle(context.TODO(), makeCreateRequest(t, obj))
	require.False(t, resp.Allowed)
	require.Contains(t, resp.Result.Message, "CheckpointRestore strategy does not support modifying init container images")
}

func TestCreate_CheckpointRestoreWithoutImageChange_Allowed(t *testing.T) {
	obj := validOps()
	obj.Spec.UpdateStrategy.Type = v1alpha1.SandboxUpdateOpsStrategyCheckpointRestore
	obj.Spec.Patch = mustMarshalPatch(corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Env: []corev1.EnvVar{{Name: "FOO", Value: "bar"}}}},
		},
	})
	h := newTestHandler()
	resp := h.Handle(context.TODO(), makeCreateRequest(t, obj))
	require.True(t, resp.Allowed)
}

func TestCreate_CheckpointRestoreNoPatch_Allowed(t *testing.T) {
	obj := validOps()
	obj.Spec.UpdateStrategy.Type = v1alpha1.SandboxUpdateOpsStrategyCheckpointRestore
	h := newTestHandler()
	resp := h.Handle(context.TODO(), makeCreateRequest(t, obj))
	require.True(t, resp.Allowed)
}

func TestCreate_RecreateWithImageChange_Allowed(t *testing.T) {
	obj := validOps()
	obj.Spec.UpdateStrategy.Type = v1alpha1.SandboxUpdateOpsStrategyRecreate
	obj.Spec.Patch = mustMarshalPatch(corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: "nginx:1.22"}},
		},
	})
	h := newTestHandler()
	resp := h.Handle(context.TODO(), makeCreateRequest(t, obj))
	require.True(t, resp.Allowed)
}

// TestCreate_InplaceUpdateWithLifecycle_Allowed verifies that lifecycle hooks are
// accepted together with the InplaceUpdate strategy: the in-place update runs
// through the sandbox controller's upgrade lifecycle, so PreUpgrade and
// PostUpgrade hooks are executed rather than silently ignored.
func TestCreate_InplaceUpdateWithLifecycle_Allowed(t *testing.T) {
	obj := validOps()
	obj.Spec.UpdateStrategy.Type = v1alpha1.SandboxUpdateOpsStrategyInplaceUpdate
	obj.Spec.Lifecycle = &v1alpha1.SandboxLifecycle{
		PreUpgrade: &v1alpha1.UpgradeAction{
			Exec: &corev1.ExecAction{Command: []string{"/bin/sh", "-c", "echo pre"}},
		},
	}
	h := newTestHandler()
	resp := h.Handle(context.TODO(), makeCreateRequest(t, obj))
	require.True(t, resp.Allowed)
}

func TestUpdate_ChangeStrategyType_Rejected(t *testing.T) {
	oldObj := validOps()
	oldObj.Spec.UpdateStrategy.Type = v1alpha1.SandboxUpdateOpsStrategyRecreate
	newObj := oldObj.DeepCopy()
	newObj.Spec.UpdateStrategy.Type = v1alpha1.SandboxUpdateOpsStrategyInplaceUpdate
	h := newTestHandler()
	resp := h.Handle(context.TODO(), makeUpdateRequest(t, oldObj, newObj))
	require.False(t, resp.Allowed)
	require.Contains(t, resp.Result.Message, "updateStrategy.type is immutable")
}

func TestCreate_InplaceUpdatePatchValidation(t *testing.T) {
	tests := []struct {
		name        string
		gateEnabled bool
		strategy    v1alpha1.SandboxUpdateOpsStrategyType
		patch       string
		wantAllowed bool
		wantMessage string
	}{
		{
			name:        "gate disabled, env change allowed",
			gateEnabled: false,
			strategy:    v1alpha1.SandboxUpdateOpsStrategyInplaceUpdate,
			patch:       `{"spec":{"containers":[{"name":"main","env":[{"name":"FOO","value":"bar"}]}]}}`,
			wantAllowed: true,
		},
		{
			name:        "gate enabled, image-only change allowed",
			gateEnabled: true,
			strategy:    v1alpha1.SandboxUpdateOpsStrategyInplaceUpdate,
			patch:       `{"spec":{"containers":[{"name":"main","image":"nginx:1.22"}]}}`,
			wantAllowed: true,
		},
		{
			name:        "gate enabled, resources-only change allowed",
			gateEnabled: true,
			strategy:    v1alpha1.SandboxUpdateOpsStrategyInplaceUpdate,
			patch:       `{"spec":{"containers":[{"name":"main","resources":{"limits":{"cpu":"2"}}}]}}`,
			wantAllowed: true,
		},
		{
			name:        "gate enabled, init container image change allowed",
			gateEnabled: true,
			strategy:    v1alpha1.SandboxUpdateOpsStrategyInplaceUpdate,
			patch:       `{"spec":{"initContainers":[{"name":"init","image":"busybox:1.29"}]}}`,
			wantAllowed: true,
		},
		{
			name:        "gate enabled, metadata labels and annotations allowed",
			gateEnabled: true,
			strategy:    v1alpha1.SandboxUpdateOpsStrategyInplaceUpdate,
			patch:       `{"metadata":{"labels":{"a":"b"},"annotations":{"c":"d"}}}`,
			wantAllowed: true,
		},
		{
			name:        "gate enabled, typed-marshal null noise allowed",
			gateEnabled: true,
			strategy:    v1alpha1.SandboxUpdateOpsStrategyInplaceUpdate,
			patch:       `{"metadata":{"creationTimestamp":null,"labels":{"a":"b"}},"spec":{"containers":null}}`,
			wantAllowed: true,
		},
		{
			name:        "gate enabled, env change rejected",
			gateEnabled: true,
			strategy:    v1alpha1.SandboxUpdateOpsStrategyInplaceUpdate,
			patch:       `{"spec":{"containers":[{"name":"main","env":[{"name":"FOO","value":"bar"}]}]}}`,
			wantAllowed: false,
			wantMessage: "spec.containers[0].env",
		},
		{
			name:        "gate enabled, volumes change rejected",
			gateEnabled: true,
			strategy:    v1alpha1.SandboxUpdateOpsStrategyInplaceUpdate,
			patch:       `{"spec":{"volumes":[{"name":"data","emptyDir":{}}]}}`,
			wantAllowed: false,
			wantMessage: "spec.volumes",
		},
		{
			name:        "gate enabled, init container command change rejected",
			gateEnabled: true,
			strategy:    v1alpha1.SandboxUpdateOpsStrategyInplaceUpdate,
			patch:       `{"spec":{"initContainers":[{"name":"init","command":["sh"]}]}}`,
			wantAllowed: false,
			wantMessage: "spec.initContainers[0].command",
		},
		{
			name:        "gate enabled, metadata.name change rejected",
			gateEnabled: true,
			strategy:    v1alpha1.SandboxUpdateOpsStrategyInplaceUpdate,
			patch:       `{"metadata":{"name":"evil"}}`,
			wantAllowed: false,
			wantMessage: "metadata.name",
		},
		{
			name:        "gate enabled, unknown top-level field rejected",
			gateEnabled: true,
			strategy:    v1alpha1.SandboxUpdateOpsStrategyInplaceUpdate,
			patch:       `{"status":{}}`,
			wantAllowed: false,
			wantMessage: "does not support field",
		},
		{
			name:        "gate enabled, SMP delete directive rejected",
			gateEnabled: true,
			strategy:    v1alpha1.SandboxUpdateOpsStrategyInplaceUpdate,
			patch:       `{"spec":{"containers":[{"name":"main","$patch":"delete"}]}}`,
			wantAllowed: false,
			wantMessage: "$patch",
		},
		{
			name:        "gate enabled, non-object patch rejected",
			gateEnabled: true,
			strategy:    v1alpha1.SandboxUpdateOpsStrategyInplaceUpdate,
			patch:       `["not","an","object"]`,
			wantAllowed: false,
			wantMessage: "failed to parse patch",
		},
		{
			name:        "gate enabled, Recreate strategy with env not checked",
			gateEnabled: true,
			strategy:    v1alpha1.SandboxUpdateOpsStrategyRecreate,
			patch:       `{"spec":{"containers":[{"name":"main","env":[{"name":"FOO","value":"bar"}]}]}}`,
			wantAllowed: true,
		},
	}
	gate := string(features.SandboxUpdateOpsInplacePatchValidationGate)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.gateEnabled {
				require.NoError(t, utilfeature.DefaultMutableFeatureGate.Set(gate+"=true"))
			}
			t.Cleanup(func() {
				_ = utilfeature.DefaultMutableFeatureGate.Set(gate + "=false")
			})

			obj := validOps()
			obj.Spec.UpdateStrategy.Type = tt.strategy
			obj.Spec.Patch = runtime.RawExtension{Raw: []byte(tt.patch)}
			h := newTestHandler()
			resp := h.Handle(context.TODO(), makeCreateRequest(t, obj))
			require.Equal(t, tt.wantAllowed, resp.Allowed, "result: %v", resp.Result)
			if tt.wantMessage != "" {
				require.Contains(t, resp.Result.Message, tt.wantMessage)
			}
		})
	}
}
