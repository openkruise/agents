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
	"k8s.io/utils/ptr"
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

func TestCreate_PauseResumeHooks(t *testing.T) {
	exec := func(cmd string) *v1alpha1.LifecycleAction {
		return &v1alpha1.LifecycleAction{Exec: &corev1.ExecAction{Command: []string{cmd}}}
	}
	t.Run("with exec allowed", func(t *testing.T) {
		for name, action := range map[string]*v1alpha1.LifecycleAction{
			"prePause":   exec("flush"),
			"postResume": exec("restore"),
		} {
			obj := validOps()
			obj.Spec.Lifecycle = &v1alpha1.SandboxLifecycle{}
			if name == "prePause" {
				obj.Spec.Lifecycle.PrePause = action
			} else {
				obj.Spec.Lifecycle.PostResume = action
			}
			resp := newTestHandler().Handle(t.Context(), makeCreateRequest(t, obj))
			require.True(t, resp.Allowed, name)
		}
	})
	t.Run("missing exec rejected", func(t *testing.T) {
		tests := []struct {
			name      string
			lifecycle *v1alpha1.SandboxLifecycle
			want      string
		}{
			{name: "prePause", lifecycle: &v1alpha1.SandboxLifecycle{PrePause: &v1alpha1.LifecycleAction{}}},
			{name: "postResume", lifecycle: &v1alpha1.SandboxLifecycle{PostResume: &v1alpha1.LifecycleAction{}}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				obj := validOps()
				obj.Spec.Lifecycle = tt.lifecycle
				resp := newTestHandler().Handle(t.Context(), makeCreateRequest(t, obj))
				require.False(t, resp.Allowed)
				require.Contains(t, resp.Result.Message, "exec is required")
			})
		}
	})
}

func installerOps() *v1alpha1.SandboxUpdateOps {
	obj := validOps()
	obj.Spec.UpdateStrategy.Type = v1alpha1.SandboxUpdateOpsStrategyInstallerUpdate
	obj.Spec.UpdateStrategy.Installer = &v1alpha1.InstallerSpec{Image: "installer:1.4.0"}
	return obj
}

