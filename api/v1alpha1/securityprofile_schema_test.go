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
	"encoding/json"
	"os"
	"testing"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"
)

func TestSecurityProfileTCPRulesSchema(t *testing.T) {
	tests := []struct {
		name      string
		rules     string
		wantError bool
	}{
		{name: "omitted rules"},
		{name: "empty rules", rules: `[]`},
		{name: "allow IPv4", rules: `[{"name":"database","match":[{"cidrs":["10.20.0.0/16"],"ports":[5432]}],"action":"Allow"}]`},
		{name: "deny IPv6", rules: `[{"name":"deny","match":[{"cidrs":["::/0"]}],"action":"Deny"}]`},
		{name: "single IPv4", rules: `[{"name":"host","match":[{"cidrs":["140.205.162.5"]}],"action":"Allow"}]`},
		{name: "single IPv6", rules: `[{"name":"host","match":[{"cidrs":["2001:db8::1"]}],"action":"Deny"}]`},
		{name: "match all", rules: `[{"name":"deny","match":[{}],"action":"Deny"}]`},
		{name: "lowercase action", rules: `[{"name":"deny","match":[{}],"action":"deny"}]`, wantError: true},
		{name: "missing action", rules: `[{"name":"deny","match":[{}]}]`, wantError: true},
		{name: "empty name", rules: `[{"name":"","match":[{}],"action":"Deny"}]`, wantError: true},
		{name: "missing match", rules: `[{"name":"deny","action":"Deny"}]`, wantError: true},
		{name: "empty match", rules: `[{"name":"deny","match":[],"action":"Deny"}]`, wantError: true},
		{name: "port zero", rules: `[{"name":"deny","match":[{"ports":[0]}],"action":"Deny"}]`, wantError: true},
		{name: "port too large", rules: `[{"name":"deny","match":[{"ports":[65536]}],"action":"Deny"}]`, wantError: true},
	}
	for _, kind := range []string{"securityprofiles", "globalsecurityprofiles"} {
		t.Run(kind, func(t *testing.T) {
			data, err := os.ReadFile("../../config/crd/bases/agents.kruise.io_" + kind + ".yaml")
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
			validator, _, err := validation.NewSchemaValidator(crd.Spec.Validation.OpenAPIV3Schema)
			if err != nil {
				t.Fatal(err)
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					obj := map[string]interface{}{"spec": map[string]interface{}{"selector": map[string]interface{}{}}}
					if tt.rules != "" {
						var rules interface{}
						if err := json.Unmarshal([]byte(tt.rules), &rules); err != nil {
							t.Fatal(err)
						}
						obj["spec"].(map[string]interface{})["tcpRules"] = rules
					}
					errs := validation.ValidateCustomResource(field.NewPath(""), obj, validator)
					if (len(errs) > 0) != tt.wantError {
						t.Fatalf("validation errors = %v, wantError = %v", errs, tt.wantError)
					}
				})
			}
		})
	}
}
