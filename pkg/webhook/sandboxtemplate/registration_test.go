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
	admissionv1 "k8s.io/api/admissionregistration/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

func TestGeneratedTemplateAdmissionConfiguration(t *testing.T) {
	data, err := os.ReadFile("../../../config/webhook/manifests.yaml")
	require.NoError(t, err)
	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	statusRegistered := false
	claimRegistered := false
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
			if path == "/validate-sandboxtemplateclaim" {
				claimRegistered = true
			}
			if path == "/validate-sandboxtemplate" {
				for _, rule := range webhook.Rules {
					if slices.Contains(rule.Resources, "sandboxtemplates/status") && slices.Contains(rule.Operations, admissionv1.Update) {
						statusRegistered = true
					}
				}
			}
		}
	}
	require.True(t, claimRegistered)
	require.True(t, statusRegistered)
	require.Len(t, GetHandlerGetters(), 3)

	data, err = os.ReadFile("../../../config/rbac/role.yaml")
	require.NoError(t, err)
	var role rbacv1.ClusterRole
	require.NoError(t, yaml.Unmarshal(data, &role))
	for _, verb := range []string{"get", "list", "watch"} {
		allowed := false
		for _, rule := range role.Rules {
			if slices.Contains(rule.APIGroups, "agents.kruise.io") && slices.Contains(rule.Resources, "sandboxtemplateclaims") && slices.Contains(rule.Verbs, verb) {
				allowed = true
			}
		}
		require.True(t, allowed, "Claim %s permission is missing", verb)
	}
}
