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

package v1alpha1_test

import (
	"context"
	"os"
	"strings"
	"testing"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	crdvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func loadSandboxTemplateCRD(t *testing.T) (*apiextensionsv1.CustomResourceDefinition, *apiextensions.CustomResourceDefinition) {
	t.Helper()
	data, err := os.ReadFile("../../config/crd/bases/agents.kruise.io_sandboxtemplates.yaml")
	if err != nil {
		t.Fatal(err)
	}

	var external apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(data, &external); err != nil {
		t.Fatal(err)
	}
	var crd apiextensions.CustomResourceDefinition
	if err := apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(&external, &crd, nil); err != nil {
		t.Fatal(err)
	}
	crd.Status.StoredVersions = []string{"v1alpha1"}
	return &external, &crd
}

func TestSandboxTemplateCRDSchema(t *testing.T) {
	if agentsv1alpha1.SandboxTemplateBackendKubernetes != "k8s" {
		t.Fatalf("kubernetes backend wire value = %q", agentsv1alpha1.SandboxTemplateBackendKubernetes)
	}
	external, crd := loadSandboxTemplateCRD(t)
	if errs := crdvalidation.ValidateCustomResourceDefinition(context.Background(), crd); len(errs) != 0 {
		t.Fatalf("CRD admission: %v", errs)
	}

	version := external.Spec.Versions[0]
	if version.Subresources == nil || version.Subresources.Status == nil {
		t.Fatal("status subresource is missing")
	}

	root := crd.Spec.Validation.OpenAPIV3Schema
	spec := root.Properties["spec"]
	backend, ok := spec.Properties["backend"]
	if !ok {
		t.Fatal("spec.backend is missing")
	}
	if len(backend.Enum) != 1 || backend.Enum[0] != "k8s" {
		t.Fatalf("backend enum = %#v, want only k8s", backend.Enum)
	}
	buildOptions, ok := spec.Properties["buildOptions"]
	if !ok {
		t.Fatal("spec.buildOptions is missing")
	}
	for _, name := range []string{"fromTemplate", "steps", "startCommand", "readyCommand"} {
		if _, ok := buildOptions.Properties[name]; !ok {
			t.Errorf("spec.buildOptions.%s is missing", name)
		}
	}
	steps := buildOptions.Properties["steps"]
	cwd := steps.Items.Schema.Properties["cwd"]
	if len(cwd.XValidations) != 1 || cwd.XValidations[0].Rule != "self == '' || self.startsWith('/')" {
		t.Fatalf("cwd absolute-path validation = %#v", cwd.XValidations)
	}

	for _, name := range []string{"templateBuildOptions", "imageCache", "businessProcess", "runCommands"} {
		if _, ok := spec.Properties[name]; ok {
			t.Errorf("legacy spec.%s must not be exposed", name)
		}
		if _, ok := buildOptions.Properties[name]; ok {
			t.Errorf("legacy spec.buildOptions.%s must not be exposed", name)
		}
	}

	status := root.Properties["status"]
	for _, name := range []string{"observedGeneration", "phase", "artifacts", "conditions"} {
		if _, ok := status.Properties[name]; !ok {
			t.Errorf("status.%s is missing", name)
		}
	}
	for _, name := range []string{"artifactRef", "message", "startTime", "readyTime"} {
		if _, ok := status.Properties[name]; ok {
			t.Errorf("legacy status.%s must not be exposed", name)
		}
	}

	structural, err := schema.NewStructural(root)
	if err != nil {
		t.Fatal(err)
	}
	obj := map[string]interface{}{
		"apiVersion": "agents.kruise.io/v1alpha1",
		"kind":       "SandboxTemplate",
		"metadata":   map[string]interface{}{"name": "python-agent"},
		"spec":       map[string]interface{}{},
	}
	defaulting.Default(obj, structural)
	got := obj["spec"].(map[string]interface{})["backend"]
	if got != "k8s" {
		t.Fatalf("default backend = %v, want k8s", got)
	}
}

func TestSandboxTemplateCRDCELRules(t *testing.T) {
	_, crd := loadSandboxTemplateCRD(t)
	structural, err := schema.NewStructural(crd.Spec.Validation.OpenAPIV3Schema)
	if err != nil {
		t.Fatal(err)
	}
	validator := cel.NewValidator(structural, true, celconfig.PerCallLimit)
	if validator == nil {
		t.Fatal("CRD carries no CEL rules")
	}

	command := map[string]interface{}{"command": "echo hi"}
	resource := func(spec map[string]interface{}) map[string]interface{} {
		return map[string]interface{}{"spec": spec}
	}
	buildOnly := func(buildOptions map[string]interface{}) map[string]interface{} {
		return resource(map[string]interface{}{"buildOptions": buildOptions})
	}

	tests := []struct {
		name    string
		obj     map[string]interface{}
		oldObj  map[string]interface{}
		wantErr string
	}{
		{name: "buildOptions with fromTemplate passes", obj: buildOnly(map[string]interface{}{"fromTemplate": "base"})},
		{name: "empty buildOptions passes", obj: buildOnly(map[string]interface{}{})},
		{name: "empty steps passes", obj: buildOnly(map[string]interface{}{"steps": []interface{}{}})},
		{name: "steps pass", obj: buildOnly(map[string]interface{}{"steps": []interface{}{command}})},
		{name: "startCommand passes", obj: buildOnly(map[string]interface{}{"startCommand": command})},
		{
			name:    "backend change on update is rejected",
			obj:     resource(map[string]interface{}{"backend": "beta"}),
			oldObj:  resource(map[string]interface{}{"backend": "k8s"}),
			wantErr: "backend is immutable",
		},
		{name: "unchanged backend on update passes", obj: resource(map[string]interface{}{"backend": "k8s"}), oldObj: resource(map[string]interface{}{"backend": "k8s"})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs, _ := validator.Validate(context.Background(), field.NewPath(""), structural, tt.obj, tt.oldObj, celconfig.RuntimeCELCostBudget)
			if tt.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("unexpected validation errors: %v", errs)
				}
				return
			}
			if len(errs) == 0 {
				t.Fatalf("expected error containing %q, got none", tt.wantErr)
			}
			for _, e := range errs {
				if strings.Contains(e.Error(), tt.wantErr) {
					return
				}
			}
			t.Fatalf("validation errors %v do not contain %q", errs, tt.wantErr)
		})
	}
}
