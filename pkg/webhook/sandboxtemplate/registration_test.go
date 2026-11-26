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

package sandboxtemplate

import (
	"bytes"
	"errors"
	"io"
	"os"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	admissionapi "k8s.io/api/admission/v1"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	"sigs.k8s.io/yaml"

	"github.com/openkruise/agents/pkg/cache/controllers"
)

func TestGeneratedTemplateAdmissionConfiguration(t *testing.T) {
	data, err := os.ReadFile("../../../config/webhook/manifests.yaml")
	require.NoError(t, err)
	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	statusRegistered := false
	for {
		var configuration admissionv1.ValidatingWebhookConfiguration
		err := decoder.Decode(&configuration)
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		for _, webhook := range configuration.Webhooks {
			if webhook.ClientConfig.Service == nil || webhook.ClientConfig.Service.Path == nil {
				continue
			}
			path := *webhook.ClientConfig.Service.Path
			require.NotEqual(t, "/default-sandboxtemplateclaim", path)
			if configuration.Kind != "ValidatingWebhookConfiguration" {
				continue
			}
			require.NotEqual(t, "/validate-sandboxtemplateclaim", path)
			if path == "/validate-sandboxtemplate" {
				for _, rule := range webhook.Rules {
					if slices.Contains(rule.Resources, "sandboxtemplates/status") && slices.Contains(rule.Operations, admissionv1.Update) {
						statusRegistered = true
					}
				}
			}
		}
	}
	require.True(t, statusRegistered)
	require.Len(t, GetHandlerGetters(), 2)

	data, err = os.ReadFile("../../../config/rbac/role.yaml")
	require.NoError(t, err)
	var role rbacv1.ClusterRole
	require.NoError(t, yaml.Unmarshal(data, &role))
	for _, rule := range role.Rules {
		require.NotContains(t, rule.Resources, "sandboxtemplateclaims")
		require.NotContains(t, rule.Resources, "sandboxtemplateclaims/finalizers")
	}
	_, err = os.Stat("../../../config/crd/bases/agents.kruise.io_sandboxtemplateclaims.yaml")
	require.True(t, os.IsNotExist(err), "%v", err)
}

func TestTemplateHandlerGetters(t *testing.T) {
	builder, err := controllers.NewMockManagerBuilder(t)
	require.NoError(t, err)
	mgr := builder.Build()
	paths := []string{"/default-sandboxtemplate", "/validate-sandboxtemplate"}
	getters := GetHandlerGetters()
	require.Len(t, getters, len(paths))
	for i, getter := range getters {
		t.Run(paths[i], func(t *testing.T) {
			handler := getter(mgr)
			require.Equal(t, paths[i], handler.Path())
			require.True(t, handler.Enabled())
			response := handler.Handle(t.Context(), admission.Request{AdmissionRequest: admissionapi.AdmissionRequest{Operation: admissionapi.Create, Object: runtime.RawExtension{Raw: []byte("{")}}})
			require.False(t, response.Allowed)
			require.EqualValues(t, 400, response.Result.Code)
		})
	}
}
