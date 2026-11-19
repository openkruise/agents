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
	for _, name := range []string{"spec", "status", "defaultVersionRef"} {
		if _, ok := native.Spec.Validation.OpenAPIV3Schema.Properties[name]; ok {
			t.Fatalf("Claim exposes %s", name)
		}
	}
	if external.Spec.Versions[0].Subresources != nil {
		t.Fatal("metadata-only Claim must not expose a status subresource")
	}
	structural, err := schema.NewStructural(native.Spec.Validation.OpenAPIV3Schema)
	if err != nil {
		t.Fatal(err)
	}
	validator := cel.NewValidator(structural, true, celconfig.PerCallLimit)
	if validator == nil {
		t.Fatal("Claim name validation is missing")
	}
	for _, tt := range []struct {
		name  string
		valid bool
	}{{"a", true}, {"1", true}, {"python-agent", true}, {strings.Repeat("a", 63), true}, {"", false}, {"Upper", false}, {"has.dot", false}, {"-bad", false}, {"bad-", false}, {strings.Repeat("a", 64), false}, {"tags", false}} {
		t.Run(tt.name, func(t *testing.T) {
			obj := map[string]interface{}{"metadata": map[string]interface{}{"name": tt.name}}
			errs, _ := validator.Validate(context.Background(), field.NewPath(""), structural, obj, nil, celconfig.RuntimeCELCostBudget)
			if (len(errs) == 0) != tt.valid {
				t.Fatalf("valid=%v errors=%v", tt.valid, errs)
			}
		})
	}
}
