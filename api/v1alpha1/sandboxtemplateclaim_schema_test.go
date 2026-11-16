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
)

func TestSandboxTemplateClaimCRDSchema(t *testing.T) {
	data, err := os.ReadFile("../../config/crd/bases/agents.kruise.io_sandboxtemplateclaims.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var external apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(data, &external); err != nil {
		t.Fatal(err)
	}
	var native apiextensions.CustomResourceDefinition
	if err := apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(&external, &native, nil); err != nil {
		t.Fatal(err)
	}
	native.Status.StoredVersions = []string{"v1alpha1"}
	if errs := crdvalidation.ValidateCustomResourceDefinition(context.Background(), &native); len(errs) != 0 {
		t.Fatalf("Claim schema invalid: %v", errs)
	}
	properties := native.Spec.Validation.OpenAPIV3Schema.Properties
	for _, name := range []string{"spec", "status", "defaultVersionRef"} {
		if _, ok := properties[name]; ok {
			t.Fatalf("Claim schema exposes %s", name)
		}
	}
	structural, err := schema.NewStructural(native.Spec.Validation.OpenAPIV3Schema)
	if err != nil {
		t.Fatal(err)
	}
	validator := cel.NewValidator(structural, true, celconfig.PerCallLimit)
	for _, test := range []struct {
		name  string
		valid bool
	}{{"a", true}, {"1", true}, {"python-agent", true}, {strings.Repeat("a", 63), true}, {"Upper", false}, {"has.dot", false}, {"-bad", false}, {strings.Repeat("a", 64), false}} {
		t.Run(test.name, func(t *testing.T) {
			obj := map[string]interface{}{"metadata": map[string]interface{}{"name": test.name}}
			errs, _ := validator.Validate(context.Background(), field.NewPath(""), structural, obj, nil, celconfig.RuntimeCELCostBudget)
			if (len(errs) == 0) != test.valid {
				t.Fatalf("valid=%v errors=%v", test.valid, errs)
			}
		})
	}
}

func TestSandboxTemplateWarmPoolSchema(t *testing.T) {
	_, crd := loadSandboxTemplateCRD(t)
	structural, err := schema.NewStructural(crd.Spec.Validation.OpenAPIV3Schema)
	if err != nil {
		t.Fatal(err)
	}
	warm := structural.Properties["spec"].Properties["buildOptions"].Properties["warmPool"]
	if warm.Properties["size"].Default.Object != nil || warm.Properties["enabled"].Default.Object != nil {
		t.Fatal("presence-sensitive enabled/size received admission defaults")
	}
	validator := cel.NewValidator(structural, true, celconfig.PerCallLimit)
	auto := func() map[string]interface{} {
		return map[string]interface{}{"maxReplicas": int64(10), "capacityPolicy": map[string]interface{}{"targetAvailable": int64(2)}}
	}
	tests := []struct {
		name  string
		warm  map[string]interface{}
		valid bool
	}{
		{"default declaration", map[string]interface{}{}, true},
		{"disabled declaration", map[string]interface{}{"enabled": false}, true},
		{"disabled explicit zero", map[string]interface{}{"enabled": false, "size": int64(0)}, false},
		{"disabled autoscaler", map[string]interface{}{"enabled": false, "autoscaler": auto()}, false},
		{"automatic default min", map[string]interface{}{"size": int64(0), "autoscaler": auto()}, true},
		{"initial capacity above max", map[string]interface{}{"size": int64(11), "autoscaler": auto()}, false},
		{"empty autoscaler", map[string]interface{}{"autoscaler": map[string]interface{}{}}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			obj := map[string]interface{}{"spec": map[string]interface{}{"buildOptions": map[string]interface{}{"warmPool": test.warm}}}
			defaulting.Default(obj, structural)
			errs, _ := validator.Validate(context.Background(), field.NewPath(""), structural, obj, nil, celconfig.RuntimeCELCostBudget)
			if (len(errs) == 0) != test.valid {
				t.Fatalf("valid=%v errors=%v", test.valid, errs)
			}
			if test.name == "disabled declaration" {
				if _, ok := test.warm["size"]; ok {
					t.Fatal("missing disabled size became explicit zero")
				}
			}
		})
	}
}
