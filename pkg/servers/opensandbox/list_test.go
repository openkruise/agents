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

package opensandbox

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
)

func TestParseListQuery(t *testing.T) {
	tests := []struct {
		name         string
		query        url.Values
		wantErr      bool
		wantCode     int
		wantStates   []SandboxState
		wantMetadata map[string]string
		wantPage     int
		wantPageSize int
	}{
		{
			name:         "empty query uses defaults",
			query:        url.Values{},
			wantPage:     defaultListPage,
			wantPageSize: defaultListPageSize,
			wantMetadata: map[string]string{},
		},
		{
			name:       "repeated state params are OR-ed",
			query:      url.Values{"state": []string{"Running", "Paused"}},
			wantStates: []SandboxState{SandboxStateRunning, SandboxStatePaused},
			wantPage:   defaultListPage, wantPageSize: defaultListPageSize, wantMetadata: map[string]string{},
		},
		{
			name:       "comma separated state param is split",
			query:      url.Values{"state": []string{"Running,Paused"}},
			wantStates: []SandboxState{SandboxStateRunning, SandboxStatePaused},
			wantPage:   defaultListPage, wantPageSize: defaultListPageSize, wantMetadata: map[string]string{},
		},
		{
			name:     "unknown state is rejected",
			query:    url.Values{"state": []string{"Bogus"}},
			wantErr:  true,
			wantCode: 400,
		},
		{
			name:         "metadata is decoded",
			query:        url.Values{"metadata": []string{url.QueryEscape("project=Apollo&note=Demo")}},
			wantMetadata: map[string]string{"project": "Apollo", "note": "Demo"},
			wantPage:     defaultListPage, wantPageSize: defaultListPageSize,
		},
		{
			name:     "blacklisted metadata key is rejected",
			query:    url.Values{"metadata": []string{url.QueryEscape(agentsv1alpha1.InternalPrefix + "owner=x")}},
			wantErr:  true,
			wantCode: 400,
		},
		{
			name:     "malformed metadata pair is rejected",
			query:    url.Values{"metadata": []string{url.QueryEscape("novalue")}},
			wantErr:  true,
			wantCode: 400,
		},
		{
			name:     "malformed metadata encoding is rejected",
			query:    url.Values{"metadata": []string{"%zz"}},
			wantErr:  true,
			wantCode: 400,
		},
		{
			name:     "page below one is rejected",
			query:    url.Values{"page": []string{"0"}},
			wantErr:  true,
			wantCode: 400,
		},
		{
			name:     "non integer page is rejected",
			query:    url.Values{"page": []string{"abc"}},
			wantErr:  true,
			wantCode: 400,
		},
		{
			name:     "pageSize above max is rejected",
			query:    url.Values{"pageSize": []string{"101"}},
			wantErr:  true,
			wantCode: 400,
		},
		{
			name:     "pageSize below min is rejected",
			query:    url.Values{"pageSize": []string{"0"}},
			wantErr:  true,
			wantCode: 400,
		},
		{
			name:  "explicit pagination is honored",
			query: url.Values{"page": []string{"3"}, "pageSize": []string{"50"}},
			// wantStates nil, wantMetadata empty
			wantPage: 3, wantPageSize: 50, wantMetadata: map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, apiErr := parseListQuery(tt.query)
			if tt.wantErr {
				require.NotNil(t, apiErr)
				assert.Equal(t, tt.wantCode, apiErr.Code)
				return
			}
			require.Nil(t, apiErr)
			assert.Equal(t, tt.wantStates, got.states)
			assert.Equal(t, tt.wantMetadata, got.metadata)
			assert.Equal(t, tt.wantPage, got.page)
			assert.Equal(t, tt.wantPageSize, got.pageSize)
			assert.NotNil(t, got.filter)
		})
	}
}

