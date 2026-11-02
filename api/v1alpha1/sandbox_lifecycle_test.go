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

package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestUpgradeActionAliasCompatibility(t *testing.T) {
	old := &UpgradeAction{Exec: &corev1.ExecAction{Command: []string{"backup"}}, TimeoutSeconds: 10}
	var current *LifecycleAction = old
	copied := current.DeepCopy()
	require.NotNil(t, copied)
	assert.Equal(t, int32(10), copied.TimeoutSeconds)
	assert.Equal(t, []string{"backup"}, copied.Exec.Command)

	copied.Exec.Command[0] = "changed"
	assert.Equal(t, []string{"backup"}, old.Exec.Command)
}

func TestSandboxLifecycleDeepCopy(t *testing.T) {
	original := &SandboxLifecycle{
		PrePause:   &LifecycleAction{Exec: &corev1.ExecAction{Command: []string{"flush"}}},
		PostResume: &LifecycleAction{Exec: &corev1.ExecAction{Command: []string{"restore"}}},
	}

	copied := original.DeepCopy()
	require.NotNil(t, copied)
	copied.PrePause.Exec.Command[0] = "changed-pre"
	copied.PostResume.Exec.Command[0] = "changed-post"

	assert.Equal(t, []string{"flush"}, original.PrePause.Exec.Command)
	assert.Equal(t, []string{"restore"}, original.PostResume.Exec.Command)
}
