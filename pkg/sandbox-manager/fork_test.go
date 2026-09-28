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

package sandbox_manager

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
)

func TestForkLeaseOwnerReference(t *testing.T) {
	source := sandboxcr.AsSandbox(&v1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{
		Namespace: "default",
		Name:      "source",
		UID:       types.UID("source-uid"),
	}}, nil)

	owner := forkLeaseOwnerReference(source)
	assert.Equal(t, v1alpha1.GroupVersion.String(), owner.APIVersion)
	assert.Equal(t, "Sandbox", owner.Kind)
	assert.Equal(t, source.GetName(), owner.Name)
	assert.Equal(t, source.GetUID(), owner.UID)
	assert.True(t, *owner.Controller)
}

func TestForkTimeoutOptions(t *testing.T) {
	now := time.Date(2026, time.September, 28, 6, 0, 0, 0, time.UTC)
	tests := []struct {
		name             string
		opts             ForkSandboxOptions
		wantPauseTime    time.Time
		wantShutdownTime time.Time
	}{
		{
			name: "shutdown timeout starts at child creation",
			opts: ForkSandboxOptions{
				TimeoutSeconds: 15,
			},
			wantShutdownTime: now.Add(15 * time.Second),
		},
		{
			name: "auto pause timeout uses child creation time and retention",
			opts: ForkSandboxOptions{
				TimeoutSeconds:  15,
				AutoPause:       true,
				PausedRetention: 30 * time.Minute,
			},
			wantPauseTime:    now.Add(15 * time.Second),
			wantShutdownTime: now.Add(30*time.Minute + 15*time.Second),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := forkTimeoutOptions(now, tt.opts)
			assert.Equal(t, tt.wantPauseTime, got.PauseTime)
			assert.Equal(t, tt.wantShutdownTime, got.ShutdownTime)
		})
	}
}
