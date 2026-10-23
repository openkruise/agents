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
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	infracache "github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestListSandboxes(t *testing.T) {
	templateName := "test-template"
	controller, fc, teardown := Setup(t)
	defer teardown()
	user := adminTestUser()
	tests := []struct {
		name           string
		createRequests []models.NewSandboxRequest // use metadata key "state" to control sandbox state, default running
		queryParams    map[string]string
		expectListed   func(sandbox *models.Sandbox) bool
		expectError    *web.ApiError
	}{
		{
			name: "list by metadata",
			createRequests: []models.NewSandboxRequest{
				{
					TemplateID: templateName,
					Metadata: map[string]string{
						"testKey": "value1",
					},
				},
				{
					TemplateID: templateName,
					Metadata: map[string]string{
						"testKey": "value2",
					},
				},
			},
			queryParams: map[string]string{
				"metadata": "testKey=value1",
			},
			expectListed: func(sbx *models.Sandbox) bool {
				return sbx.Metadata["testKey"] == "value1"
			},
		},
		{
			name: "list by single state",
			createRequests: []models.NewSandboxRequest{
				{
					TemplateID: templateName,
					Metadata: map[string]string{
						"state": "paused",
					},
				},
				{
					TemplateID: templateName,
					Metadata: map[string]string{
						"state": "running",
					},
				},
			},
			queryParams: map[string]string{
				"state": "running",
			},
			expectListed: func(sbx *models.Sandbox) bool {
				return sbx.Metadata["state"] == "running"
			},
		},
		{
			name: "list by multi state",
			createRequests: []models.NewSandboxRequest{
				{
					TemplateID: templateName,
					Metadata: map[string]string{
						"state": "paused",
					},
				},
				{
					TemplateID: templateName,
					Metadata: map[string]string{
						"state": "running",
					},
				},
			},
			queryParams: map[string]string{
				"state": "running,paused",
			},
			expectListed: func(sbx *models.Sandbox) bool {
				return sbx.Metadata["state"] == "running" || sbx.Metadata["state"] == "paused"
			},
		},
		{
			name: "list by illegal state",
			createRequests: []models.NewSandboxRequest{
				{
					TemplateID: templateName,
					Metadata: map[string]string{
						"state": "paused",
					},
				},
				{
					TemplateID: templateName,
					Metadata: map[string]string{
						"state": "running",
					},
				},
			},
			queryParams: map[string]string{
				"state": "foo",
			},
			expectError: &web.ApiError{
				Code: http.StatusBadRequest,
				Message: fmt.Sprintf("Only '%s' and '%s' state are supported, not: '%s'",
					models.SandboxStateRunning, models.SandboxStatePaused, "foo"),
			},
		},
		{
			name:           "limit exceeds max limit returns 400 error",
			createRequests: nil,
			queryParams: map[string]string{
				"limit": "101",
			},
			expectError: &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: fmt.Sprintf("Invalid limit: 101, must be between %d and %d", models.MinListLimit, models.MaxListLimit),
			},
		},
		{
			name:           "limit is zero returns 400 error",
			createRequests: nil,
			queryParams: map[string]string{
				"limit": "0",
			},
			expectError: &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: fmt.Sprintf("Invalid limit: 0, must be between %d and %d", models.MinListLimit, models.MaxListLimit),
			},
		},
		{
			name:           "limit is negative returns 400 error",
			createRequests: nil,
			queryParams: map[string]string{
				"limit": "-1",
			},
			expectError: &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: fmt.Sprintf("Invalid limit: -1, must be between %d and %d", models.MinListLimit, models.MaxListLimit),
			},
		},
		{
			name:           "invalid order returns 400 error",
			createRequests: nil,
			queryParams: map[string]string{
				"order": "newest",
			},
			expectError: &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: "Invalid order: must be a single value of asc or desc",
			},
		},
		{
			name:           "empty template returns 400 error",
			createRequests: nil,
			queryParams: map[string]string{
				"template": "",
			},
			expectError: &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: "Invalid template: must be a single non-empty value",
			},
		},
		{
			name:           "invalid startedAfter returns 400 error",
			createRequests: nil,
			queryParams: map[string]string{
				"startedAfter": "yesterday",
			},
			expectError: &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: "Invalid startedAfter: must be a single RFC3339 timestamp",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Error-only cases fail at request parsing and never touch the pool.
			if len(tt.createRequests) > 0 {
				cleanup := CreateSandboxPool(t, controller, templateName, 10)
				defer cleanup()
			}

			var expectedListed []models.Sandbox
			var createdRequests []*http.Request
			var expectStates []string
			for _, request := range tt.createRequests {
				request.Extensions.SkipInitRuntime = true
				request.Metadata[models.ExtensionKeySkipInitRuntime] = agentsv1alpha1.True
				resp, apiError := controller.CreateSandbox(NewRequest(t, nil, request, nil, user))
				assert.Nil(t, apiError)
				sandbox := resp.Body
				expectState := "running"
				if sandbox.Metadata["state"] != "" {
					expectState = sandbox.Metadata["state"]
				}
				req := NewRequest(t, nil, nil, map[string]string{
					"sandboxID": sandbox.SandboxID,
				}, user)
				if expectState == "paused" {
					EnableWaitSim(t, controller, sandbox.SandboxID)
					pauseSandboxHelper(t, controller, fc, sandbox.SandboxID, false, false, user)
				}
				createdRequests = append(createdRequests, req)
				expectStates = append(expectStates, expectState)
			}

			// Wait for sandboxes to be ready before attempting pause operations
			if len(createdRequests) > 0 {
				time.Sleep(200 * time.Millisecond)
			}

			for i, request := range createdRequests {
				// Wait for sandbox to be in expected state
				var sandbox *models.Sandbox
				assert.Eventually(t, func() bool {
					describe, describeErr := controller.DescribeSandbox(request)
					if describeErr != nil {
						return false
					}
					sandbox = describe.Body
					return sandbox.State == expectStates[i]
				}, 2*time.Second, 50*time.Millisecond, "sandbox %s should reach state %s", request.PathValue("sandboxID"), expectStates[i])

				assert.Equal(t, expectStates[i], sandbox.State)
				sandbox.EnvdAccessToken = "" // token is not listed
				if tt.expectListed != nil && tt.expectListed(sandbox) {
					expectedListed = append(expectedListed, *sandbox)
				}
			}

			resp, apiError := controller.ListSandboxes(NewRequest(t, tt.queryParams, nil, nil, user))
			if tt.expectError != nil {
				assert.NotNil(t, apiError)
				if apiError != nil {
					assert.Equal(t, tt.expectError.Code, apiError.Code)
					assert.Equal(t, tt.expectError.Message, apiError.Message)
				}
			} else {
				assert.Nil(t, apiError)
				list := resp.Body
				var gotListed []models.Sandbox
				for _, sandbox := range list {
					gotListed = append(gotListed, *sandbox)
				}
				assert.ElementsMatch(t, expectedListed, gotListed)
			}
		})
	}
}

