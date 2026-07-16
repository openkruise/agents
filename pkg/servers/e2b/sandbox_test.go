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

package e2b

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/servers/e2b/adapters"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

func TestReplacer(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "replaces ws scheme", in: "ws://localhost:9222/devtools/browser/12345678-1234-1234-1234-123456789012", want: "ws://hello-world/devtools/browser/12345678-1234-1234-1234-123456789012"},
		{name: "replaces wss scheme", in: "wss://localhost:9222/devtools/browser/12345678-1234-1234-1234-123456789012", want: "ws://hello-world/devtools/browser/12345678-1234-1234-1234-123456789012"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := browserWebSocketReplacer.ReplaceAllString(tt.in, "ws://hello-world")
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveSandboxDomain(t *testing.T) {
	tests := []struct {
		name             string
		configuredDomain string
		host             string
		path             string
		expect           string
		expectError      string
	}{
		{name: "configured domain bypasses empty host and is preserved", configuredDomain: "API.Static.example.com.", path: "/sandboxes", expect: "API.Static.example.com."},
		{name: "native request resolves domain", host: "API.example.com:8443", path: "/sandboxes", expect: "example.com:8443"},
		{name: "dynamic domain rejects empty host", path: "/sandboxes", expectError: "cannot resolve sandbox domain: empty host"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller := &Controller{
				domain:  tt.configuredDomain,
				adapter: adapters.NewE2BAdapter(0),
			}
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req.Host = tt.host

			got, apiErr := controller.resolveSandboxDomain(req)
			if tt.expectError != "" {
				require.NotNil(t, apiErr)
				assert.Equal(t, http.StatusBadRequest, apiErr.Code)
				assert.Contains(t, apiErr.Message, tt.expectError)
				return
			}
			require.Nil(t, apiErr)
			assert.Equal(t, tt.expect, got)
		})
	}
}

func TestGetNamespaceOfUser(t *testing.T) {
	controller := &Controller{}

	tests := []struct {
		name      string
		user      *models.CreatedTeamAPIKey
		namespace string
	}{
		{name: "admin team keeps cluster scope", user: &models.CreatedTeamAPIKey{Team: models.AdminTeam()}},
		{name: "normal team maps to team namespace", user: &models.CreatedTeamAPIKey{Team: &models.Team{Name: "team-a"}}, namespace: "team-a"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.namespace, controller.getNamespaceOfUser(tt.user))
		})
	}
}

func TestConvertToE2BSandboxPodIPMetadata(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		podIP       string
		expectValue string
	}{
		{name: "disabled does not return pod ip metadata", podIP: "1.2.3.4"},
		{name: "enabled returns pod ip metadata", annotations: map[string]string{models.ExtensionKeyReturnPodIP: agentsv1alpha1.True}, podIP: "1.2.3.4", expectValue: "1.2.3.4"},
		{name: "enabled skips pod ip metadata when pod ip is empty", annotations: map[string]string{models.ExtensionKeyReturnPodIP: agentsv1alpha1.True}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := &sandboxcr.Sandbox{
				Sandbox: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Annotations: tt.annotations,
					},
					Status: agentsv1alpha1.SandboxStatus{
						PodInfo: agentsv1alpha1.PodInfo{
							PodIP: tt.podIP,
						},
					},
				},
			}

			got := (&Controller{}).convertToE2BSandbox(sbx, "", "")
			value, exists := got.Metadata[models.MetadataKeyPodIP]
			assert.Equal(t, tt.expectValue != "", exists)
			if exists {
				assert.Equal(t, tt.expectValue, value)
			}
		})
	}
}

func TestE2BResource(t *testing.T) {
	tests := []struct {
		name       string
		resource   infra.SandboxResource
		wantCPU    int64
		wantMemory int64
		wantDisk   int64
	}{
		{
			name: "uses limit cpu and memory",
			resource: infra.SandboxResource{
				Requests: infra.ResourceList{CPUMilli: 1000, MemoryMB: 1024},
				Limits:   infra.ResourceList{CPUMilli: 4000, MemoryMB: 8192},
			},
			wantCPU:    4,
			wantMemory: 8192,
		},
		{
			name: "falls back to request resources",
			resource: infra.SandboxResource{
				Requests: infra.ResourceList{CPUMilli: 2000, MemoryMB: 2048, DiskSizeMB: 3072},
			},
			wantCPU:    2,
			wantMemory: 2048,
			wantDisk:   3072,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cpu, memory, disk := e2bResource(tt.resource)
			assert.Equal(t, tt.wantCPU, cpu)
			assert.Equal(t, tt.wantMemory, memory)
			assert.Equal(t, tt.wantDisk, disk)
		})
	}
}
