/*
Copyright 2025.

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
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/api/v1alpha1"
	infracache "github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/proxy"
	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/servers/e2b/adapters"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

type listSandboxSetsSpy struct {
	infracache.Provider
	result    []*v1alpha1.SandboxSet
	err       error
	called    bool
	namespace string
}

func (s *listSandboxSetsSpy) ListSandboxSets(_ context.Context, opts infracache.ListSandboxSetsOptions) ([]*v1alpha1.SandboxSet, error) {
	s.called = true
	s.namespace = opts.Namespace
	return s.result, s.err
}

func TestDeleteTemplate(t *testing.T) {
	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
		Team: models.AdminTeam(),
	}
	teamAUser := &models.CreatedTeamAPIKey{
		ID:   uuid.New(),
		Key:  "team-a-key",
		Name: "team-a-user",
		Team: &models.Team{
			ID:   uuid.New(),
			Name: "team-a",
		},
	}

	tests := []struct {
		name                     string
		templateID               string
		setupTemplate            bool // whether to create checkpoint + template using CreateCheckpointAndTemplate
		setupSandboxSetNamespace string
		mockDeleteTemplate       error                     // mock error for DefaultDeleteSandboxTemplate
		user                     *models.CreatedTeamAPIKey // user for the request
		expectStatus             int
		expectError              bool
		expectErrorMessage       string
	}{
		{
			name:          "delete template successfully",
			templateID:    "test-tmpl-delete-success",
			setupTemplate: true,
			user:          user,
			expectStatus:  http.StatusNoContent,
		},
		{
			name:               "delete template with infra error",
			templateID:         "test-tmpl-delete-error",
			setupTemplate:      true,
			mockDeleteTemplate: fmt.Errorf("mock delete template error"),
			user:               user,
			expectStatus:       http.StatusInternalServerError,
			expectError:        true,
		},
		{
			name:          "user is nil returns unauthorized",
			templateID:    "test-tmpl-no-user",
			setupTemplate: false,
			user:          nil,
			expectStatus:  http.StatusUnauthorized,
			expectError:   true,
		},
		{
			name:                     "sandboxset-backed template delete is unsupported for team user",
			templateID:               "test-sbs-delete-unsupported",
			setupSandboxSetNamespace: "team-a",
			user:                     teamAUser,
			expectStatus:             http.StatusUnauthorized,
			expectError:              true,
			expectErrorMessage:       "SandboxSet-backed templates",
		},
		{
			name:                     "team user does not see sandboxset-backed template in another namespace",
			templateID:               "test-sbs-delete-other-namespace",
			setupSandboxSetNamespace: "team-b",
			user:                     teamAUser,
			expectStatus:             http.StatusNoContent,
		},
		{
			name:                     "sandboxset-backed template delete is unsupported for admin across namespaces",
			templateID:               "test-sbs-delete-unsupported-admin",
			setupSandboxSetNamespace: "team-a",
			user:                     user,
			expectStatus:             http.StatusUnauthorized,
			expectError:              true,
			expectErrorMessage:       "SandboxSet-backed templates",
		},
		{
			name:          "non-owner user returns 204 (idempotent)",
			templateID:    "test-tmpl-non-owner",
			setupTemplate: true,
			user: &models.CreatedTeamAPIKey{
				ID:   uuid.New(),
				Key:  "different-key",
				Name: "different-user",
			},
			expectStatus: http.StatusNoContent,
		},
		{
			name:          "template not found",
			templateID:    "test-tmpl-not-found",
			setupTemplate: false,
			user:          user,
			expectStatus:  http.StatusNoContent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller, fc, teardown := Setup(t)
			defer teardown()

			if tt.setupTemplate {
				_ = CreateCheckpointAndTemplate(t, controller, tt.templateID)
				// Set owner annotation on the checkpoint
				cp := &v1alpha1.Checkpoint{}
				err := fc.Get(t.Context(), ctrlclient.ObjectKey{Namespace: Namespace, Name: tt.templateID}, cp)
				require.NoError(t, err)
				if cp.Annotations == nil {
					cp.Annotations = map[string]string{}
				}
				cp.Annotations[v1alpha1.AnnotationOwner] = user.ID.String()
				err = fc.Update(t.Context(), cp)
				require.NoError(t, err)
			}
			if tt.setupSandboxSetNamespace != "" {
				cleanup := CreateSandboxPool(t, controller, tt.templateID, 0, CreateSandboxPoolOptions{
					Namespace: tt.setupSandboxSetNamespace,
				})
				defer cleanup()
			}

			// Set up decorator mock for template deletion
			if tt.mockDeleteTemplate != nil {
				orig := sandboxcr.DefaultDeleteSandboxTemplate
				sandboxcr.DefaultDeleteSandboxTemplate = func(ctx context.Context, c ctrlclient.Client, namespace, name string) error {
					return tt.mockDeleteTemplate
				}
				t.Cleanup(func() { sandboxcr.DefaultDeleteSandboxTemplate = orig })
			}

			req := NewRequest(t, nil, nil, map[string]string{
				"templateID": tt.templateID,
			}, tt.user)

			resp, apiErr := controller.DeleteTemplate(req)

			if tt.expectError {
				require.NotNil(t, apiErr)
				if apiErr.Code == 0 {
					apiErr.Code = http.StatusInternalServerError
				}
				assert.Equal(t, tt.expectStatus, apiErr.Code)
				if tt.expectErrorMessage != "" {
					assert.Contains(t, apiErr.Message, tt.expectErrorMessage)
				}
			} else {
				require.Nil(t, apiErr)
				assert.Equal(t, tt.expectStatus, resp.Code)
			}
		})
	}
}

func TestDeleteCheckpointNamespaceIsolationWithSameID(t *testing.T) {
	controller, fc, teardown := Setup(t)
	defer teardown()

	ownerID := uuid.New()
	teamAUser := &models.CreatedTeamAPIKey{
		ID:   ownerID,
		Key:  "team-a-key",
		Name: "team-a-user",
		Team: &models.Team{
			ID:   uuid.New(),
			Name: "team-a",
		},
	}
	adminUser := &models.CreatedTeamAPIKey{
		ID:   ownerID,
		Key:  "admin-key",
		Name: "admin-user",
		Team: models.AdminTeam(),
	}

	cleanupA := CreateCheckpointAndTemplateInNamespace(t, controller, "team-a", "shared-checkpoint", "shared-checkpoint-id", ownerID.String(), "team-a-sandbox", "2024-07-01T00:00:01Z")
	defer cleanupA()
	cleanupB := CreateCheckpointAndTemplateInNamespace(t, controller, "team-b", "shared-checkpoint", "shared-checkpoint-id", ownerID.String(), "team-b-sandbox", "2024-07-01T00:00:02Z")
	defer cleanupB()

	resp, apiErr := controller.DeleteTemplate(NewRequest(t, nil, nil, map[string]string{
		"templateID": "shared-checkpoint-id",
	}, teamAUser))
	require.Nil(t, apiErr)
	assert.Equal(t, http.StatusNoContent, resp.Code)

	require.Eventually(t, func() bool {
		cp := &v1alpha1.Checkpoint{}
		err := fc.Get(t.Context(), ctrlclient.ObjectKey{Namespace: "team-a", Name: "shared-checkpoint"}, cp)
		return apierrors.IsNotFound(err)
	}, time.Second, 10*time.Millisecond)

	teamBCP := &v1alpha1.Checkpoint{}
	require.NoError(t, fc.Get(t.Context(), ctrlclient.ObjectKey{Namespace: "team-b", Name: "shared-checkpoint"}, teamBCP))

	teamAResp, apiErr := controller.ListSnapshots(NewRequest(t, nil, nil, nil, teamAUser))
	require.Nil(t, apiErr)
	assert.Empty(t, teamAResp.Body)

	adminResp, apiErr := controller.ListSnapshots(NewRequest(t, nil, nil, nil, adminUser))
	require.Nil(t, apiErr)
	require.Len(t, adminResp.Body, 1)
	assert.Equal(t, "shared-checkpoint-id", adminResp.Body[0].SnapshotID)
}

// TestListTemplates tests the ListTemplates method
func TestListTemplates(t *testing.T) {
	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
		Team: models.AdminTeam(),
	}
	teamAUser := &models.CreatedTeamAPIKey{
		ID:   uuid.New(),
		Key:  "team-a-key",
		Name: "team-a-user",
		Team: &models.Team{
			ID:   uuid.New(),
			Name: "team-a",
		},
	}

	tests := []struct {
		name         string
		setupPools   func(t *testing.T, controller *Controller, fc ctrlclient.Client) []func()
		queryTeamID  string
		user         *models.CreatedTeamAPIKey
		expectStatus int
		expectError  bool
		expectCount  int
		validateFunc func(t *testing.T, templates []*models.TemplateInfo)
	}{
		{
			name: "admin lists templates across namespaces",
			setupPools: func(t *testing.T, controller *Controller, fc ctrlclient.Client) []func() {
				cleanup1 := CreateSandboxPool(t, controller, "test-pool-1", 2, CreateSandboxPoolOptions{Namespace: "team-a"})
				cleanup2 := CreateSandboxPool(t, controller, "test-pool-2", 1, CreateSandboxPoolOptions{Namespace: "team-b"})
				return []func(){cleanup1, cleanup2}
			},
			queryTeamID:  "",
			user:         user,
			expectStatus: http.StatusOK,
			expectError:  false,
			expectCount:  2,
			validateFunc: func(t *testing.T, templates []*models.TemplateInfo) {
				templateNames := make(map[string]bool)
				for _, tmpl := range templates {
					templateNames[tmpl.TemplateID] = true
					assert.NotEmpty(t, tmpl.TemplateID)
					assert.NotEmpty(t, tmpl.BuildID)
					assert.Equal(t, tmpl.TemplateID, tmpl.BuildID)
					assert.True(t, tmpl.Public)
					assert.NotEmpty(t, tmpl.Aliases)
					assert.NotEmpty(t, tmpl.Names)
					assert.Equal(t, "0.1.1", tmpl.EnvdVersion)
					assert.NotEmpty(t, tmpl.BuildStatus)
				}
				assert.True(t, templateNames["test-pool-1"])
				assert.True(t, templateNames["test-pool-2"])
			},
		},
		{
			name: "list templates ignores query teamID and uses caller team namespace",
			setupPools: func(t *testing.T, controller *Controller, fc ctrlclient.Client) []func() {
				cleanup1 := CreateSandboxPool(t, controller, "team-a-pool", 1, CreateSandboxPoolOptions{Namespace: "team-a"})
				cleanup2 := CreateSandboxPool(t, controller, "team-b-pool", 1, CreateSandboxPoolOptions{Namespace: "team-b"})
				return []func(){cleanup1, cleanup2}
			},
			queryTeamID:  "team-b",
			user:         teamAUser,
			expectStatus: http.StatusOK,
			expectError:  false,
			expectCount:  1,
			validateFunc: func(t *testing.T, templates []*models.TemplateInfo) {
				assert.Len(t, templates, 1)
				assert.Equal(t, "team-a-pool", templates[0].TemplateID)
			},
		},
		{
			name:         "list templates with no pools",
			setupPools:   func(t *testing.T, controller *Controller, fc ctrlclient.Client) []func() { return nil },
			queryTeamID:  "",
			user:         user,
			expectStatus: http.StatusOK,
			expectError:  false,
			expectCount:  0,
		},
		{
			name:         "user is nil returns error",
			setupPools:   func(t *testing.T, controller *Controller, fc ctrlclient.Client) []func() { return nil },
			queryTeamID:  "",
			user:         nil,
			expectStatus: http.StatusInternalServerError,
			expectError:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller, fc, teardown := Setup(t)
			defer teardown()

			var cleanups []func()
			if tt.setupPools != nil {
				cleanups = tt.setupPools(t, controller, fc)
			}
			for _, cleanup := range cleanups {
				defer cleanup()
			}

			// Create request with query parameters
			req := NewRequest(t, map[string]string{"teamID": tt.queryTeamID}, nil, nil, tt.user)

			resp, apiErr := controller.ListTemplates(req)

			if tt.expectError {
				require.NotNil(t, apiErr)
				assert.Equal(t, tt.expectStatus, apiErr.Code)
			} else {
				require.Nil(t, apiErr)
				assert.Equal(t, tt.expectStatus, resp.Code)
				assert.NotNil(t, resp.Body)
				assert.Equal(t, tt.expectCount, len(resp.Body))
				if tt.validateFunc != nil {
					tt.validateFunc(t, resp.Body)
				}
			}
		})
	}
}

func TestTemplateNamespaceIsolationWithSameName(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()

	teamAUser := &models.CreatedTeamAPIKey{
		ID:   uuid.New(),
		Key:  "team-a-key",
		Name: "team-a-user",
		Team: &models.Team{
			ID:   uuid.New(),
			Name: "team-a",
		},
	}
	teamBUser := &models.CreatedTeamAPIKey{
		ID:   uuid.New(),
		Key:  "team-b-key",
		Name: "team-b-user",
		Team: &models.Team{
			ID:   uuid.New(),
			Name: "team-b",
		},
	}
	adminUser := &models.CreatedTeamAPIKey{
		ID:   uuid.New(),
		Key:  "admin-key",
		Name: "admin",
		Team: models.AdminTeam(),
	}

	cleanupA := CreateSandboxPool(t, controller, "shared-template", 0, CreateSandboxPoolOptions{
		Namespace:  "team-a",
		CPURequest: "1000m",
		Memory:     "128Mi",
	})
	defer cleanupA()
	cleanupB := CreateSandboxPool(t, controller, "shared-template", 0, CreateSandboxPoolOptions{
		Namespace:  "team-b",
		CPURequest: "2000m",
		Memory:     "256Mi",
	})
	defer cleanupB()

	tests := []struct {
		name       string
		user       *models.CreatedTeamAPIKey
		expectCPUs []int
	}{
		{
			name:       "team-a sees only team-a template",
			user:       teamAUser,
			expectCPUs: []int{1},
		},
		{
			name:       "team-b sees only team-b template",
			user:       teamBUser,
			expectCPUs: []int{2},
		},
		{
			name:       "admin lists same-name templates across namespaces",
			user:       adminUser,
			expectCPUs: []int{1, 2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, apiErr := controller.ListTemplates(NewRequest(t, nil, nil, nil, tt.user))
			require.Nil(t, apiErr)
			assert.Equal(t, http.StatusOK, resp.Code)
			require.Len(t, resp.Body, len(tt.expectCPUs))

			gotCPUs := make([]int, 0, len(resp.Body))
			for _, tmpl := range resp.Body {
				assert.Equal(t, "shared-template", tmpl.TemplateID)
				gotCPUs = append(gotCPUs, tmpl.CPUCount)
			}
			assert.ElementsMatch(t, tt.expectCPUs, gotCPUs)
		})
	}

	getTests := []struct {
		name      string
		user      *models.CreatedTeamAPIKey
		expectCPU int
	}{
		{
			name:      "team-a gets same-name template from team-a namespace",
			user:      teamAUser,
			expectCPU: 1,
		},
		{
			name:      "team-b gets same-name template from team-b namespace",
			user:      teamBUser,
			expectCPU: 2,
		},
	}

	for _, tt := range getTests {
		t.Run(tt.name, func(t *testing.T) {
			resp, apiErr := controller.GetTemplate(NewRequest(t, nil, nil, map[string]string{
				"templateID": "shared-template",
			}, tt.user))
			require.Nil(t, apiErr)
			require.NotNil(t, resp.Body)
			require.Len(t, resp.Body.Builds, 1)
			assert.Equal(t, tt.expectCPU, resp.Body.Builds[0].CPUCount)
		})
	}

	t.Run("team-a cannot get template that only exists in team-b", func(t *testing.T) {
		cleanup := CreateSandboxPool(t, controller, "team-b-only-template", 0, CreateSandboxPoolOptions{Namespace: "team-b"})
		defer cleanup()

		_, apiErr := controller.GetTemplate(NewRequest(t, nil, nil, map[string]string{
			"templateID": "team-b-only-template",
		}, teamAUser))
		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusNotFound, apiErr.Code)
	})
}

func TestListTemplatesUsesCacheProvider(t *testing.T) {
	baseCache, _, err := cachetest.NewTestCache(t)
	require.NoError(t, err)

	spyCache := &listSandboxSetsSpy{
		Provider: baseCache,
		result: []*v1alpha1.SandboxSet{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "spy-template",
					Namespace: "team-a",
				},
			},
		},
	}

	opts := config.InitOptions(config.SandboxManagerOptions{
		SystemNamespace:    "sandbox-system",
		MemberlistBindPort: config.DefaultMemberlistBindPort,
	})
	manager, err := sandboxmanager.NewSandboxManagerBuilder(opts).
		WithRequestAdapter(adapters.DefaultAdapterFactory(TestServerPort)).
		WithCustomInfra(func() (infra.Builder, error) {
			return sandboxcr.NewInfraBuilder(opts).
				WithCache(spyCache).
				WithAPIReader(spyCache.GetAPIReader()).
				WithProxy(proxy.NewServer(opts)), nil
		}).
		Build()
	require.NoError(t, err)

	controller := &Controller{
		domain:  "example.com",
		cache:   spyCache,
		manager: manager,
	}
	user := &models.CreatedTeamAPIKey{
		ID:   uuid.New(),
		Key:  "team-a-key",
		Name: "team-a-user",
		Team: &models.Team{
			ID:   uuid.New(),
			Name: "team-a",
		},
	}

	resp, apiErr := controller.ListTemplates(NewRequest(t, nil, nil, nil, user))
	require.Nil(t, apiErr)
	require.True(t, spyCache.called)
	assert.Equal(t, "team-a", spyCache.namespace)
	assert.Equal(t, http.StatusOK, resp.Code)
	require.Len(t, resp.Body, 1)
	assert.Equal(t, "spy-template", resp.Body[0].TemplateID)
}

// TestGetTemplate tests the GetTemplate method
func TestGetTemplate(t *testing.T) {
	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
		Team: models.AdminTeam(),
	}

	tests := []struct {
		name         string
		setupPool    func(t *testing.T, controller *Controller, fc ctrlclient.Client) func()
		templateID   string
		user         *models.CreatedTeamAPIKey
		expectStatus int
		expectError  bool
		validateFunc func(t *testing.T, template *models.Template)
	}{
		{
			name: "admin gets template across namespaces",
			setupPool: func(t *testing.T, controller *Controller, fc ctrlclient.Client) func() {
				return CreateSandboxPool(t, controller, "test-tmpl-get", 2, CreateSandboxPoolOptions{Namespace: "team-a"})
			},
			templateID:   "test-tmpl-get",
			user:         user,
			expectStatus: http.StatusOK,
			expectError:  false,
			validateFunc: func(t *testing.T, template *models.Template) {
				assert.Equal(t, "test-tmpl-get", template.TemplateID)
				assert.True(t, template.Public)
				assert.NotEmpty(t, template.Aliases)
				assert.NotEmpty(t, template.Names)
				assert.NotEmpty(t, template.Builds)
				assert.Len(t, template.Builds, 1)

				build := template.Builds[0]
				assert.Equal(t, "test-tmpl-get", build.BuildID)
				assert.Equal(t, "0.1.1", build.EnvdVersion)
				assert.NotEmpty(t, build.Status)
				assert.True(t, build.CPUCount >= 0)
				assert.True(t, build.MemoryMB >= 0)
			},
		},
		{
			name:         "template not found",
			setupPool:    func(t *testing.T, controller *Controller, fc ctrlclient.Client) func() { return nil },
			templateID:   "non-existent-template",
			user:         user,
			expectStatus: http.StatusNotFound,
			expectError:  true,
		},
		{
			name:         "user is nil returns error",
			setupPool:    func(t *testing.T, controller *Controller, fc ctrlclient.Client) func() { return nil },
			templateID:   "test-tmpl",
			user:         nil,
			expectStatus: http.StatusInternalServerError,
			expectError:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller, fc, teardown := Setup(t)
			defer teardown()

			var cleanup func()
			if tt.setupPool != nil {
				cleanup = tt.setupPool(t, controller, fc)
			}
			if cleanup != nil {
				defer cleanup()
			}

			req := NewRequest(t, nil, nil, map[string]string{
				"templateID": tt.templateID,
			}, tt.user)

			resp, apiErr := controller.GetTemplate(req)

			if tt.expectError {
				require.NotNil(t, apiErr)
				assert.Equal(t, tt.expectStatus, apiErr.Code)
			} else {
				require.Nil(t, apiErr)
				assert.Equal(t, tt.expectStatus, resp.Code)
				assert.NotNil(t, resp.Body)
				if tt.validateFunc != nil {
					tt.validateFunc(t, resp.Body)
				}
			}
		})
	}
}

// TestBuildResource tests the BuildResource function
func TestBuildResource(t *testing.T) {
	cpuQuantity, _ := resource.ParseQuantity("2000m")
	memoryQuantity, _ := resource.ParseQuantity("2048Mi")
	storageQuantity, _ := resource.ParseQuantity("10Gi")

	tests := []struct {
		name           string
		sandboxSet     *v1alpha1.SandboxSet
		expectCPU      int
		expectMemoryMB int
		expectDiskMB   int
	}{
		{
			name: "single container with resources",
			sandboxSet: &v1alpha1.SandboxSet{
				Spec: v1alpha1.SandboxSetSpec{
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name: "main",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												corev1.ResourceCPU:    cpuQuantity,
												corev1.ResourceMemory: memoryQuantity,
											},
										},
									},
								},
							},
						},
					},
				},
			},
			expectCPU:      2,
			expectMemoryMB: 2048,
			expectDiskMB:   0,
		},
		{
			name: "multiple containers with resources",
			sandboxSet: &v1alpha1.SandboxSet{
				Spec: v1alpha1.SandboxSetSpec{
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name: "main",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												corev1.ResourceCPU:    cpuQuantity,
												corev1.ResourceMemory: memoryQuantity,
											},
										},
									},
									{
										Name: "sidecar",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												corev1.ResourceCPU:    cpuQuantity,
												corev1.ResourceMemory: memoryQuantity,
											},
										},
									},
								},
							},
						},
					},
				},
			},
			expectCPU:      4,
			expectMemoryMB: 4096,
			expectDiskMB:   0,
		},
		{
			name: "sandbox set with volume claim templates",
			sandboxSet: &v1alpha1.SandboxSet{
				Spec: v1alpha1.SandboxSetSpec{
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name: "main",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												corev1.ResourceCPU:    cpuQuantity,
												corev1.ResourceMemory: memoryQuantity,
											},
										},
									},
								},
							},
						},
						VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
							{
								ObjectMeta: metav1.ObjectMeta{
									Name: "data",
								},
								Spec: corev1.PersistentVolumeClaimSpec{
									AccessModes: []corev1.PersistentVolumeAccessMode{
										corev1.ReadWriteOnce,
									},
									Resources: corev1.VolumeResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceStorage: storageQuantity,
										},
									},
								},
							},
						},
					},
				},
			},
			expectCPU:      2,
			expectMemoryMB: 2048,
			expectDiskMB:   10240, // 10Gi = 10240Mi
		},
		{
			name: "no resources specified",
			sandboxSet: &v1alpha1.SandboxSet{
				Spec: v1alpha1.SandboxSetSpec{
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name: "main",
									},
								},
							},
						},
					},
				},
			},
			expectCPU:      0,
			expectMemoryMB: 0,
			expectDiskMB:   0,
		},
		{
			name: "nil template",
			sandboxSet: &v1alpha1.SandboxSet{
				Spec: v1alpha1.SandboxSetSpec{
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: nil,
					},
				},
			},
			expectCPU:      0,
			expectMemoryMB: 0,
			expectDiskMB:   0,
		},
		{
			name: "multiple volume claim templates",
			sandboxSet: &v1alpha1.SandboxSet{
				Spec: v1alpha1.SandboxSetSpec{
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name: "main",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												corev1.ResourceCPU:    cpuQuantity,
												corev1.ResourceMemory: memoryQuantity,
											},
										},
									},
								},
							},
						},
						VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
							{
								ObjectMeta: metav1.ObjectMeta{
									Name: "data",
								},
								Spec: corev1.PersistentVolumeClaimSpec{
									AccessModes: []corev1.PersistentVolumeAccessMode{
										corev1.ReadWriteOnce,
									},
									Resources: corev1.VolumeResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceStorage: storageQuantity,
										},
									},
								},
							},
							{
								ObjectMeta: metav1.ObjectMeta{
									Name: "logs",
								},
								Spec: corev1.PersistentVolumeClaimSpec{
									AccessModes: []corev1.PersistentVolumeAccessMode{
										corev1.ReadWriteOnce,
									},
									Resources: corev1.VolumeResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceStorage: storageQuantity,
										},
									},
								},
							},
						},
					},
				},
			},
			expectCPU:      2,
			expectMemoryMB: 2048,
			expectDiskMB:   20480, // 2 * 10Gi = 20480Mi
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cpu, memory, disk := BuildResource(tt.sandboxSet)
			assert.Equal(t, tt.expectCPU, cpu)
			assert.Equal(t, tt.expectMemoryMB, memory)
			assert.Equal(t, tt.expectDiskMB, disk)
		})
	}
}