func TestCreate_InstallerUpdate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*v1alpha1.SandboxUpdateOps)
		patch   string
		allow   bool
		wantMsg string
	}{
		{
			name:  "valid installer",
			allow: true,
		},
		{
			name: "valid installer with command and target",
			mutate: func(o *v1alpha1.SandboxUpdateOps) {
				o.Spec.UpdateStrategy.Installer.Command = []string{"/install.sh"}
				o.Spec.UpdateStrategy.Installer.TargetContainerName = "sandbox"
			},
			allow: true,
		},
		{
			name: "missing installer rejected",
			mutate: func(o *v1alpha1.SandboxUpdateOps) {
				o.Spec.UpdateStrategy.Installer = nil
			},
			allow:   false,
			wantMsg: "installer is required",
		},
		{
			name: "empty image rejected",
			mutate: func(o *v1alpha1.SandboxUpdateOps) {
				o.Spec.UpdateStrategy.Installer.Image = ""
			},
			allow:   false,
			wantMsg: "installer.image is required",
		},
		{
			name: "empty command element rejected",
			mutate: func(o *v1alpha1.SandboxUpdateOps) {
				o.Spec.UpdateStrategy.Installer.Command = []string{"/install.sh", ""}
			},
			allow:   false,
			wantMsg: "command elements must be non-empty",
		},
		{
			name: "non-positive timeout rejected",
			mutate: func(o *v1alpha1.SandboxUpdateOps) {
				o.Spec.UpdateStrategy.Installer.TimeoutSeconds = ptr.To[int32](0)
			},
			allow:   false,
			wantMsg: "timeoutSeconds must be positive",
		},
		{
			name: "reserved env prefix rejected",
			mutate: func(o *v1alpha1.SandboxUpdateOps) {
				o.Spec.UpdateStrategy.Installer.Env = []corev1.EnvVar{{Name: "SANDBOX_INSTALLER_HACK", Value: "x"}}
			},
			allow:   false,
			wantMsg: "SANDBOX_INSTALLER_ prefix are reserved",
		},
		{
			name: "privileged security context rejected",
			mutate: func(o *v1alpha1.SandboxUpdateOps) {
				o.Spec.UpdateStrategy.Installer.SecurityContext = &corev1.SecurityContext{Privileged: ptr.To(true)}
			},
			allow:   false,
			wantMsg: "privileged installer containers are not allowed",
		},
		{
			name: "installer on non-installer strategy rejected",
			mutate: func(o *v1alpha1.SandboxUpdateOps) {
				o.Spec.UpdateStrategy.Type = v1alpha1.SandboxUpdateOpsStrategyRecreate
			},
			allow:   false,
			wantMsg: "installer is only allowed",
		},
		{
			name: "image change in patch rejected",
			mutate: func(o *v1alpha1.SandboxUpdateOps) {
				o.Spec.Patch = mustMarshalPatch(corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "nginx:1.22"}}},
				})
			},
			allow:   false,
			wantMsg: "InstallerUpdate only supports metadata-only template patches",
		},
		{
			name: "environment patch rejected",
			mutate: func(o *v1alpha1.SandboxUpdateOps) {
				o.Spec.Patch = mustMarshalPatch(corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Env: []corev1.EnvVar{{Name: "FOO", Value: "bar"}}}}},
				})
			},
			wantMsg: "metadata-only template patches",
		},
		{
			name: "typed metadata-only patch allowed",
			mutate: func(o *v1alpha1.SandboxUpdateOps) {
				o.Spec.Patch = mustMarshalPatch(corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"harness-version": "v2"}, Annotations: map[string]string{"harness-version": "v2"},
				}})
			},
			allow: true,
		},
		{name: "raw metadata patch allowed", patch: `{"metadata":{"labels":{"harness-version":"v2"}}}`, allow: true},
		{name: "empty metadata values allowed", patch: `{"metadata":{"labels":{"custom":""},"annotations":{"custom":""}}}`, allow: true},
		{name: "metadata deletion rejected", patch: `{"metadata":{"annotations":{"old":null}}}`, wantMsg: "metadata deletion or replacement"},
		{name: "label map deletion rejected", patch: `{"metadata":{"labels":null}}`, wantMsg: "metadata deletion or replacement"},
		{name: "metadata map deletion rejected", patch: `{"metadata":null}`, wantMsg: "metadata deletion or replacement"},
		{name: "label map replacement rejected", patch: `{"metadata":{"labels":{"$patch":"replace","harness-version":"v2"}}}`, wantMsg: "metadata deletion or replacement"},
		{name: "empty patch allowed", patch: `{}`, allow: true},
		{name: "empty spec allowed", patch: `{"spec":{}}`, allow: true},
		{name: "spec deletion rejected", patch: `{"spec":null}`, wantMsg: "metadata-only template patches"},
		{name: "container deletion rejected", patch: `{"spec":{"containers":[{"name":"main","$patch":"delete"}]}}`, wantMsg: "metadata-only template patches"},
		{name: "spec replacement rejected", patch: `{"spec":{"$patch":"replace"}}`, wantMsg: "metadata-only template patches"},
		{name: "unknown spec field rejected", patch: `{"spec":{"unknownField":"value"}}`, wantMsg: "metadata-only template patches"},
		{name: "root replacement rejected", patch: `{"$patch":"replace","metadata":{"labels":{"harness-version":"v2"}}}`, wantMsg: "metadata-only template patch"},
		{name: "template identity rejected", patch: `{"metadata":{"name":"replacement"}}`, wantMsg: "only supports template labels and annotations"},
		{name: "null patch decoded as omitted", patch: `null`, allow: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := installerOps()
			if tt.mutate != nil {
				tt.mutate(obj)
			}
			if tt.patch != "" {
				obj.Spec.Patch.Raw = []byte(tt.patch)
			}
			resp := newTestHandler().Handle(t.Context(), makeCreateRequest(t, obj))
			if tt.allow {
				require.True(t, resp.Allowed, "expected allowed, got: %s", resp.Result)
				return
			}
			require.False(t, resp.Allowed)
			require.Contains(t, resp.Result.Message, tt.wantMsg)
		})
	}
}

func TestUpdate_InstallerImmutable(t *testing.T) {
	oldObj := installerOps()
	newObj := oldObj.DeepCopy()
	newObj.Spec.UpdateStrategy.Installer.Image = "installer:1.5.0"
	h := newTestHandler()
	resp := h.Handle(context.TODO(), makeUpdateRequest(t, oldObj, newObj))
	require.False(t, resp.Allowed)
	require.Contains(t, resp.Result.Message, "installer is immutable")
}

func TestUpdate_InstallerUnchangedAllowed(t *testing.T) {
	for _, tt := range []struct {
		name  string
		patch string
		allow bool
	}{
		{name: "no patch", allow: true},
		{name: "metadata patch", patch: `{"metadata":{"labels":{"harness-version":"v2"}}}`, allow: true},
		{name: "spec patch rejected on update", patch: `{"spec":{"containers":[{"name":"main","env":[{"name":"FOO","value":"bar"}]}]}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			oldObj := installerOps()
			if tt.patch != "" {
				oldObj.Spec.Patch.Raw = []byte(tt.patch)
			}
			newObj := oldObj.DeepCopy()
			newObj.Spec.Paused = true
			resp := newTestHandler().Handle(t.Context(), makeUpdateRequest(t, oldObj, newObj))
			require.Equal(t, tt.allow, resp.Allowed)
			if !tt.allow {
				require.Contains(t, resp.Result.Message, "metadata-only template patches")
			}
		})
	}
}
