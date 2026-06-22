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
