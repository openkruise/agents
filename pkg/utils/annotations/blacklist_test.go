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

package annotations

import (
	"testing"

	"github.com/stretchr/testify/assert"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func TestIsBlackListed(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want bool
	}{
		{
			name: "internal prefix is blacklisted",
			key:  agentsv1alpha1.InternalPrefix + "sandbox-claim-name",
			want: true,
		},
		{
			name: "e2b prefix is blacklisted",
			key:  agentsv1alpha1.E2BPrefix + "template-id",
			want: true,
		},
		{
			name: "plain user key is allowed",
			key:  "team.example.com/owner",
			want: false,
		},
		{
			name: "security prefix is allowed since it does not match internal prefix literally",
			key:  "security.agents.kruise.io/agent-name",
			want: false,
		},
		{
			name: "internal prefix without trailing path is allowed",
			key:  "agents.kruise.io",
			want: false,
		},
		{
			name: "empty key is allowed",
			key:  "",
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsBlackListed(tt.key))
		})
	}
}

func TestFilterBlackListed(t *testing.T) {
	tests := []struct {
		name  string
		input map[string]string
		want  map[string]string
	}{
		{
			name:  "nil input returns nil",
			input: nil,
			want:  nil,
		},
		{
			name:  "empty input returns nil",
			input: map[string]string{},
			want:  nil,
		},
		{
			name: "all keys blacklisted returns nil",
			input: map[string]string{
				agentsv1alpha1.InternalPrefix + "claim-time": "now",
				agentsv1alpha1.E2BPrefix + "template-id":     "tpl",
			},
			want: nil,
		},
		{
			name: "user keys pass through",
			input: map[string]string{
				"team.example.com/owner": "alice",
				"plain-key":              "v",
			},
			want: map[string]string{
				"team.example.com/owner": "alice",
				"plain-key":              "v",
			},
		},
		{
			name: "mixed keys keep only non-blacklisted ones",
			input: map[string]string{
				agentsv1alpha1.InternalPrefix + "claim-time": "now",
				"security.agents.kruise.io/agent-name":       "agent",
				"team.example.com/owner":                     "alice",
			},
			want: map[string]string{
				"security.agents.kruise.io/agent-name": "agent",
				"team.example.com/owner":               "alice",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, FilterBlackListed(tt.input))
		})
	}
}
