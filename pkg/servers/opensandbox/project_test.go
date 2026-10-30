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
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
)

// runningClaimedSandbox builds a claimed, running, ready sandbox with the given
// id/namespace/name, claim time, shutdown time, labels and annotations. Times are
// relative to now so GetState never flips to dead on a stale wall clock.
func runningClaimedSandbox(t *testing.T, namespace, name, sandboxID string, mutate func(*agentsv1alpha1.Sandbox)) *sandboxcr.Sandbox {
	t.Helper()
	claimTime := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Second)
	shutdownTime := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Second)
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			Labels: map[string]string{
				agentsv1alpha1.LabelSandboxID: sandboxID,
			},
			Annotations: map[string]string{
				agentsv1alpha1.AnnotationClaimTime: claimTime.Format(time.RFC3339),
			},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			ShutdownTime: &metav1.Time{Time: shutdownTime},
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
			Conditions: []metav1.Condition{
				{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue},
			},
		},
	}
	if mutate != nil {
		mutate(sbx)
	}
	return &sandboxcr.Sandbox{Sandbox: sbx}
}

func TestProjectSandbox(t *testing.T) {
	t.Run("running sandbox projects required keys", func(t *testing.T) {
		sbx := runningClaimedSandbox(t, "team-a", "sbx-1", "sbx-id-1", func(s *agentsv1alpha1.Sandbox) {
			s.Annotations["user-key"] = "user-val"
		})

		got, err := projectSandbox(sbx)
		require.NoError(t, err)
		assert.Equal(t, "sbx-id-1", got.ID)
		assert.Equal(t, SandboxStateRunning, got.Status.State)
		assert.Equal(t, []string{}, got.Entrypoint)
		assert.Equal(t, "user-val", got.Metadata["user-key"])
		assert.Equal(t, "team-a/sbx-1", got.Metadata[MetadataKeySandboxResource])
		assert.NotEmpty(t, got.CreatedAt)
		assert.NotEmpty(t, got.ExpiresAt)

		// The SDK pops id/status/createdAt/entrypoint unconditionally, so all
		// four keys must survive serialization regardless of value.
		raw, err := json.Marshal(got)
		require.NoError(t, err)
		var body map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(raw, &body))
		for _, key := range []string{"id", "status", "createdAt", "entrypoint"} {
			assert.Contains(t, body, key)
		}
	})

	t.Run("internal annotations are not leaked", func(t *testing.T) {
		sbx := runningClaimedSandbox(t, "team-a", "sbx-1", "sbx-id-1", func(s *agentsv1alpha1.Sandbox) {
			s.Annotations[agentsv1alpha1.AnnotationOwner] = "some-user-id"
			s.Annotations["user-key"] = "user-val"
		})

		got, err := projectSandbox(sbx)
		require.NoError(t, err)
		assert.NotContains(t, got.Metadata, agentsv1alpha1.AnnotationOwner)
		assert.Equal(t, "user-val", got.Metadata["user-key"])
	})
}

func TestResolveExpiresAt(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	shutdown := now.Add(30 * time.Minute)
	pause := now.Add(10 * time.Minute)

	t.Run("shutdown time when no pause time", func(t *testing.T) {
		sbx := runningClaimedSandbox(t, "team-a", "sbx-1", "sbx-id-1", func(s *agentsv1alpha1.Sandbox) {
			s.Spec.ShutdownTime = &metav1.Time{Time: shutdown}
			s.Spec.PauseTime = nil
		})
		assert.True(t, resolveExpiresAt(sbx).Equal(shutdown))
	})

	t.Run("pause time takes precedence", func(t *testing.T) {
		sbx := runningClaimedSandbox(t, "team-a", "sbx-1", "sbx-id-1", func(s *agentsv1alpha1.Sandbox) {
			s.Spec.ShutdownTime = &metav1.Time{Time: shutdown}
			s.Spec.PauseTime = &metav1.Time{Time: pause}
		})
		assert.True(t, resolveExpiresAt(sbx).Equal(pause))
	})
}

func TestIsSandboxViewable(t *testing.T) {
	tests := []struct {
		name  string
		build func(t *testing.T) *sandboxcr.Sandbox
		want  bool
	}{
		{
			name:  "running is viewable",
			build: func(t *testing.T) *sandboxcr.Sandbox { return runningClaimedSandbox(t, "team-a", "sbx", "id", nil) },
			want:  true,
		},
		{
			name: "creating is hidden",
			build: func(t *testing.T) *sandboxcr.Sandbox {
				return runningClaimedSandbox(t, "team-a", "sbx", "id", func(s *agentsv1alpha1.Sandbox) {
					s.Status.Phase = agentsv1alpha1.SandboxPending
					s.Status.Conditions = nil
				})
			},
			want: false,
		},
		{
			name: "terminating is hidden",
			build: func(t *testing.T) *sandboxcr.Sandbox {
				return runningClaimedSandbox(t, "team-a", "sbx", "id", func(s *agentsv1alpha1.Sandbox) {
					s.Status.Phase = agentsv1alpha1.SandboxTerminating
				})
			},
			want: false,
		},
		{
			name: "failed is hidden",
			build: func(t *testing.T) *sandboxcr.Sandbox {
				return runningClaimedSandbox(t, "team-a", "sbx", "id", func(s *agentsv1alpha1.Sandbox) {
					s.Status.Phase = agentsv1alpha1.SandboxFailed
				})
			},
			want: false,
		},
		{
			name: "reserved failed is hidden",
			build: func(t *testing.T) *sandboxcr.Sandbox {
				return runningClaimedSandbox(t, "team-a", "sbx", "id", func(s *agentsv1alpha1.Sandbox) {
					s.Labels[agentsv1alpha1.LabelSandboxReservedFailed] = agentsv1alpha1.True
				})
			},
			want: false,
		},
		{
			name: "deletion timestamp is hidden",
			build: func(t *testing.T) *sandboxcr.Sandbox {
				now := metav1.Now()
				return runningClaimedSandbox(t, "team-a", "sbx", "id", func(s *agentsv1alpha1.Sandbox) {
					s.DeletionTimestamp = &now
				})
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isSandboxViewable(tt.build(t)))
		})
	}
}
