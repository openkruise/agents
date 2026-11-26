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

package sessionservice

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agents "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils/defaults"
)

func fixture() *agents.SessionService {
	obj := &agents.SessionService{ObjectMeta: metav1.ObjectMeta{Name: "session-service"}}
	obj.Spec.Template = &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "nginx:latest"}}}}
	obj.Spec.Probes = []agents.Probe{{Name: "idle", Probe: corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"check-idle"}}}}}}
	obj.Spec.AutoPausePolicy = &agents.AutoPausePolicy{Pause: &agents.PausePolicy{WhenProbedIdleState: &agents.ProbedIdleStateRule{Probe: "idle", MessageRegex: "^idle$", ThresholdDuration: &metav1.Duration{Duration: time.Minute}}}}
	obj.Spec.Lifecycle = &agents.SandboxLifecycle{PrePause: &agents.LifecycleAction{Exec: &corev1.ExecAction{Command: []string{"flush"}}, TimeoutSeconds: 1}}
	obj.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "data"}, Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, VolumeMode: ptr.To(corev1.PersistentVolumeFilesystem), Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}}}
	obj.Spec.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}
	defaults.SetDefaultPodSpec(&obj.Spec.Template.Spec)
	return obj
}
func request(t *testing.T, obj, old *agents.SessionService) admission.Request {
	t.Helper()
	data, err := json.Marshal(obj)
	require.NoError(t, err)
	req := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Operation: admissionv1.Create, Object: runtime.RawExtension{Raw: data}}}
	if old != nil {
		data, err = json.Marshal(old)
		require.NoError(t, err)
		req.Operation = admissionv1.Update
		req.OldObject = runtime.RawExtension{Raw: data}
	}
	return req
}
func TestValidation(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, agents.AddToScheme(scheme))
	handler := &ValidatingHandler{Decoder: admission.NewDecoder(scheme)}
	for _, tc := range []struct {
		name    string
		edit    func(*agents.SessionService)
		update  bool
		allowed bool
	}{
		{name: "create", allowed: true},
		{name: "longest service DNS name", edit: func(p *agents.SessionService) { p.Name = strings.Repeat("a", 63) }, allowed: true},
		{name: "service DNS name too long", edit: func(p *agents.SessionService) { p.Name = strings.Repeat("a", 64) }},
		{name: "dotted service name", edit: func(p *agents.SessionService) { p.Name = "service.example" }},
		{name: "uppercase service name", edit: func(p *agents.SessionService) { p.Name = "Service" }},
		{name: "invalid service name", edit: func(p *agents.SessionService) { p.Name = "-service" }},
		{name: "backend", edit: func(p *agents.SessionService) { p.Spec.SandboxTemplateSpec.Backend = "other" }},
		{name: "empty build options", edit: func(p *agents.SessionService) {
			p.Spec.SandboxTemplateSpec.BuildOptions = &agents.SandboxTemplateBuildOptions{}
		}},
		{name: "build options", edit: func(p *agents.SessionService) {
			p.Spec.SandboxTemplateSpec.BuildOptions = &agents.SandboxTemplateBuildOptions{FromTemplate: "base"}
		}},
		{name: "duplicate probes", edit: func(p *agents.SessionService) {
			p.Spec.Probes = append(p.Spec.Probes, p.Spec.Probes[0])
		}},
		{name: "unsupported probe handler", edit: func(p *agents.SessionService) {
			p.Spec.SandboxTemplateSpec.Probes[0].ProbeHandler = corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/"}}
		}},
		{name: "empty auto pause policy", edit: func(p *agents.SessionService) {
			p.Spec.SandboxTemplateSpec.AutoPausePolicy = &agents.AutoPausePolicy{}
		}},
		{name: "missing policy probe", edit: func(p *agents.SessionService) {
			p.Spec.SandboxTemplateSpec.AutoPausePolicy.Pause.WhenProbedIdleState.Probe = "missing"
		}},
		{name: "invalid policy regex", edit: func(p *agents.SessionService) {
			p.Spec.SandboxTemplateSpec.AutoPausePolicy.Pause.WhenProbedIdleState.MessageRegex = "["
		}},
		{name: "missing PVC name", edit: func(p *agents.SessionService) { p.Spec.SandboxTemplateSpec.VolumeClaimTemplates[0].Name = "" }},
		{name: "invalid PVC volume name", edit: func(p *agents.SessionService) {
			p.Spec.SandboxTemplateSpec.VolumeClaimTemplates[0].Name = "bad_name"
		}},
		{name: "duplicate PVC names", edit: func(p *agents.SessionService) {
			p.Spec.VolumeClaimTemplates = append(p.Spec.VolumeClaimTemplates, *p.Spec.VolumeClaimTemplates[0].DeepCopy())
		}},
		{name: "unmounted PVC", edit: func(p *agents.SessionService) {
			p.Spec.SandboxTemplateSpec.Template.Spec.Containers[0].VolumeMounts = nil
		}},
		{name: "pre upgrade lifecycle", edit: func(p *agents.SessionService) {
			p.Spec.SandboxTemplateSpec.Lifecycle.PreUpgrade = &agents.LifecycleAction{}
		}},
		{name: "post upgrade lifecycle", edit: func(p *agents.SessionService) {
			p.Spec.SandboxTemplateSpec.Lifecycle.PostUpgrade = &agents.LifecycleAction{}
		}},
		{name: "template update", update: true, allowed: true, edit: func(o *agents.SessionService) { o.Spec.Template.Spec.Containers[0].Image = "nginx:stable" }},
		{name: "PVC template update", update: true, allowed: true, edit: func(o *agents.SessionService) {
			o.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "data"}, Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}}}
			o.Spec.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}
		}},
		{name: "idle remains runtime unsupported", allowed: true, edit: func(o *agents.SessionService) { o.Spec.SessionPolicy.PauseAfterIdle = "1m" }},
		{name: "missing template", edit: func(o *agents.SessionService) { o.Spec.Template = nil }},
		{name: "backend immutable", update: true, edit: func(o *agents.SessionService) { o.Spec.Backend = "k8s" }},
		{name: "unsupported backend", edit: func(o *agents.SessionService) { o.Spec.Backend = "substrate" }},
		{name: "invalid pod", edit: func(o *agents.SessionService) { o.Spec.Template.Spec.Containers[0].Name = "INVALID" }},

		{name: "invalid surge", edit: func(o *agents.SessionService) { o.Spec.UpdateStrategy.MaxSurge = ptr.To(intstr.FromInt(0)) }},
		{name: "surge overflow", edit: func(o *agents.SessionService) {
			o.Spec.Replicas = ptr.To[int32](2)
			o.Spec.UpdateStrategy.MaxSurge = ptr.To(intstr.FromString("999999999999999999999999999999%"))
		}},
		{name: "unmounted pvc", edit: func(o *agents.SessionService) { o.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{}} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := fixture()
			old := obj.DeepCopy()
			if tc.edit != nil {
				tc.edit(obj)
			}
			var previous *agents.SessionService
			if tc.update {
				previous = old
			}
			copy := obj.DeepCopy()
			response := handler.Handle(context.Background(), request(t, obj, previous))
			require.Equal(t, tc.allowed, response.Allowed, "%v", response.Result)
			require.Equal(t, copy, obj)
		})
	}
}
func TestDefaulting(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, agents.AddToScheme(scheme))
	handler := &Defaulter{Decoder: admission.NewDecoder(scheme)}
	for _, token := range []*bool{nil, ptr.To(false), ptr.To(true)} {
		obj := fixture()
		obj.Spec.Template.Spec.AutomountServiceAccountToken = token
		// An already defaulted object receives no redundant patch.
		response := handler.Handle(context.Background(), request(t, obj, nil))
		require.True(t, response.Allowed)
		require.Empty(t, response.Patches)
		obj.Spec.Template.Spec.RestartPolicy = ""
		obj.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{}}
		for _, old := range []*agents.SessionService{nil, fixture()} {
			response = handler.Handle(context.Background(), request(t, obj, old))
			require.True(t, response.Allowed)
			require.NotEmpty(t, response.Patches)
		}
		patchData, err := json.Marshal(response.Patches)
		require.NoError(t, err)
		patch, err := jsonpatch.DecodePatch(patchData)
		require.NoError(t, err)
		original, err := json.Marshal(obj)
		require.NoError(t, err)
		mutated, err := patch.Apply(original)
		require.NoError(t, err)
		actual := &agents.SessionService{}
		require.NoError(t, json.Unmarshal(mutated, actual))
		require.Equal(t, token, actual.Spec.Template.Spec.AutomountServiceAccountToken)
		require.Equal(t, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, actual.Spec.VolumeClaimTemplates[0].Spec.AccessModes)
		require.Equal(t, corev1.PersistentVolumeFilesystem, *actual.Spec.VolumeClaimTemplates[0].Spec.VolumeMode)
		require.Empty(t, handler.Handle(context.Background(), request(t, actual, nil)).Patches)
		// Explicit token selection belongs to the template, not admission policy.
		for _, patch := range response.Patches {
			require.NotContains(t, patch.Path, "automountServiceAccountToken")
		}
	}
}

