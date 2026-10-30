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
package securityprofile_test

import (
	"context"
	"os"
	"strings"
	"testing"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	crdvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"
)

func TestTLSCRDSchema(t *testing.T) {
	for _, resource := range []string{"securityprofiles", "globalsecurityprofiles"} {
		t.Run(resource, func(t *testing.T) {
			data, err := os.ReadFile("../../../config/crd/bases/agents.kruise.io_" + resource + ".yaml")
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
			if errs := crdvalidation.ValidateCustomResourceDefinition(context.Background(), &crd); len(errs) != 0 {
				t.Fatalf("CRD admission: %v", errs)
			}
			t.Run("sample", func(t *testing.T) {
				data, err := os.ReadFile("../../../config/samples/agents_v1alpha1_securityprofile_tls.yaml")
				if err != nil {
					t.Fatal(err)
				}
				var obj map[string]interface{}
				if err := yaml.UnmarshalStrict(data, &obj); err != nil {
					t.Fatal(err)
				}
				obj["kind"] = external.Spec.Names.Kind
				if resource == "globalsecurityprofiles" {
					delete(obj["metadata"].(map[string]interface{}), "namespace")
				}
				root := crd.Spec.Validation.OpenAPIV3Schema
				structural, err := schema.NewStructural(root)
				if err != nil {
					t.Fatal(err)
				}
				defaulting.Default(obj, structural)
				validator, _, err := validation.NewSchemaValidator(root)
				if err != nil {
					t.Fatal(err)
				}
				if errs := validation.ValidateCustomResource(field.NewPath("profile"), obj, validator); len(errs) != 0 {
					t.Fatal(errs)
				}
			})
			rule := crd.Spec.Validation.OpenAPIV3Schema.Properties["spec"].Properties["rules"].Items.Schema
			structural, err := schema.NewStructural(rule)
			if err != nil {
				t.Fatal(err)
			}
			validator, _, err := validation.NewSchemaValidator(rule)
			if err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct{ name, match, actions, wantError string }{
				{"deny", `[{"domains":["blocked.example.com"],"ports":[443,8443]}]`, `{"tls":{"action":"Deny"}}`, ""},
				{"passthrough", `[{"domains":["private.example.com"],"ports":[443]}]`, `{"tls":{"action":"Passthrough"}}`, ""},
				{"unrestricted ports", `[{"domains":["*"]}]`, `{"tls":{"action":"Deny"}}`, ""},
				{"empty ports", `[{"domains":["*.example.com"],"ports":[]}]`, `{"tls":{"action":"Deny"}}`, ""},
				{"port boundaries", `[{"domains":["a"],"ports":[1,65535]}]`, `{"tls":{"action":"Deny"}}`, ""},
				{"independent matches", `[{"domains":["a"],"ports":[443]},{"domains":["b"],"ports":[8443]}]`, `{"tls":{"action":"Deny"}}`, ""},
				{"bypass false", `[{"domains":["a"]}]`, `{"tls":{"action":"Deny"},"bypass":false}`, ""},
				{"missing action", `[{"domains":["a"]}]`, `{"tls":{}}`, "action"},
				{"unknown action", `[{"domains":["a"]}]`, `{"tls":{"action":"Terminate"}}`, "Unsupported value"},
				{"empty action", `[{"domains":["a"]}]`, `{"tls":{"action":""}}`, "Unsupported value"},
				{"missing domains", `[{}]`, `{"tls":{"action":"Deny"}}`, "domains"},
				{"empty domains", `[{"domains":[]}]`, `{"tls":{"action":"Deny"}}`, "domains"},
				{"no match", `[]`, `{"tls":{"action":"Deny"}}`, "match"},
				{"zero port", `[{"domains":["a"],"ports":[0]}]`, `{"tls":{"action":"Deny"}}`, "ports"},
				{"large port", `[{"domains":["a"],"ports":[65536]}]`, `{"tls":{"action":"Deny"}}`, "ports"},
				{"existing HTTP rule", `[{"domains":["a"],"schemes":["https"],"paths":[{"value":"/health"}],"methods":["GET"],"headers":[{"name":"x-foo","value":"bar"}],"queryParams":[{"name":"q","value":"v"}]}]`, `{"block":{}}`, ""},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var obj map[string]interface{}
					if err := yaml.UnmarshalStrict([]byte(`{"name":"test","match":`+tc.match+`,"actions":`+tc.actions+`}`), &obj); err != nil {
						t.Fatal(err)
					}
					defaulting.Default(obj, structural)
					errs := validation.ValidateCustomResource(field.NewPath("rule"), obj, validator)
					if tc.wantError == "" {
						if len(errs) != 0 {
							t.Fatal(errs)
						}
					} else if len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), tc.wantError) {
						t.Fatalf("errors = %v, want %q", errs, tc.wantError)
					}
				})
			}
		})
	}
}
