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

package proxyutils

import (
	"testing"

	"github.com/stretchr/testify/assert"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func TestShouldDeleteRoute(t *testing.T) {
	tests := []struct {
		name  string
		state string
		want  bool
	}{
		{name: "dead is deleted", state: agentsv1alpha1.SandboxStateDead, want: true},
		{name: "running is kept", state: agentsv1alpha1.SandboxStateRunning, want: false},
		{name: "paused is kept", state: agentsv1alpha1.SandboxStatePaused, want: false},
		{name: "creating is kept", state: agentsv1alpha1.SandboxStateCreating, want: false},
		{name: "available is kept", state: agentsv1alpha1.SandboxStateAvailable, want: false},
		{name: "empty state is kept", state: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ShouldDeleteRoute(tt.state))
		})
	}
}