func TestBuildListFilter(t *testing.T) {
	running := runningClaimedSandbox(t, "team-a", "sbx-run", "id-run", func(s *agentsv1alpha1.Sandbox) {
		s.Annotations["project"] = "Apollo"
	})
	paused := runningClaimedSandbox(t, "team-a", "sbx-paused", "id-paused", func(s *agentsv1alpha1.Sandbox) {
		s.Status.Phase = agentsv1alpha1.SandboxPaused
		s.Status.Conditions = nil
		s.Spec.Paused = true
	})
	terminating := runningClaimedSandbox(t, "team-a", "sbx-gone", "id-gone", func(s *agentsv1alpha1.Sandbox) {
		s.Status.Phase = agentsv1alpha1.SandboxTerminating
	})

	tests := []struct {
		name     string
		states   []SandboxState
		metadata map[string]string
		sbx      infra.Sandbox
		want     bool
	}{
		{name: "no filter accepts running", sbx: running, want: true},
		{name: "no filter rejects non-viewable", sbx: terminating, want: false},
		{name: "state filter matches", states: []SandboxState{SandboxStateRunning}, sbx: running, want: true},
		{name: "state filter excludes", states: []SandboxState{SandboxStatePaused}, sbx: running, want: false},
		{name: "state filter matches paused", states: []SandboxState{SandboxStatePaused}, sbx: paused, want: true},
		{name: "metadata filter matches", metadata: map[string]string{"project": "Apollo"}, sbx: running, want: true},
		{name: "metadata filter excludes", metadata: map[string]string{"project": "Other"}, sbx: running, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter := buildListFilter(tt.states, tt.metadata)
			assert.Equal(t, tt.want, filter(tt.sbx))
		})
	}
}

func TestProjectListPage(t *testing.T) {
	// Build five running sandboxes with a stable order-independent identity.
	var sandboxes []infra.Sandbox
	for i := 0; i < 5; i++ {
		sandboxes = append(sandboxes, &sandboxcr.Sandbox{Sandbox: runningClaimedSandbox(t, "team-a", "sbx", "id", nil).Sandbox})
	}

	tests := []struct {
		name        string
		page        int
		pageSize    int
		wantItems   int
		wantTotal   int
		wantPages   int
		wantHasNext bool
	}{
		{name: "first page of two", page: 1, pageSize: 2, wantItems: 2, wantTotal: 5, wantPages: 3, wantHasNext: true},
		{name: "last partial page", page: 3, pageSize: 2, wantItems: 1, wantTotal: 5, wantPages: 3, wantHasNext: false},
		{name: "single page covers all", page: 1, pageSize: 10, wantItems: 5, wantTotal: 5, wantPages: 1, wantHasNext: false},
		{name: "page beyond range is empty", page: 9, pageSize: 2, wantItems: 0, wantTotal: 5, wantPages: 3, wantHasNext: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, apiErr := projectListPage(sandboxes, tt.page, tt.pageSize)
			require.Nil(t, apiErr)
			require.NotNil(t, got)
			// Items must never be nil so an empty page serializes as [].
			require.NotNil(t, got.Items)
			assert.Len(t, got.Items, tt.wantItems)
			assert.Equal(t, tt.wantTotal, got.Pagination.TotalItems)
			assert.Equal(t, tt.wantPages, got.Pagination.TotalPages)
			assert.Equal(t, tt.page, got.Pagination.Page)
			assert.Equal(t, tt.pageSize, got.Pagination.PageSize)
			assert.Equal(t, tt.wantHasNext, got.Pagination.HasNextPage)
		})
	}
}

func TestProjectListPageEmpty(t *testing.T) {
	got, apiErr := projectListPage(nil, 1, defaultListPageSize)
	require.Nil(t, apiErr)
	require.NotNil(t, got.Items)
	assert.Empty(t, got.Items)
	assert.Equal(t, 0, got.Pagination.TotalItems)
	assert.Equal(t, 0, got.Pagination.TotalPages)
	assert.False(t, got.Pagination.HasNextPage)
}