func TestParseListSandboxesRequestRejectsClaimMethodMetadata(t *testing.T) {
	claimMethod := agentsv1alpha1.LabelSandboxClaimMethod
	tests := []struct {
		name  string
		query string
	}{
		{
			name: "metadata parameter",
			query: url.Values{
				"metadata": []string{claimMethod + "=" + agentsv1alpha1.ClaimMethodColdStart},
			}.Encode(),
		},
		{
			name: "direct metadata key",
			query: url.Values{
				claimMethod: []string{agentsv1alpha1.ClaimMethodColdStart},
			}.Encode(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v2/sandboxes?"+tt.query, nil)
			_, apiErr := parseListSandboxesRequest(req)
			require.NotNil(t, apiErr)
			assert.Equal(t, http.StatusBadRequest, apiErr.Code)
			assert.Contains(t, apiErr.Message, "Forbidden metadata key")
		})
	}
}

func TestListSandboxesSkipsReservedFailedSandbox(t *testing.T) {
	controller, fc, teardown := Setup(t)
	defer teardown()

	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
		Team: models.AdminTeam(),
	}
	visible := CreateClaimedSandboxCR(t, controller, Namespace, "visible-sandbox", "test-template", user.ID.String(), nil)
	hidden := CreateClaimedSandboxCR(t, controller, Namespace, "reserved-failed-sandbox", "test-template", user.ID.String(), nil)
	hidden.Labels[agentsv1alpha1.LabelSandboxReservedFailed] = agentsv1alpha1.True
	require.NoError(t, fc.Update(t.Context(), hidden))

	resp, apiErr := controller.ListSandboxes(NewRequest(t, nil, nil, nil, user))

	require.Nil(t, apiErr)
	require.Len(t, resp.Body, 1)
	assert.Equal(t, fmt.Sprintf("%s--%s", visible.Namespace, visible.Name), resp.Body[0].SandboxID)
}

