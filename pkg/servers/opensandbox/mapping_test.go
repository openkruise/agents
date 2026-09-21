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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func TestResolveTemplateID(t *testing.T) {
	tests := []struct {
		name         string
		aliases      map[string]string
		imageURI     string
		wantTemplate string
		wantErr      bool
	}{
		{
			name:         "exact hit returns template",
			aliases:      map[string]string{"python:3.11": "python-tpl"},
			imageURI:     "python:3.11",
			wantTemplate: "python-tpl",
		},
		{
			name:    "miss returns ErrImageNotMapped",
			aliases: map[string]string{"python:3.11": "python-tpl"},
			// A different tag is a different image; Phase 1 does no
			// registry/tag normalization, so this must miss.
			imageURI: "python:3.12",
			wantErr:  true,
		},
		{
			name:     "empty image uri is a miss",
			aliases:  map[string]string{"python:3.11": "python-tpl"},
			imageURI: "",
			wantErr:  true,
		},
		{
			name:     "nil alias table is a miss",
			aliases:  nil,
			imageURI: "python:3.11",
			wantErr:  true,
		},
		{
			name:     "empty alias table is a miss",
			aliases:  map[string]string{},
			imageURI: "python:3.11",
			wantErr:  true,
		},
		{
			name:     "alias mapped to empty template is a miss",
			aliases:  map[string]string{"python:3.11": ""},
			imageURI: "python:3.11",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveTemplateID(tt.aliases, tt.imageURI)
			if tt.wantErr {
				require.Error(t, err)
				// The caller classifies this as 400 by type, not by message,
				// so the concrete type is part of the contract.
				var notMapped *ErrImageNotMapped
				require.ErrorAs(t, err, &notMapped)
				assert.Equal(t, tt.imageURI, notMapped.ImageURI)
				assert.Empty(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantTemplate, got)
		})
	}
}

func TestMapState(t *testing.T) {
	tests := []struct {
		name        string
		agentsState string
		reason      string
		want        SandboxState
	}{
		{
			name:        "running maps to Running",
			agentsState: agentsv1alpha1.SandboxStateRunning,
			reason:      "RunningResourceClaimedAndReady",
			want:        SandboxStateRunning,
		},
		{
			name:        "claimed but not ready is surfaced as Running",
			agentsState: agentsv1alpha1.SandboxStateDead,
			reason:      "RunningResourceClaimedButNotReady",
			want:        SandboxStateRunning,
		},
		{
			name:        "paused maps to Paused",
			agentsState: agentsv1alpha1.SandboxStatePaused,
			reason:      "RunningResourceClaimedAndPaused",
			want:        SandboxStatePaused,
		},
		{
			name:        "creating maps to Pending",
			agentsState: agentsv1alpha1.SandboxStateCreating,
			reason:      agentsv1alpha1.SandboxStateReasonResourcePending,
			want:        SandboxStatePending,
		},
		{
			name:        "dead with terminal reason maps to Terminated",
			agentsState: agentsv1alpha1.SandboxStateDead,
			reason:      "ResourceTerminating",
			want:        SandboxStateTerminated,
		},
		{
			name:        "unknown state falls back to Running",
			agentsState: "some-future-state",
			reason:      "Whatever",
			want:        SandboxStateRunning,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, MapState(tt.agentsState, tt.reason))
		})
	}
}

func TestParseImageAlias(t *testing.T) {
	tests := []struct {
		name         string
		entry        string
		wantImage    string
		wantTemplate string
		wantErr      bool
	}{
		{name: "simple pair", entry: "python:3.11=python-tpl", wantImage: "python:3.11", wantTemplate: "python-tpl"},
		{name: "surrounding spaces are trimmed", entry: "  python:3.11 = python-tpl  ", wantImage: "python:3.11", wantTemplate: "python-tpl"},
		{name: "registry with port", entry: "registry.local:5000/app:v1=app-tpl", wantImage: "registry.local:5000/app:v1", wantTemplate: "app-tpl"},
		{name: "missing separator", entry: "python:3.11", wantErr: true},
		{name: "empty image uri", entry: "=python-tpl", wantErr: true},
		{name: "empty template id", entry: "python:3.11=", wantErr: true},
		{name: "empty entry", entry: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			image, template, err := ParseImageAlias(tt.entry)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantImage, image)
			assert.Equal(t, tt.wantTemplate, template)
		})
	}
}

func TestParseImageAliases(t *testing.T) {
	tests := []struct {
		name    string
		entries []string
		want    map[string]string
		wantErr bool
	}{
		{
			name:    "nil list yields nil map",
			entries: nil,
			want:    nil,
		},
		{
			name:    "empty list yields nil map",
			entries: []string{},
			want:    nil,
		},
		{
			name:    "single entry",
			entries: []string{"python:3.11=python-tpl"},
			want:    map[string]string{"python:3.11": "python-tpl"},
		},
		{
			name:    "multiple distinct entries",
			entries: []string{"python:3.11=python-tpl", "node:20=node-tpl"},
			want:    map[string]string{"python:3.11": "python-tpl", "node:20": "node-tpl"},
		},
		{
			name:    "duplicate image uri is rejected",
			entries: []string{"python:3.11=tpl-a", "python:3.11=tpl-b"},
			wantErr: true,
		},
		{
			name:    "malformed entry is rejected",
			entries: []string{"python:3.11=python-tpl", "bad-entry"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseImageAliases(tt.entries)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSplitImageAliasesEnv(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  []string
	}{
		{name: "empty value yields no entries", value: "", want: nil},
		{name: "blank value yields no entries", value: "   ", want: nil},
		{name: "single entry", value: "python:3.11=python-tpl", want: []string{"python:3.11=python-tpl"}},
		{
			name:  "comma separated entries are trimmed",
			value: " python:3.11=python-tpl , nginx:1.27=web-tpl ",
			want:  []string{"python:3.11=python-tpl", "nginx:1.27=web-tpl"},
		},
		{
			name:  "empty items are dropped",
			value: "python:3.11=python-tpl,, ,nginx:1.27=web-tpl,",
			want:  []string{"python:3.11=python-tpl", "nginx:1.27=web-tpl"},
		},
		{
			name:  "malformed entries pass through for ParseImageAliases",
			value: "bad-entry,python:3.11=python-tpl",
			want:  []string{"bad-entry", "python:3.11=python-tpl"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, SplitImageAliasesEnv(tt.value))
		})
	}
}
