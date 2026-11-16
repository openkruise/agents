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

package templatevalidation

import (
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"

	api "github.com/openkruise/agents/api/v1alpha1"
)

func minimalDefinition() *api.SandboxTemplateSpec {
	return &api.SandboxTemplateSpec{Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "python:3.12"}}}}}
}
func automaticOptions() *api.SandboxTemplatePoolAutoscalerOptions {
	return &api.SandboxTemplatePoolAutoscalerOptions{MaxReplicas: 10, CapacityPolicy: &api.CapacityPolicy{TargetAvailable: intstr.FromInt32(2)}}
}
func TestNamesAndIDs(t *testing.T) {
	for _, test := range []struct {
		name  string
		valid bool
	}{{"a", true}, {"1", true}, {"python-agent", true}, {strings.Repeat("a", 63), true}, {"", false}, {"UPPER", false}, {"name.space", false}, {"-bad", false}, {"bad-", false}, {strings.Repeat("a", 64), false}, {"中文", false}} {
		t.Run(test.name, func(t *testing.T) {
			if (len(ValidateName(test.name, field.NewPath("name"))) == 0) != test.valid {
				t.Fatalf("unexpected validity %v", test)
			}
		})
	}
	for _, id := range []string{"a7f2k9m4p1q8r3s6t0v5", strings.Repeat("0", 20)} {
		if len(ValidateID(id, field.NewPath("id"))) != 0 {
			t.Fatal(id)
		}
	}
	for _, id := range []string{"short", strings.Repeat("A", 20), strings.Repeat("a", 19), strings.Repeat("a", 21), "a7f2k9m4p1q8r3s6t0v-"} {
		if len(ValidateID(id, field.NewPath("id"))) == 0 {
			t.Fatal(id)
		}
	}
}
func TestNormalizeCustomSpec(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*api.SandboxTemplateSpec)
		wantError string
		enabled   bool
		size      *int32
	}{
		{name: "default pool", enabled: true, size: ptr.To(int32(0))},
		{name: "empty pool", mutate: func(s *api.SandboxTemplateSpec) {
			s.BuildOptions = &api.SandboxTemplateBuildOptions{WarmPool: &api.SandboxTemplateWarmPoolOptions{}}
		}, enabled: true, size: ptr.To(int32(0))},
		{name: "disabled", mutate: func(s *api.SandboxTemplateSpec) {
			s.BuildOptions = &api.SandboxTemplateBuildOptions{WarmPool: &api.SandboxTemplateWarmPoolOptions{Enabled: ptr.To(false)}}
		}},
		{name: "fixed positive", mutate: func(s *api.SandboxTemplateSpec) {
			s.BuildOptions = &api.SandboxTemplateBuildOptions{WarmPool: &api.SandboxTemplateWarmPoolOptions{Size: ptr.To(int32(2))}}
		}, enabled: true, size: ptr.To(int32(2))},
		{name: "disabled explicit zero", mutate: func(s *api.SandboxTemplateSpec) {
			s.BuildOptions = &api.SandboxTemplateBuildOptions{WarmPool: &api.SandboxTemplateWarmPoolOptions{Enabled: ptr.To(false), Size: ptr.To(int32(0))}}
		}, wantError: "must be omitted"},
		{name: "disabled automatic", mutate: func(s *api.SandboxTemplateSpec) {
			s.BuildOptions = &api.SandboxTemplateBuildOptions{WarmPool: &api.SandboxTemplateWarmPoolOptions{Enabled: ptr.To(false), Autoscaler: automaticOptions()}}
		}, wantError: "must be omitted"},
		{name: "negative capacity", mutate: func(s *api.SandboxTemplateSpec) {
			s.BuildOptions = &api.SandboxTemplateBuildOptions{WarmPool: &api.SandboxTemplateWarmPoolOptions{Size: ptr.To(int32(-1))}}
		}, wantError: "nonnegative"},
		{name: "automatic below min", mutate: func(s *api.SandboxTemplateSpec) {
			a := automaticOptions()
			a.MinReplicas = 2
			s.BuildOptions = &api.SandboxTemplateBuildOptions{WarmPool: &api.SandboxTemplateWarmPoolOptions{Size: ptr.To(int32(0)), Autoscaler: a}}
		}, enabled: true, size: ptr.To(int32(0))},
		{name: "above max", mutate: func(s *api.SandboxTemplateSpec) {
			s.BuildOptions = &api.SandboxTemplateBuildOptions{WarmPool: &api.SandboxTemplateWarmPoolOptions{Size: ptr.To(int32(11)), Autoscaler: automaticOptions()}}
		}, wantError: "maxReplicas"},
		{name: "empty automatic", mutate: func(s *api.SandboxTemplateSpec) {
			s.BuildOptions = &api.SandboxTemplateBuildOptions{WarmPool: &api.SandboxTemplateWarmPoolOptions{Autoscaler: &api.SandboxTemplatePoolAutoscalerOptions{}}}
		}, wantError: "maxReplicas"},
		{name: "percentage without positive min", mutate: func(s *api.SandboxTemplateSpec) {
			a := automaticOptions()
			a.CapacityPolicy.TargetAvailable = intstr.FromString("70%")
			s.BuildOptions = &api.SandboxTemplateBuildOptions{WarmPool: &api.SandboxTemplateWarmPoolOptions{Autoscaler: a}}
		}, wantError: "minReplicas"},
		{name: "old commands rejected", mutate: func(s *api.SandboxTemplateSpec) {
			s.BuildOptions = &api.SandboxTemplateBuildOptions{Steps: []api.TemplateBuildCommand{{Command: "echo setup"}}}
		}, wantError: "not supported"},
		{name: "missing image", mutate: func(s *api.SandboxTemplateSpec) { s.Template.Spec.Containers[0].Image = "" }, wantError: "image"},
		{name: "unknown runtime", mutate: func(s *api.SandboxTemplateSpec) { s.Runtimes = []api.RuntimeConfig{{Name: "unknown"}} }, wantError: "runtimes"},
		{name: "duplicate runtime", mutate: func(s *api.SandboxTemplateSpec) { s.Runtimes = []api.RuntimeConfig{{Name: "csi"}, {Name: "csi"}} }, wantError: "Duplicate"},
		{name: "unsupported backend", mutate: func(s *api.SandboxTemplateSpec) { s.Backend = "substrate" }, wantError: "backend"},
		{name: "upgrade hook rejected", mutate: func(s *api.SandboxTemplateSpec) {
			s.Lifecycle = &api.SandboxLifecycle{PreUpgrade: &api.LifecycleAction{}}
		}, wantError: "preUpgrade"},
		{name: "probe missing container", mutate: func(s *api.SandboxTemplateSpec) {
			s.Probes = []api.Probe{{Name: "work", ContainerName: "missing", Probe: corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"check"}}}}}}
		}, wantError: "containerName"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := minimalDefinition()
			if test.mutate != nil {
				test.mutate(input)
			}
			before := input.DeepCopy()
			normalized, errs := NormalizeCustomSpec(input, field.NewPath("spec"))
			if !reflect.DeepEqual(input, before) {
				t.Fatal("normalization changed input")
			}
			if test.wantError != "" {
				if len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), test.wantError) {
					t.Fatalf("errors=%v want=%s", errs, test.wantError)
				}
				return
			}
			if len(errs) != 0 {
				t.Fatal(errs)
			}
			warm := normalized.BuildOptions.WarmPool
			if *warm.Enabled != test.enabled || !reflect.DeepEqual(warm.Size, test.size) {
				t.Fatalf("normalized pool=%+v", warm)
			}
			if normalized.Backend != "k8s" || normalized.Template.Spec.RestartPolicy != corev1.RestartPolicyAlways {
				t.Fatalf("missing definition defaults: %+v", normalized)
			}
		})
	}
}
func TestPVCInputAndLegacyValidation(t *testing.T) {
	input := minimalDefinition()
	input.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}
	input.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "data"}, Spec: corev1.PersistentVolumeClaimSpec{Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}}}
	for _, test := range []struct {
		name   string
		mutate func(*api.SandboxTemplateSpec)
		valid  bool
	}{{"name and spec", nil, true}, {"label rejected", func(s *api.SandboxTemplateSpec) { s.VolumeClaimTemplates[0].Labels = map[string]string{"team": "test"} }, false}, {"annotation rejected", func(s *api.SandboxTemplateSpec) {
		s.VolumeClaimTemplates[0].Annotations = map[string]string{"note": "test"}
	}, false}, {"duplicate names", func(s *api.SandboxTemplateSpec) {
		s.VolumeClaimTemplates = append(s.VolumeClaimTemplates, s.VolumeClaimTemplates[0])
	}, false}, {"unmounted claim", func(s *api.SandboxTemplateSpec) { s.Template.Spec.Containers[0].VolumeMounts = nil }, false}} {
		t.Run(test.name, func(t *testing.T) {
			s := input.DeepCopy()
			if test.mutate != nil {
				test.mutate(s)
			}
			out, errs := NormalizeCustomSpec(s, field.NewPath("spec"))
			if (len(errs) == 0) != test.valid {
				t.Fatalf("errors=%v", errs)
			}
			if out != nil && len(out.VolumeClaimTemplates[0].Spec.AccessModes) != 1 {
				t.Fatal("PVC defaults not applied")
			}
		})
	}
	legacy := input.DeepCopy()
	DefaultSpec(legacy)
	legacy.VolumeClaimTemplates[0].Labels = map[string]string{"legacy": "value"}
	legacy.BuildOptions = &api.SandboxTemplateBuildOptions{FromTemplate: "base", Steps: []api.TemplateBuildCommand{{Command: "run"}}}
	legacy.Runtimes = []api.RuntimeConfig{{Name: "legacy-runtime"}}
	if errs := ValidateSpec(*legacy, field.NewPath("spec")); len(errs) != 0 {
		t.Fatalf("legacy contract tightened: %v", errs)
	}
}
func TestManagedUpdateScope(t *testing.T) {
	original := &api.SandboxTemplate{ObjectMeta: metav1.ObjectMeta{Name: "a7f2k9m4p1q8r3s6t0v5", OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "SandboxTemplateClaim", Name: "python-agent", UID: "claim-uid", Controller: ptr.To(true)}}}, Spec: *minimalDefinition()}
	for _, test := range []struct {
		name   string
		mutate func(*api.SandboxTemplate)
		valid  bool
	}{{"unchanged", nil, true}, {"status only", func(s *api.SandboxTemplate) { s.Status.Phase = api.SandboxTemplatePhaseReady }, true}, {"spec immutable", func(s *api.SandboxTemplate) { s.Spec.Template.Spec.Containers[0].Image = "changed" }, false}, {"owner removal", func(s *api.SandboxTemplate) { s.OwnerReferences = nil }, false}, {"owner change", func(s *api.SandboxTemplate) { s.OwnerReferences[0].UID = "new-uid" }, false}} {
		t.Run(test.name, func(t *testing.T) {
			next := original.DeepCopy()
			if test.mutate != nil {
				test.mutate(next)
			}
			if (len(ValidateManagedUpdate(original, next, field.NewPath("spec"))) == 0) != test.valid {
				t.Fatal("incorrect update scope")
			}
		})
	}
	legacy := original.DeepCopy()
	legacy.OwnerReferences = nil
	updated := legacy.DeepCopy()
	updated.Spec.Template.Spec.Containers[0].Image = "changed"
	if errs := ValidateManagedUpdate(legacy, updated, field.NewPath("spec")); len(errs) != 0 {
		t.Fatalf("legacy update rejected: %v", errs)
	}
	if errs := ValidateManagedUpdate(legacy, original, field.NewPath("spec")); len(errs) == 0 {
		t.Fatal("adoption allowed")
	}
}

// API and controller callers may normalize the same informer-derived definition
// concurrently. Each result must own its maps, slices and pointer defaults.
func TestNormalizeCustomSpecConcurrent(t *testing.T) {
	input := minimalDefinition()
	input.Template.Labels = map[string]string{"source": "shared"}
	input.BuildOptions = &api.SandboxTemplateBuildOptions{WarmPool: &api.SandboxTemplateWarmPoolOptions{Size: ptr.To(int32(2)), Autoscaler: automaticOptions()}}
	before := input.DeepCopy()
	for i := 0; i < 8; i++ {
		t.Run("independent-copy", func(t *testing.T) {
			t.Parallel()
			normalized, errs := NormalizeCustomSpec(input, field.NewPath("spec"))
			if len(errs) != 0 {
				t.Fatal(errs)
			}
			normalized.Template.Labels["source"] = "changed"
			*normalized.BuildOptions.WarmPool.Size = 9
			if !reflect.DeepEqual(input, before) {
				t.Fatal("concurrent normalization mutated shared definition")
			}
		})
	}
}
