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
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	crdvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/listtype"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"
)

func TestOAuth2CRDSchema(t *testing.T) {
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

			root := crd.Spec.Validation.OpenAPIV3Schema
			t.Run("lark-cli example", func(t *testing.T) {
				data, err := os.ReadFile("../../../config/samples/agents_v1alpha1_securityprofile_oauth2.yaml")
				if err != nil {
					t.Fatal(err)
				}
				var obj map[string]interface{}
				if err := yaml.UnmarshalStrict(data, &obj); err != nil {
					t.Fatal(err)
				}
				obj["kind"] = external.Spec.Names.Kind
				structural, err := schema.NewStructural(root)
				if err != nil {
					t.Fatal(err)
				}
				defaulting.Default(obj, structural)
				validator, _, err := validation.NewSchemaValidator(root)
				if err != nil {
					t.Fatal(err)
				}
				errs := validation.ValidateCustomResource(field.NewPath("profile"), obj, validator)
				if len(errs) != 0 {
					t.Fatal(errs)
				}
			})
			rules := root.Properties["spec"].Properties["rules"]
			transformation := rules.Items.Schema.Properties["actions"].Properties["tokenTransformation"]
			oauth := transformation.Properties["oauth2"]
			if _, ok := oauth.Properties["customResponse"]; !ok {
				t.Fatal("customResponse must be alongside operation")
			}
			if _, ok := oauth.Properties["requestFormat"]; ok {
				t.Fatal("requestFormat must not be exposed")
			}
			for role, branch := range oauth.Properties["operation"].Properties {
				if _, ok := branch.Properties["customResponse"]; ok {
					t.Fatalf("%s must not duplicate customResponse", role)
				}
			}
			structural, err := schema.NewStructural(&transformation)
			if err != nil {
				t.Fatal(err)
			}
			validator, _, err := validation.NewSchemaValidator(&transformation)
			if err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				name      string
				config    string
				wantError string
			}{
				{name: "device minimum", config: `{"operation":{"deviceAuthorization":{}}}`},
				{name: "token minimum", config: `{"operation":{"token":{}}}`},
				{name: "resource minimum", config: `{"operation":{"resource":{}}}`},
				{name: "device errors", config: `{"operation":{"deviceAuthorization":{}},"customResponse":{"error":{"code":20050},"authorizationRequired":{"code":20026}}}`},
				{name: "resource errors", config: `{"operation":{"resource":{}},"customResponse":{"error":{"code":20050},"authorizationRequired":{"code":99991663}}}`},
				{name: "token extensions", config: `{"operation":{"token":{}},"customResponse":{"success":{"code":0,"data":{"public":true},"extra":[1,"x",null]},"error":{"code":20050},"authorizationRequired":{"code":20026}}}`},
				{name: "grant set", config: `{"operation":{"token":{"allowedGrantTypes":["urn:ietf:params:oauth:grant-type:device_code","refresh_token"]}}}`},
				{name: "empty grant set", config: `{"operation":{"token":{"allowedGrantTypes":[]}}}`},
				{name: "extension grant", config: `{"operation":{"token":{"allowedGrantTypes":["https://example.com/grants/custom"]}}}`},
				{name: "empty grant string", config: `{"operation":{"token":{"allowedGrantTypes":[""]}}}`, wantError: "allowedGrantTypes"},
				{name: "duplicate grants", config: `{"operation":{"token":{"allowedGrantTypes":["refresh_token","refresh_token"]}}}`, wantError: "Duplicate"},
				{name: "operation missing", config: `{}`, wantError: "operation"},
				{name: "non-object response", config: `{"operation":{"token":{}},"customResponse":{"success":42}}`, wantError: "success"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var config map[string]interface{}
					if err := json.Unmarshal([]byte(tc.config), &config); err != nil {
						t.Fatal(err)
					}
					obj := map[string]interface{}{
						"type": "OAuth2", "oauth2": config,
						"credentialRef": map[string]interface{}{"credentialProvider": map[string]interface{}{"name": "provider"}},
					}
					defaulting.Default(obj, structural)
					errs := validation.ValidateCustomResource(field.NewPath("tokenTransformation"), obj, validator)
					errs = append(errs, listtype.ValidateListSetsAndMaps(field.NewPath("tokenTransformation"), structural, obj)...)
					if tc.wantError != "" {
						if len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), tc.wantError) {
							t.Fatalf("want error containing %q, got %v", tc.wantError, errs)
						}
						return
					}
					if len(errs) != 0 {
						t.Fatal(errs)
					}
					operation := config["operation"].(map[string]interface{})
					if len(operation) != 1 {
						t.Fatalf("defaulting created unselected operation: %v", operation)
					}
					if device, ok := operation["deviceAuthorization"].(map[string]interface{}); ok {
						if len(device) != 0 {
							t.Fatalf("deviceAuthorization must remain an empty object: %v", device)
						}
					}
					encoded, err := json.Marshal(config["customResponse"])
					if err != nil {
						t.Fatal(err)
					}
					pruning.Prune(obj, structural, false)
					var expected interface{}
					if err := json.Unmarshal(encoded, &expected); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(config["customResponse"], expected) {
						t.Fatal("pruning lost application response fields")
					}
				})
			}
			for _, tc := range []struct {
				name string
				obj  string
			}{
				{name: "existing API key", obj: `{"type":"ApiKey","apiKey":{"valueTemplate":"Bearer {{ .Token }}"}}`},
				{name: "existing STS", obj: `{"type":"AliyunSTS"}`},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var obj map[string]interface{}
					if err := json.Unmarshal([]byte(tc.obj), &obj); err != nil {
						t.Fatal(err)
					}
					obj["credentialRef"] = map[string]interface{}{"credentialProvider": map[string]interface{}{"name": "provider"}}
					defaulting.Default(obj, structural)
					errs := validation.ValidateCustomResource(field.NewPath("tokenTransformation"), obj, validator)
					if len(errs) != 0 {
						t.Fatal(errs)
					}
				})
			}
		})
	}
}