func TestListSnapshotsNamespaceIsolationWithSameCheckpointID(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()

	ownerID := uuid.New()
	teamAUser := &models.CreatedTeamAPIKey{
		ID:   ownerID,
		Key:  "team-a-key",
		Name: "team-a-user",
		Team: &models.Team{
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

	tests := []struct {
		name        string
		user        *models.CreatedTeamAPIKey
		query       map[string]string
		expectCount int
	}{
		{
			name:        "team-a sees only team-a snapshot with colliding checkpoint id",
			user:        teamAUser,
			expectCount: 1,
		},
		{
			name:        "team-a sandboxID filter cannot see team-b snapshot",
			user:        teamAUser,
			query:       map[string]string{"sandboxID": "team-b-sandbox"},
			expectCount: 0,
		},
		{
			name:        "admin sees same checkpoint id across namespaces",
			user:        adminUser,
			expectCount: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, apiErr := controller.ListSnapshots(NewRequest(t, tt.query, nil, nil, tt.user))
			require.Nil(t, apiErr)
			require.Len(t, resp.Body, tt.expectCount)
			for _, snapshot := range resp.Body {
				assert.Equal(t, "shared-checkpoint-id", snapshot.SnapshotID)
			}
		})
	}
}

func TestListSandboxes_Pagination(t *testing.T) {
	templateName := "pagination-template"
	controller, fc, teardown := Setup(t)
	defer teardown()
	user := adminTestUser()

	// Create SandboxSet first (similar to CreateSandboxPool but without creating sandboxes)
	tmpl := agentsv1alpha1.EmbeddedSandboxTemplate{
		Template: &corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "main",
						Image: "test-image",
					},
				},
			},
		},
	}
	sbs := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      templateName,
			Namespace: Namespace,
			UID:       types.UID(uuid.NewString()),
		},
		Spec: agentsv1alpha1.SandboxSetSpec{
			EmbeddedSandboxTemplate: tmpl,
		},
	}
	err := fc.Create(t.Context(), sbs)
	assert.NoError(t, err)
	defer func() {
		_ = fc.Delete(context.Background(), sbs)
	}()

	// Create sandboxes with different claim times
	claimTimes := []string{
		"2024-01-01T00:00:01Z",
		"2024-01-01T00:00:01.1Z",
		"2024-01-01T00:00:01.11Z",
		"2024-01-01T00:00:02Z",
		"2024-01-01T00:00:03Z",
	}
	var createdSandboxIDs []string

	now := metav1.Now()
	for i, claimTime := range claimTimes {
		sbxName := fmt.Sprintf("%s-pagination-%d", templateName, i)
		sbx := &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sbxName,
				Namespace: Namespace,
				Labels: map[string]string{
					agentsv1alpha1.LabelSandboxTemplate:  templateName,
					agentsv1alpha1.LabelSandboxIsClaimed: "true",
				},
				Annotations: map[string]string{
					agentsv1alpha1.AnnotationClaimTime: claimTime,
					agentsv1alpha1.AnnotationOwner:     user.ID.String(),
				},
				ResourceVersion:   "",
				UID:               types.UID(uuid.NewString()),
				CreationTimestamp: now,
			},
			Spec: agentsv1alpha1.SandboxSpec{
				EmbeddedSandboxTemplate: tmpl,
			},
			Status: agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxRunning,
				Conditions: []metav1.Condition{
					{
						Type:   string(agentsv1alpha1.SandboxConditionReady),
						Status: metav1.ConditionTrue,
					},
					{
						Type:   string(agentsv1alpha1.SandboxConditionPaused),
						Status: metav1.ConditionFalse,
					},
					{
						Type:   string(agentsv1alpha1.SandboxConditionResumed),
						Status: metav1.ConditionTrue,
					},
				},
				PodInfo: agentsv1alpha1.PodInfo{
					PodIP: "1.2.3.4",
				},
			},
		}
		CreateSandboxWithStatus(t, fc, sbx)
		createdSandboxIDs = append(createdSandboxIDs, fmt.Sprintf("%s--%s", Namespace, sbxName))
	}

	// Wait for sandboxes to be available in cache
	assert.Eventually(t, func() bool {
		resp, apiError := controller.ListSandboxes(NewRequest(t, nil, nil, nil, user))
		if apiError != nil {
			return false
		}
		return len(resp.Body) >= len(claimTimes)
	}, 2*time.Second, 50*time.Millisecond, "sandboxes should be available in cache")

	defer func() {
		for i := range claimTimes {
			sbxName := fmt.Sprintf("%s-pagination-%d", templateName, i)
			sbx := &agentsv1alpha1.Sandbox{}
			sbx.Name = sbxName
			sbx.Namespace = Namespace
			_ = fc.Delete(context.Background(), sbx)
		}
	}()

	// pageExpectation defines the expected result for each page in pagination
	type pageExpectation struct {
		count         int   // expected count, -1 means use minCount
		minCount      int   // minimum count when count is -1
		hasNextToken  bool  // whether x-next-token should be present
		startedAtIdxs []int // indices into claimTimes to verify StartedAt
	}

	tests := []struct {
		name  string
		query map[string]string
		pages []pageExpectation // sequence of pages to verify (supports pagination chain)
	}{
		{
			name:  "next page with nextToken",
			query: map[string]string{"limit": "2"},
			pages: []pageExpectation{
				{count: 2, hasNextToken: true, startedAtIdxs: []int{4, 3}},
				{count: 2, hasNextToken: true, startedAtIdxs: []int{2, 1}},
				{count: 1, hasNextToken: false, startedAtIdxs: []int{0}},
			},
		},
		{
			name:  "explicit asc keeps oldest first",
			query: map[string]string{"limit": "2", "order": "asc"},
			pages: []pageExpectation{
				{count: 2, hasNextToken: true, startedAtIdxs: []int{0, 1}},
				{count: 2, hasNextToken: true, startedAtIdxs: []int{2, 3}},
				{count: 1, hasNextToken: false, startedAtIdxs: []int{4}},
			},
		},
		{
			name:  "start with raw key nextToken",
			query: map[string]string{"limit": "2", "order": "asc", "nextToken": "2024-01-01T00:00:01.100000000Z"},
			pages: []pageExpectation{
				{count: 2, hasNextToken: true, startedAtIdxs: []int{2, 3}},
			},
		},
		{
			name: "start with existing stable cursor",
			query: map[string]string{"limit": "2", "order": "asc", "nextToken": base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(
				`{"key":"2024-01-01T00:00:01.100000000Z","id":%q}`, createdSandboxIDs[1],
			)))},
			pages: []pageExpectation{
				{count: 2, hasNextToken: true, startedAtIdxs: []int{2, 3}},
			},
		},
		{
			name:  "unknown template returns an empty list",
			query: map[string]string{"template": "missing-template"},
			pages: []pageExpectation{
				{count: 0, hasNextToken: false},
			},
		},
		{
			name:  "startedAfter includes the boundary and orders newest first",
			query: map[string]string{"startedAfter": "2024-01-01T00:00:01.11Z"},
			pages: []pageExpectation{
				{count: 3, hasNextToken: false, startedAtIdxs: []int{4, 3, 2}},
			},
		},
		{
			name:  "startedAfter timezone offset matches the same instant",
			query: map[string]string{"startedAfter": "2023-12-31T16:00:01.11-08:00"},
			pages: []pageExpectation{
				{count: 3, hasNextToken: false, startedAtIdxs: []int{4, 3, 2}},
			},
		},
		{
			name:  "startedAfter fractional second excludes the earlier instant",
			query: map[string]string{"startedAfter": "2024-01-01T00:00:01.5Z"},
			pages: []pageExpectation{
				{count: 2, hasNextToken: false, startedAtIdxs: []int{4, 3}},
			},
		},
		{
			name:  "no limit parameter returns all sandboxes",
			query: nil,
			pages: []pageExpectation{
				{count: -1, minCount: len(claimTimes), hasNextToken: false},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var nextToken string
			for pageIdx, page := range tt.pages {
				// Build query, adding nextToken if available from previous page
				query := make(map[string]string)
				for k, v := range tt.query {
					query[k] = v
				}
				if nextToken != "" {
					query["nextToken"] = nextToken
				}

				resp, apiError := controller.ListSandboxes(NewRequest(t, query, nil, nil, user))
				assert.Nil(t, apiError, "page %d should not return error", pageIdx)

				// Verify count
				if page.count >= 0 {
					require.Len(t, resp.Body, page.count, "page %d should have %d sandboxes", pageIdx, page.count)
				} else {
					assert.GreaterOrEqual(t, len(resp.Body), page.minCount, "page %d should have at least %d sandboxes", pageIdx, page.minCount)
				}

				// Verify x-next-token presence
				if page.hasNextToken {
					assert.NotEmpty(t, resp.Headers["x-next-token"], "page %d should have x-next-token", pageIdx)
					nextToken = resp.Headers["x-next-token"]
				} else {
					assert.Empty(t, resp.Headers, "page %d should not have x-next-token", pageIdx)
					nextToken = ""
				}

				// Verify StartedAt values if specified
				if len(page.startedAtIdxs) > 0 {
					var gotStartedAts []string
					for _, sbx := range resp.Body {
						gotStartedAts = append(gotStartedAts, sbx.StartedAt)
					}
					for i, idx := range page.startedAtIdxs {
						claimTime, err := time.Parse(time.RFC3339Nano, claimTimes[idx])
						require.NoError(t, err)
						assert.Equal(t, claimTime.Format(time.RFC3339), gotStartedAts[i])
						assert.Equal(t, createdSandboxIDs[idx], resp.Body[i].SandboxID, "page %d: sandbox %d should match time order", pageIdx, i)
					}
				}
			}
		})
	}
}

