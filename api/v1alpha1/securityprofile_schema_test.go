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
	"encoding/json"
	"os"
	"testing"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	crdvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

func TestSecurityProfileProxySchema(t *testing.T) {
	tests := []struct {
		name, actions string
		wantError     bool
	}{
		{"omitted", `{}`, false},
		{"http", `{"proxy":{"url":"http://proxy.example:80"}}`, false},
		{"https root", `{"proxy":{"url":"https://proxy.example:443/"}}`, false},
		{"socks IPv4", `{"proxy":{"url":"socks5://127.0.0.1:1080"}}`, false},
		{"IPv6", `{"proxy":{"url":"http://[2001:db8::1]:65535"}}`, false},
		{"bypass false", `{"proxy":{"url":"http://proxy:80"},"bypass":false}`, false},
		{"header", `{"proxy":{"url":"http://proxy:80"},"headerManipulation":{"set":[{"name":"x-test","value":"yes"}]}}`, false},
		{"uppercase and trailing dot", `{"proxy":{"url":"HTTPS://Proxy.Example.:443"}}`, false},
		{"mapped IPv6", `{"proxy":{"url":"socks5://[::ffff:192.0.2.1]:1080"}}`, false},
		{"leading zero port", `{"proxy":{"url":"http://proxy:00080"}}`, false},
		{"IPv6 loopback", `{"proxy":{"url":"http://[::1]:80"}}`, false},
		{"IPv6 unspecified", `{"proxy":{"url":"http://[::]:80"}}`, false},
		{"IPv6 full", `{"proxy":{"url":"http://[1:2:3:4:5:6:7:8]:80"}}`, false},
		{"IPv6 trailing compression", `{"proxy":{"url":"http://[2001:db8::]:80"}}`, false},
		{"IPv6 full IPv4 tail", `{"proxy":{"url":"http://[1:2:3:4:5:6:192.0.2.1]:80"}}`, false},
		{"IPv6 compressed IPv4 tail", `{"proxy":{"url":"http://[1:2::3:192.0.2.1]:80"}}`, false},
		{"callout", `{"proxy":{"url":"http://proxy:80"},"httpCallout":{"provider":{"name":"scanner"},"request":{}}}`, false},
		{"MCP", `{"proxy":{"url":"http://proxy:80"},"mcpToolPolicy":{"defaultAction":"deny","rules":[{"method":"tools/call","action":"allow"}]}}`, false},
		{"empty proxy", `{"proxy":{}}`, true},
		{"empty URL", `{"proxy":{"url":""}}`, true},
		{"direct", `{"proxy":{"url":"direct"}}`, true},
		{"default", `{"proxy":{"url":"default"}}`, true},
		{"scheme", `{"proxy":{"url":"ftp://proxy:80"}}`, true},
		{"port missing", `{"proxy":{"url":"http://proxy"}}`, true},
		{"zero port", `{"proxy":{"url":"http://proxy:0"}}`, true},
		{"large port", `{"proxy":{"url":"http://proxy:65536"}}`, true},
		{"userinfo", `{"proxy":{"url":"http://user:pass@proxy:80"}}`, true},
		{"path", `{"proxy":{"url":"http://proxy:80/api"}}`, true},
		{"query", `{"proxy":{"url":"http://proxy:80?"}}`, true},
		{"fragment", `{"proxy":{"url":"http://proxy:80#"}}`, true},
		{"bad IPv6", `{"proxy":{"url":"http://[:::1]:80"}}`, true},
		{"IPv6 too many groups", `{"proxy":{"url":"http://[1:2:3:4:5:6:7:8:9]:80"}}`, true},
		{"IPv6 double compression", `{"proxy":{"url":"http://[1::2::3]:80"}}`, true},
		{"IPv6 invalid IPv4 tail", `{"proxy":{"url":"http://[::ffff:999.0.0.1]:80"}}`, true},
		{"IPv6 zone", `{"proxy":{"url":"http://[fe80::1%25eth0]:80"}}`, true},
		{"non-numeric port", `{"proxy":{"url":"http://proxy:abc"}}`, true},
		{"empty userinfo", `{"proxy":{"url":"http://@proxy:80"}}`, true},
		{"escaped path", `{"proxy":{"url":"http://proxy:80/%2f"}}`, true},
		{"actions array", `[]`, true},
		{"space", `{"proxy":{"url":"http://proxy name:80"}}`, true},
		{"block", `{"proxy":{"url":"http://proxy:80"},"block":{}}`, true},
		{"bypass", `{"proxy":{"url":"http://proxy:80"},"bypass":true}`, true},
		{"tls", `{"proxy":{"url":"http://proxy:80"},"tls":{"action":"Passthrough"}}`, true},
		{"existing block", `{"block":{}}`, false},
		{"existing tls", `{"tls":{"action":"Deny"}}`, false},
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
			crd.Status.StoredVersions = []string{"v1alpha1"}
			if errs := crdvalidation.ValidateCustomResourceDefinition(context.Background(), &crd); len(errs) != 0 {
				t.Fatalf("CRD admission: %v", errs)
			}
			root := crd.Spec.Validation.OpenAPIV3Schema
			validator, _, err := validation.NewSchemaValidator(root)
			if err != nil {
				t.Fatal(err)
			}
			structural, err := schema.NewStructural(root)
			if err != nil {
				t.Fatal(err)
			}
			celValidator := cel.NewValidator(structural, true, celconfig.PerCallLimit)
			if celValidator == nil {
				t.Fatal("missing CEL validator")
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					var actions interface{}
					if err := json.Unmarshal([]byte(tt.actions), &actions); err != nil {
						t.Fatal(err)
					}
					obj := map[string]interface{}{"spec": map[string]interface{}{"selector": map[string]interface{}{}, "rules": []interface{}{map[string]interface{}{"name": "test", "match": []interface{}{map[string]interface{}{"domains": []interface{}{"example.com"}}}, "actions": actions}}}}
					errs := validation.ValidateCustomResource(field.NewPath(""), obj, validator)
					celErrs, _ := celValidator.Validate(context.Background(), field.NewPath(""), structural, obj, nil, celconfig.RuntimeCELCostBudget)
					errs = append(errs, celErrs...)
					if (len(errs) > 0) != tt.wantError {
						t.Fatalf("errors = %v, wantError = %v", errs, tt.wantError)
					}
				})
			}
		})
	}
}

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