func TestGeneratedWebhookRegistration(t *testing.T) {
	data, err := os.ReadFile("../../../config/webhook/manifests.yaml")
	require.NoError(t, err)
	expected := map[string]string{"md-ssvc.kb.io": (&Defaulter{}).Path(), "v-ssvc.kb.io": (&ValidatingHandler{}).Path()}
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		// Mutating and validating configurations share these registration fields.
		var config struct {
			Webhooks []admissionregistrationv1.ValidatingWebhook `json:"webhooks"`
		}
		err := decoder.Decode(&config)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		for _, webhook := range config.Webhooks {
			path, ok := expected[webhook.Name]
			if !ok {
				continue
			}
			require.NotNil(t, webhook.ClientConfig.Service)
			require.Equal(t, path, ptr.Deref(webhook.ClientConfig.Service.Path, ""))
			require.Equal(t, admissionregistrationv1.Fail, ptr.Deref(webhook.FailurePolicy, admissionregistrationv1.Ignore))
			require.Equal(t, admissionregistrationv1.SideEffectClassNone, ptr.Deref(webhook.SideEffects, admissionregistrationv1.SideEffectClassUnknown))
			require.Len(t, webhook.Rules, 1)
			require.Equal(t, []string{"agents.kruise.io"}, webhook.Rules[0].APIGroups)
			require.Equal(t, []string{"v1alpha1"}, webhook.Rules[0].APIVersions)
			require.Equal(t, []string{"sessionservices"}, webhook.Rules[0].Resources)
			require.ElementsMatch(t, []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update}, webhook.Rules[0].Operations)
			delete(expected, webhook.Name)
		}
	}
	require.Empty(t, expected, "SessionService webhook entries missing from generated configuration")
}