func TestListSandboxes_PaginationDuplicateClaimTime(t *testing.T) {
	tests := []struct {
		name         string
		sandboxCount int
		limit        int
		claimTime    string
		useShortIDs  bool
	}{
		{
			name:         "same claim time across page boundary",
			sandboxCount: 5,
			limit:        2,
			claimTime:    "2024-01-01T00:00:01Z",
		},
		{
			name:         "opaque short IDs across page boundary",
			sandboxCount: 5,
			limit:        2,
			claimTime:    "2024-01-01T00:00:01Z",
			useShortIDs:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			templateName := "pagination-duplicate-template"
			controller, fc, teardown := Setup(t)
			defer teardown()
			user := adminTestUser()

			tmpl := agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "main",
								Image: "test-image",
							},
						},
					},
				},
			}
			sbs := &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      templateName,
					Namespace: Namespace,
					UID:       types.UID(uuid.NewString()),
				},
				Spec: agentsv1alpha1.SandboxSetSpec{
					EmbeddedSandboxTemplate: tmpl,
				},
			}
			require.NoError(t, fc.Create(t.Context(), sbs))
			defer func() {
				_ = fc.Delete(context.Background(), sbs)
			}()

			expectedSandboxIDs := make([]string, 0, tt.sandboxCount)
			now := metav1.Now()
			for i := 0; i < tt.sandboxCount; i++ {
				sbxName := fmt.Sprintf("%s-%d", templateName, i)
				sbx := &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      sbxName,
						Namespace: Namespace,
						Labels: map[string]string{
							agentsv1alpha1.LabelSandboxTemplate:  templateName,
							agentsv1alpha1.LabelSandboxIsClaimed: "true",
						},
						Annotations: map[string]string{
							agentsv1alpha1.AnnotationClaimTime: tt.claimTime,
							agentsv1alpha1.AnnotationOwner:     user.ID.String(),
						},
						UID:               types.UID(uuid.NewString()),
						CreationTimestamp: now,
					},
					Spec: agentsv1alpha1.SandboxSpec{
						EmbeddedSandboxTemplate: tmpl,
					},
					Status: agentsv1alpha1.SandboxStatus{
						Phase: agentsv1alpha1.SandboxRunning,
						Conditions: []metav1.Condition{
							{
								Type:   string(agentsv1alpha1.SandboxConditionReady),
								Status: metav1.ConditionTrue,
							},
							{
								Type:   string(agentsv1alpha1.SandboxConditionPaused),
								Status: metav1.ConditionFalse,
							},
							{
								Type:   string(agentsv1alpha1.SandboxConditionResumed),
								Status: metav1.ConditionTrue,
							},
						},
						PodInfo: agentsv1alpha1.PodInfo{
							PodIP: "1.2.3.4",
						},
					},
				}
				expectedSandboxID := fmt.Sprintf("%s--%s", Namespace, sbxName)
				if tt.useShortIDs {
					expectedSandboxID = fmt.Sprintf("opaque-short-%d", i)
					sbx.Labels[agentsv1alpha1.LabelSandboxID] = expectedSandboxID
				}
				CreateSandboxWithStatus(t, fc, sbx)
				expectedSandboxIDs = append(expectedSandboxIDs, expectedSandboxID)
			}

			assert.Eventually(t, func() bool {
				resp, apiError := controller.ListSandboxes(NewRequest(t, nil, nil, nil, user))
				return apiError == nil && len(resp.Body) >= len(expectedSandboxIDs)
			}, 2*time.Second, 50*time.Millisecond, "sandboxes should be available in cache")

			defer func() {
				for i := 0; i < tt.sandboxCount; i++ {
					sbxName := fmt.Sprintf("%s-%d", templateName, i)
					sbx := &agentsv1alpha1.Sandbox{}
					sbx.Name = sbxName
					sbx.Namespace = Namespace
					_ = fc.Delete(context.Background(), sbx)
				}
			}()

			var nextToken string
			var gotSandboxIDs []string
			for page := 0; page < 10; page++ {
				query := map[string]string{"limit": fmt.Sprintf("%d", tt.limit)}
				if nextToken != "" {
					query["nextToken"] = nextToken
				}

				resp, apiError := controller.ListSandboxes(NewRequest(t, query, nil, nil, user))
				require.Nil(t, apiError, "page %d should not return error", page)
				require.NotEmpty(t, resp.Body, "page %d should return at least one sandbox", page)

				for _, sbx := range resp.Body {
					gotSandboxIDs = append(gotSandboxIDs, sbx.SandboxID)
					assert.Equal(t, tt.claimTime, sbx.StartedAt, "sandbox should keep duplicate claim time")
				}

				nextToken = resp.Headers["x-next-token"]
				if nextToken == "" {
					break
				}
			}

			assert.Empty(t, nextToken, "pagination should finish")
			assert.Len(t, gotSandboxIDs, len(expectedSandboxIDs))
			assert.ElementsMatch(t, expectedSandboxIDs, gotSandboxIDs)
		})
	}
}

