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
	"fmt"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestListSandboxes(t *testing.T) {
	templateName := "test-template"
	controller, fc, teardown := Setup(t)
	defer teardown()
	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
	}
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanup := CreateSandboxPool(t, controller, templateName, 10)
			defer cleanup()

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
			time.Sleep(200 * time.Millisecond)

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

func TestListSandboxes_Pagination(t *testing.T) {
	templateName := "pagination-template"
	controller, fc, teardown := Setup(t)
	defer teardown()
	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
	}

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
		"2024-01-01T00:00:02Z",
		"2024-01-01T00:00:03Z",
		"2024-01-01T00:00:04Z",
		"2024-01-01T00:00:05Z",
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
			name:  "first page with limit",
			query: map[string]string{"limit": "2"},
			pages: []pageExpectation{
				{count: 2, hasNextToken: true, startedAtIdxs: []int{0, 1}},
			},
		},
		{
			name:  "next page with nextToken",
			query: map[string]string{"limit": "2"},
			pages: []pageExpectation{
				{count: 2, hasNextToken: true, startedAtIdxs: []int{0, 1}},
				{count: 2, hasNextToken: true, startedAtIdxs: []int{2, 3}},
				{count: 1, hasNextToken: false, startedAtIdxs: []int{4}},
			},
		},
		{
			name:  "start with nextToken",
			query: map[string]string{"limit": "2", "nextToken": "2024-01-01T00:00:02Z"},
			pages: []pageExpectation{
				{count: 2, hasNextToken: true, startedAtIdxs: []int{2, 3}},
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
					assert.Len(t, resp.Body, page.count, "page %d should have %d sandboxes", pageIdx, page.count)
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
					assert.True(t, sort.StringsAreSorted(gotStartedAts), "page %d: sandboxes should be sorted by claim time", pageIdx)
					for i, idx := range page.startedAtIdxs {
						assert.Equal(t, claimTimes[idx], gotStartedAts[i], "page %d: sandbox %d should have StartedAt=%s", pageIdx, i, claimTimes[idx])
					}
				}
			}
		})
	}
}

func TestListSnapshots(t *testing.T) {
	controller, fc, teardown := Setup(t)
	defer teardown()

	adminUser := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
	}

	otherUserID := uuid.New()
	otherUser := &models.CreatedTeamAPIKey{
		ID:   otherUserID,
		Key:  "other-key-123",
		Name: "other",
	}

	// Helper to create a checkpoint with given parameters
	createCheckpoint := func(name, owner, sandboxID, checkpointID, creationTime string) *agentsv1alpha1.Checkpoint {
		parsedTime, _ := time.Parse(time.RFC3339, creationTime)
		cp := &agentsv1alpha1.Checkpoint{
			ObjectMeta: metav1.ObjectMeta{
				Name:              name,
				Namespace:         Namespace,
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
				return func() {
					for _, name := range []string{"cp-filter-1", "cp-filter-2", "cp-filter-3"} {
						cp := &agentsv1alpha1.Checkpoint{}
						cp.Name = name
						cp.Namespace = Namespace
						_ = fc.Delete(context.Background(), cp)
					}
				}
			},
			user:  adminUser,
			query: map[string]string{"sandboxID": "target-sandbox"},
			pages: []pageExpectation{
				{count: 2, hasNextToken: false},
			},
			expectedIDs: []string{"filter-cp-1", "filter-cp-3"},
		},
		{
			name: "no sandboxID filter returns all",
			setup: func() func() {
				createCheckpoint("cp-nofilter-1", adminUser.ID.String(), "sandbox-a", "nofilter-cp-1", "2024-05-01T00:00:01Z")
				createCheckpoint("cp-nofilter-2", adminUser.ID.String(), "sandbox-b", "nofilter-cp-2", "2024-05-01T00:00:02Z")
				return func() {
					for _, name := range []string{"cp-nofilter-1", "cp-nofilter-2"} {
						cp := &agentsv1alpha1.Checkpoint{}
						cp.Name = name
						cp.Namespace = Namespace
						_ = fc.Delete(context.Background(), cp)
					}
				}
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