func TestListSnapshots(t *testing.T) {
	controller, fc, teardown := Setup(t)
	defer teardown()

	adminUser := adminTestUser()

	otherUserID := uuid.New()
	otherUser := &models.CreatedTeamAPIKey{
		ID:   otherUserID,
		Key:  "other-key-123",
		Name: "other",
	}

	createCheckpointInNamespace := func(namespace, name, owner, sandboxID, checkpointID, creationTime string) *agentsv1alpha1.Checkpoint {
		parsedTime, _ := time.Parse(time.RFC3339, creationTime)
		cp := &agentsv1alpha1.Checkpoint{
			ObjectMeta: metav1.ObjectMeta{
				Name:              name,
				Namespace:         namespace,
				UID:               types.UID(uuid.NewString()),
				CreationTimestamp: metav1.NewTime(parsedTime),
				Annotations: map[string]string{
					agentsv1alpha1.AnnotationOwner:     owner,
					agentsv1alpha1.AnnotationSandboxID: sandboxID,
				},
			},
			Spec: agentsv1alpha1.CheckpointSpec{},
			Status: agentsv1alpha1.CheckpointStatus{
				Phase:        agentsv1alpha1.CheckpointSucceeded,
				CheckpointId: checkpointID,
			},
		}
		err := fc.Create(t.Context(), cp)
		assert.NoError(t, err)
		err = fc.Status().Update(t.Context(), cp)
		assert.NoError(t, err)
		return cp
	}

	// Helper to create a checkpoint with given parameters
	createCheckpoint := func(name, owner, sandboxID, checkpointID, creationTime string) *agentsv1alpha1.Checkpoint {
		return createCheckpointInNamespace(Namespace, name, owner, sandboxID, checkpointID, creationTime)
	}

	// deleteCheckpoints returns the per-case cleanup for checkpoints created in Namespace.
	deleteCheckpoints := func(names ...string) func() {
		return func() {
			for _, name := range names {
				cp := &agentsv1alpha1.Checkpoint{}
				cp.Name = name
				cp.Namespace = Namespace
				_ = fc.Delete(context.Background(), cp)
			}
		}
	}

	// waitAliasVisible blocks until the informer cache observes count Succeeded
	// checkpoints carrying the alias, so name-filtered queries see the labels
	// that were added by Update after Create.
	waitAliasVisible := func(alias string, count int) {
		t.Helper()
		require.Eventually(t, func() bool {
			cps, err := controller.cache.ListCheckpoints(t.Context(), infracache.ListCheckpointsOptions{
				Namespace:      Namespace,
				MatchingLabels: map[string]string{agentsv1alpha1.CheckpointLabelAlias: alias},
			})
			if err != nil || len(cps) != count {
				return false
			}
			for _, cp := range cps {
				if cp.Status.Phase != agentsv1alpha1.CheckpointSucceeded {
					return false
				}
			}
			return true
		}, 2*time.Second, 50*time.Millisecond)
	}

	// pageExpectation defines the expected result for each page in pagination
	type pageExpectation struct {
		count        int  // expected count, -1 means use minCount
		minCount     int  // minimum count when count is -1
		hasNextToken bool // whether x-next-token should be present
	}

	tests := []struct {
		name          string
		setup         func() func() // returns cleanup function
		user          *models.CreatedTeamAPIKey
		query         map[string]string
		pages         []pageExpectation
		expectedIDs   []string      // expected checkpoint IDs in order (for single page tests)
		expectedNames []string      // expected snapshot names on every returned item
		expectedTotal int           // expected total count across all pages (for multi-page tests)
		expectError   *web.ApiError // expected error response
	}{
		{
			name: "first page with limit",
			setup: func() func() {
				for i := 0; i < 5; i++ {
					createCheckpoint(
						fmt.Sprintf("cp-pagination-%d", i),
						adminUser.ID.String(),
						fmt.Sprintf("sandbox-%d", i),
						fmt.Sprintf("checkpoint-id-%d", i),
						fmt.Sprintf("2024-01-01T00:00:0%dZ", i+1),
					)
				}
				return func() {
					for i := 0; i < 5; i++ {
						cp := &agentsv1alpha1.Checkpoint{}
						cp.Name = fmt.Sprintf("cp-pagination-%d", i)
						cp.Namespace = Namespace
						_ = fc.Delete(context.Background(), cp)
					}
				}
			},
			user:  adminUser,
			query: map[string]string{"limit": "2"},
			pages: []pageExpectation{
				{count: 2, hasNextToken: true},
			},
			expectedIDs: []string{"checkpoint-id-0", "checkpoint-id-1"},
		},
		{
			name: "pagination chain - multiple pages",
			setup: func() func() {
				for i := 0; i < 5; i++ {
					createCheckpoint(
						fmt.Sprintf("cp-chain-%d", i),
						adminUser.ID.String(),
						fmt.Sprintf("sandbox-chain-%d", i),
						fmt.Sprintf("chain-checkpoint-id-%d", i),
						fmt.Sprintf("2024-02-01T00:00:0%dZ", i+1),
					)
				}
				return func() {
					for i := 0; i < 5; i++ {
						cp := &agentsv1alpha1.Checkpoint{}
						cp.Name = fmt.Sprintf("cp-chain-%d", i)
						cp.Namespace = Namespace
						_ = fc.Delete(context.Background(), cp)
					}
				}
			},
			user:  adminUser,
			query: map[string]string{"limit": "2"},
			pages: []pageExpectation{
				{count: 2, hasNextToken: true},
				{count: 2, hasNextToken: true},
				{count: 1, hasNextToken: false},
			},
			expectedTotal: 5,
		},
		{
			name: "pagination chain with duplicate creation timestamps",
			setup: func() func() {
				for i := 0; i < 5; i++ {
					createCheckpoint(
						fmt.Sprintf("cp-duplicate-time-%d", i),
						adminUser.ID.String(),
						fmt.Sprintf("sandbox-duplicate-time-%d", i),
						fmt.Sprintf("duplicate-time-checkpoint-id-%d", i),
						"2024-02-02T00:00:01Z",
					)
				}
				return func() {
					for i := 0; i < 5; i++ {
						cp := &agentsv1alpha1.Checkpoint{}
						cp.Name = fmt.Sprintf("cp-duplicate-time-%d", i)
						cp.Namespace = Namespace
						_ = fc.Delete(context.Background(), cp)
					}
				}
			},
			user:  adminUser,
			query: map[string]string{"limit": "2"},
			pages: []pageExpectation{
				{count: 2, hasNextToken: true},
				{count: 2, hasNextToken: true},
				{count: 1, hasNextToken: false},
			},
			expectedTotal: 5,
		},
		{
			name: "pagination chain with duplicate creation timestamps and checkpoint ids",
			setup: func() func() {
				namespaces := []string{"team-a", "team-b", "team-c", "team-d", "team-e"}
				for i, namespace := range namespaces {
					createCheckpointInNamespace(
						namespace,
						fmt.Sprintf("cp-duplicate-id-%d", i),
						adminUser.ID.String(),
						fmt.Sprintf("sandbox-duplicate-id-%d", i),
						"shared-checkpoint-id",
						"2024-02-03T00:00:01Z",
					)
				}
				return func() {
					for i, namespace := range namespaces {
						cp := &agentsv1alpha1.Checkpoint{}
						cp.Name = fmt.Sprintf("cp-duplicate-id-%d", i)
						cp.Namespace = namespace
						_ = fc.Delete(context.Background(), cp)
					}
				}
			},
			user:  adminUser,
			query: map[string]string{"limit": "2"},
			pages: []pageExpectation{
				{count: 2, hasNextToken: true},
				{count: 2, hasNextToken: true},
				{count: 1, hasNextToken: false},
			},
			expectedTotal: 5,
		},
		{
			name: "no limit returns all",
			setup: func() func() {
				for i := 0; i < 3; i++ {
					createCheckpoint(
						fmt.Sprintf("cp-all-%d", i),
						adminUser.ID.String(),
						fmt.Sprintf("sandbox-all-%d", i),
						fmt.Sprintf("all-checkpoint-id-%d", i),
						fmt.Sprintf("2024-03-01T00:00:0%dZ", i+1),
					)
				}
				return func() {
					for i := 0; i < 3; i++ {
						cp := &agentsv1alpha1.Checkpoint{}
						cp.Name = fmt.Sprintf("cp-all-%d", i)
						cp.Namespace = Namespace
						_ = fc.Delete(context.Background(), cp)
					}
				}
			},
			user:  adminUser,
			query: nil,
			pages: []pageExpectation{
				{count: -1, minCount: 3, hasNextToken: false},
			},
		},
		{
			name: "filter by sandboxID",
			setup: func() func() {
				// Create checkpoints with different sandbox IDs
				createCheckpoint("cp-filter-1", adminUser.ID.String(), "target-sandbox", "filter-cp-1", "2024-04-01T00:00:01Z")
				createCheckpoint("cp-filter-2", adminUser.ID.String(), "other-sandbox", "filter-cp-2", "2024-04-01T00:00:02Z")
				createCheckpoint("cp-filter-3", adminUser.ID.String(), "target-sandbox", "filter-cp-3", "2024-04-01T00:00:03Z")
				return deleteCheckpoints("cp-filter-1", "cp-filter-2", "cp-filter-3")
			},
			user:  adminUser,
			query: map[string]string{"sandboxID": "target-sandbox"},
			pages: []pageExpectation{
				{count: 2, hasNextToken: false},
			},
			expectedIDs: []string{"filter-cp-1", "filter-cp-3"},
		},
		{
			name: "opaque historical sandbox ID filter paginates duplicate timestamps",
			setup: func() func() {
				createCheckpoint("cp-opaque-1", adminUser.ID.String(), "opaque-historical-id", "opaque-cp-1", "2024-04-02T00:00:01Z")
				createCheckpoint("cp-opaque-2", adminUser.ID.String(), "opaque-historical-id", "opaque-cp-2", "2024-04-02T00:00:01Z")
				createCheckpoint("cp-opaque-other", adminUser.ID.String(), "different-historical-id", "opaque-cp-other", "2024-04-02T00:00:01Z")
				return deleteCheckpoints("cp-opaque-1", "cp-opaque-2", "cp-opaque-other")
			},
			user:  adminUser,
			query: map[string]string{"sandboxID": "opaque-historical-id", "limit": "1"},
			pages: []pageExpectation{
				{count: 1, hasNextToken: true},
				{count: 1, hasNextToken: false},
			},
			expectedTotal: 2,
		},
		{
			name: "historical Checkpoint annotation survives later short ID and ignores Checkpoint label",
			setup: func() func() {
				legacyID := Namespace + "--history-sandbox"
				oldCheckpoint := createCheckpoint(
					"cp-history-legacy",
					adminUser.ID.String(),
					legacyID,
					"history-cp-legacy",
					"2024-04-03T00:00:01Z",
				)
				oldCheckpoint.Labels = map[string]string{
					agentsv1alpha1.LabelSandboxID: "opaque-current-id",
				}
				require.NoError(t, fc.Update(t.Context(), oldCheckpoint))
				createCheckpoint(
					"cp-history-short",
					adminUser.ID.String(),
					"opaque-current-id",
					"history-cp-short",
					"2024-04-03T00:00:02Z",
				)
				return deleteCheckpoints("cp-history-legacy", "cp-history-short")
			},
			user:  adminUser,
			query: map[string]string{"sandboxID": Namespace + "--history-sandbox"},
			pages: []pageExpectation{
				{count: 1, hasNextToken: false},
			},
			expectedIDs: []string{"history-cp-legacy"},
		},
		{
			name: "no sandboxID filter returns all",
			setup: func() func() {
				createCheckpoint("cp-nofilter-1", adminUser.ID.String(), "sandbox-a", "nofilter-cp-1", "2024-05-01T00:00:01Z")
				createCheckpoint("cp-nofilter-2", adminUser.ID.String(), "sandbox-b", "nofilter-cp-2", "2024-05-01T00:00:02Z")
				return deleteCheckpoints("cp-nofilter-1", "cp-nofilter-2")
			},
			user:  adminUser,
			query: nil,
			pages: []pageExpectation{
				{count: -1, minCount: 2, hasNextToken: false},
			},
		},
		{
			name: "user isolation - only see own snapshots",
			setup: func() func() {
				// Create checkpoints for admin user
				createCheckpoint("cp-admin-1", adminUser.ID.String(), "admin-sandbox", "admin-cp-1", "2024-06-01T00:00:01Z")
				createCheckpoint("cp-admin-2", adminUser.ID.String(), "admin-sandbox", "admin-cp-2", "2024-06-01T00:00:02Z")
				// Create checkpoints for other user
				createCheckpoint("cp-other-1", otherUser.ID.String(), "other-sandbox", "other-cp-1", "2024-06-01T00:00:03Z")
				return func() {
					for _, name := range []string{"cp-admin-1", "cp-admin-2", "cp-other-1"} {
						cp := &agentsv1alpha1.Checkpoint{}
						cp.Name = name
						cp.Namespace = Namespace
						_ = fc.Delete(context.Background(), cp)
					}
				}
			},
			user:  otherUser,
			query: nil,
			pages: []pageExpectation{
				{count: 1, hasNextToken: false},
			},
			expectedIDs: []string{"other-cp-1"},
		},
		{
			name: "user is nil returns error",
			setup: func() func() {
				return func() {}
			},
			user:  nil,
			query: nil,
			pages: []pageExpectation{
				{count: -1, minCount: 2, hasNextToken: false},
			},
			expectError: &web.ApiError{
				Message: "User not found",
			},
		},
		{
			name: "invalid limit returns 400 error",
			setup: func() func() {
				return func() {}
			},
			user:  adminUser,
			query: map[string]string{"limit": "abc"},
			pages: []pageExpectation{
				{count: -1, minCount: 2, hasNextToken: false},
			},
			expectError: &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: fmt.Sprintf("Invalid limit: abc, must be between %d and %d", models.MinListLimit, models.MaxListLimit),
			},
		},
		{
			name:  "limit exceeds max limit returns 400 error",
			setup: func() func() { return func() {} },
			user:  adminUser,
			query: map[string]string{"limit": "101"},
			pages: []pageExpectation{
				{count: -1, minCount: 0, hasNextToken: false},
			},
			expectError: &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: fmt.Sprintf("Invalid limit: 101, must be between %d and %d", models.MinListLimit, models.MaxListLimit),
			},
		},
		{
			name:  "limit is zero returns 400 error",
			setup: func() func() { return func() {} },
			user:  adminUser,
			query: map[string]string{"limit": "0"},
			pages: []pageExpectation{
				{count: -1, minCount: 0, hasNextToken: false},
			},
			expectError: &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: fmt.Sprintf("Invalid limit: 0, must be between %d and %d", models.MinListLimit, models.MaxListLimit),
			},
		},
		{
			name:  "limit is negative returns 400 error",
			setup: func() func() { return func() {} },
			user:  adminUser,
			query: map[string]string{"limit": "-1"},
			pages: []pageExpectation{
				{count: -1, minCount: 0, hasNextToken: false},
			},
			expectError: &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: fmt.Sprintf("Invalid limit: -1, must be between %d and %d", models.MinListLimit, models.MaxListLimit),
			},
		},
		{
			name: "empty name query returns all",
			setup: func() func() {
				createCheckpoint("cp-empty-name-1", adminUser.ID.String(), "sandbox-empty-name-1", "empty-name-cp-1", "2024-08-01T00:00:01Z")
				createCheckpoint("cp-empty-name-2", adminUser.ID.String(), "sandbox-empty-name-2", "empty-name-cp-2", "2024-08-01T00:00:02Z")
				return deleteCheckpoints("cp-empty-name-1", "cp-empty-name-2")
			},
			user:        adminUser,
			query:       map[string]string{"name": ""},
			pages:       []pageExpectation{{count: 2, hasNextToken: false}},
			expectedIDs: []string{"empty-name-cp-1", "empty-name-cp-2"},
		},
		{
			name: "filter by snapshot name",
			setup: func() func() {
				namedA := createCheckpoint("cp-name-a", adminUser.ID.String(), "sandbox-name-a", "name-cp-a", "2024-07-01T00:00:01Z")
				namedA.Labels = map[string]string{
					agentsv1alpha1.CheckpointLabelAlias: "alias-a",
				}
				require.NoError(t, fc.Update(t.Context(), namedA))
				namedB := createCheckpoint("cp-name-b", adminUser.ID.String(), "sandbox-name-b", "name-cp-b", "2024-07-01T00:00:02Z")
				namedB.Labels = map[string]string{
					agentsv1alpha1.CheckpointLabelAlias: "alias-b",
				}
				require.NoError(t, fc.Update(t.Context(), namedB))
				createCheckpoint("cp-name-unnamed", adminUser.ID.String(), "sandbox-name-unnamed", "name-cp-unnamed", "2024-07-01T00:00:03Z")
				waitAliasVisible("alias-a", 1)
				return deleteCheckpoints("cp-name-a", "cp-name-b", "cp-name-unnamed")
			},
			user:          adminUser,
			query:         map[string]string{"name": "alias-a"},
			pages:         []pageExpectation{{count: 1, hasNextToken: false}},
			expectedIDs:   []string{"name-cp-a"},
			expectedNames: []string{"alias-a"},
		},
		{
			name: "duplicate name returns only the newest snapshot",
			setup: func() func() {
				old := createCheckpoint("cp-dup-old", adminUser.ID.String(), "sandbox-dup", "dup-cp-old", "2024-09-01T00:00:01Z")
				old.Labels = map[string]string{
					agentsv1alpha1.CheckpointLabelAlias: "alias-dup",
				}
				require.NoError(t, fc.Update(t.Context(), old))
				newest := createCheckpoint("cp-dup-new", adminUser.ID.String(), "sandbox-dup", "dup-cp-new", "2024-09-01T00:00:02Z")
				newest.Labels = map[string]string{
					agentsv1alpha1.CheckpointLabelAlias: "alias-dup",
				}
				require.NoError(t, fc.Update(t.Context(), newest))
				waitAliasVisible("alias-dup", 2)
				return deleteCheckpoints("cp-dup-old", "cp-dup-new")
			},
			user:          adminUser,
			query:         map[string]string{"name": "alias-dup"},
			pages:         []pageExpectation{{count: 1, hasNextToken: false}},
			expectedIDs:   []string{"dup-cp-new"},
			expectedNames: []string{"alias-dup"},
		},
		{
			name: "name query does not resolve other members' snapshots",
			setup: func() func() {
				other := createCheckpoint("cp-name-other-owner", otherUser.ID.String(), "sandbox-name-other", "name-cp-other", "2024-09-02T00:00:01Z")
				other.Labels = map[string]string{
					agentsv1alpha1.CheckpointLabelAlias: "alias-other-owner",
				}
				require.NoError(t, fc.Update(t.Context(), other))
				waitAliasVisible("alias-other-owner", 1)
				return deleteCheckpoints("cp-name-other-owner")
			},
			user:  adminUser,
			query: map[string]string{"name": "alias-other-owner"},
			pages: []pageExpectation{{count: 0, hasNextToken: false}},
		},
		{
			name: "name query with sandboxID filter",
			setup: func() func() {
				first := createCheckpoint("cp-name-sbx-1", adminUser.ID.String(), "sandbox-name-1", "name-cp-sbx-1", "2024-09-03T00:00:01Z")
				first.Labels = map[string]string{
					agentsv1alpha1.CheckpointLabelAlias: "alias-sbx",
				}
				require.NoError(t, fc.Update(t.Context(), first))
				second := createCheckpoint("cp-name-sbx-2", adminUser.ID.String(), "sandbox-name-2", "name-cp-sbx-2", "2024-09-03T00:00:02Z")
				second.Labels = map[string]string{
					agentsv1alpha1.CheckpointLabelAlias: "alias-sbx",
				}
				require.NoError(t, fc.Update(t.Context(), second))
				waitAliasVisible("alias-sbx", 2)
				return deleteCheckpoints("cp-name-sbx-1", "cp-name-sbx-2")
			},
			user:          adminUser,
			query:         map[string]string{"name": "alias-sbx", "sandboxID": "sandbox-name-1"},
			pages:         []pageExpectation{{count: 1, hasNextToken: false}},
			expectedIDs:   []string{"name-cp-sbx-1"},
			expectedNames: []string{"alias-sbx"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanup := tt.setup()
			defer cleanup()

			var nextToken string
			var allSnapshotIDs []string

			for pageIdx, page := range tt.pages {
				// Build query, adding nextToken if available from previous page
				query := make(map[string]string)
				for k, v := range tt.query {
					query[k] = v
				}
				if nextToken != "" {
					query["nextToken"] = nextToken
				}

				resp, apiError := controller.ListSnapshots(NewRequest(t, query, nil, nil, tt.user))

				// Handle error cases
				if tt.expectError != nil {
					assert.NotNil(t, apiError, "expected error but got nil")
					if apiError != nil {
						assert.Equal(t, tt.expectError.Code, apiError.Code, "error code should match")
						assert.Equal(t, tt.expectError.Message, apiError.Message, "error message should match")
					}
					return
				}
				assert.Nil(t, apiError, "page %d should not return error", pageIdx)

				// Verify count
				if page.count >= 0 {
					assert.Len(t, resp.Body, page.count, "page %d should have %d snapshots", pageIdx, page.count)
				} else {
					assert.GreaterOrEqual(t, len(resp.Body), page.minCount, "page %d should have at least %d snapshots", pageIdx, page.minCount)
				}

				// Verify x-next-token presence
				if page.hasNextToken {
					assert.NotEmpty(t, resp.Headers["x-next-token"], "page %d should have x-next-token", pageIdx)
					nextToken = resp.Headers["x-next-token"]
				} else {
					assert.Empty(t, resp.Headers, "page %d should not have x-next-token", pageIdx)
					nextToken = ""
				}

				// Collect all snapshot IDs for multipage tests
				for _, snapshot := range resp.Body {
					require.NotNil(t, snapshot.Names)
					if len(tt.expectedNames) > 0 {
						assert.Equal(t, tt.expectedNames, snapshot.Names)
					} else {
						assert.Empty(t, snapshot.Names)
					}
					allSnapshotIDs = append(allSnapshotIDs, snapshot.SnapshotID)
				}

				// For single page tests, verify expected IDs
				if len(tt.pages) == 1 && len(tt.expectedIDs) > 0 {
					var gotIDs []string
					for _, snapshot := range resp.Body {
						gotIDs = append(gotIDs, snapshot.SnapshotID)
					}
					assert.ElementsMatch(t, tt.expectedIDs, gotIDs, "snapshot IDs should match expected")
				}
			}

			// For multipage tests, verify total count
			if tt.expectedTotal > 0 {
				assert.Len(t, allSnapshotIDs, tt.expectedTotal, "total snapshots across all pages should be %d", tt.expectedTotal)
			}
		})
	}
}

func TestListSnapshotsResponseJSON(t *testing.T) {
	controller, fc, teardown := Setup(t)
	defer teardown()

	adminUser := adminTestUser()
	parsedTime, _ := time.Parse(time.RFC3339, "2024-02-01T00:00:00Z")
	cp := &agentsv1alpha1.Checkpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "cp-json-test",
			Namespace:         Namespace,
			UID:               types.UID(uuid.NewString()),
			CreationTimestamp: metav1.NewTime(parsedTime),
			Annotations: map[string]string{
				agentsv1alpha1.AnnotationOwner:     adminUser.ID.String(),
				agentsv1alpha1.AnnotationSandboxID: "sandbox-json-test",
			},
		},
		Spec: agentsv1alpha1.CheckpointSpec{},
		Status: agentsv1alpha1.CheckpointStatus{
			Phase:        agentsv1alpha1.CheckpointSucceeded,
			CheckpointId: "cp-json-id",
		},
	}
	require.NoError(t, fc.Create(t.Context(), cp))
	require.NoError(t, fc.Status().Update(t.Context(), cp))

	req := httptest.NewRequest(http.MethodGet, "/snapshots", nil)
	req.Header.Set(models.HeaderApiKey, InitKey)
	rec := httptest.NewRecorder()
	controller.mux.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"names":[]`)
	assert.NotContains(t, rec.Body.String(), `"names":null`)
}

func TestParseListSandboxesReservedParams(t *testing.T) {
	tests := []struct {
		name        string
		target      string
		wantMessage string
		wantOrder   string
		wantMeta    string
	}{
		{name: "duplicate order", target: "/v2/sandboxes?order=asc&order=desc", wantMessage: "Invalid order: must be a single value of asc or desc"},
		{name: "empty order", target: "/v2/sandboxes?order=", wantMessage: "Invalid order: must be a single value of asc or desc"},
		{name: "duplicate startedAfter", target: "/v2/sandboxes?startedAfter=2024-01-01T00:00:00Z&startedAfter=2024-01-02T00:00:00Z", wantMessage: "Invalid startedAfter: must be a single RFC3339 timestamp"},
		{name: "duplicate nextToken", target: "/v2/sandboxes?nextToken=abc&nextToken=def", wantMessage: "Invalid nextToken: must be a single non-empty value"},
		{name: "empty nextToken", target: "/v2/sandboxes?nextToken=", wantMessage: "Invalid nextToken: must be a single non-empty value"},
		{name: "duplicate limit", target: "/v2/sandboxes?limit=10&limit=20", wantMessage: "Invalid limit: must be a single value between 1 and 100"},
		{name: "empty limit", target: "/v2/sandboxes?limit=", wantMessage: "Invalid limit: must be a single value between 1 and 100"},
		{name: "metadata keeps reserved names", target: "/v2/sandboxes?metadata=order%3Ddesc%26template%3Dpython", wantOrder: "", wantMeta: "desc"},
		{name: "asc order", target: "/v2/sandboxes?order=asc", wantOrder: "asc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.target, nil)
			parsed, apiErr := parseListSandboxesRequest(req)
			if tt.wantMessage != "" {
				require.NotNil(t, apiErr)
				assert.Equal(t, http.StatusBadRequest, apiErr.Code)
				assert.Equal(t, tt.wantMessage, apiErr.Message)
				return
			}
			require.Nil(t, apiErr)
			assert.Equal(t, tt.wantOrder, parsed.Order)
			assert.Equal(t, tt.wantMeta, parsed.Metadata["order"])
		})
	}
}
