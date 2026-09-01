/*
Copyright 2025 The Kruise Authors.

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

package fieldindex

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func TestCheckpointSandboxNameIndexFunc(t *testing.T) {
	tests := []struct {
		name string
		obj  client.Object
		want []string
	}{
		{
			name: "ignores non-checkpoint object",
			obj:  &corev1.Pod{},
		},
		{
			name: "ignores checkpoint without sandbox or pod name",
			obj:  &agentsv1alpha1.Checkpoint{},
		},
		{
			name: "ignores empty sandbox and pod names",
			obj: &agentsv1alpha1.Checkpoint{Spec: agentsv1alpha1.CheckpointSpec{
				SandboxName: ptr.To(""),
				PodName:     ptr.To(""),
			}},
		},
		{
			name: "indexes sandbox name",
			obj: &agentsv1alpha1.Checkpoint{Spec: agentsv1alpha1.CheckpointSpec{
				SandboxName: ptr.To("sandbox-a"),
			}},
			want: []string{"sandbox-a"},
		},
		{
			name: "falls back to pod name",
			obj: &agentsv1alpha1.Checkpoint{Spec: agentsv1alpha1.CheckpointSpec{
				PodName: ptr.To("sandbox-from-pod"),
			}},
			want: []string{"sandbox-from-pod"},
		},
		{
			name: "falls back to pod name when sandbox name is empty",
			obj: &agentsv1alpha1.Checkpoint{Spec: agentsv1alpha1.CheckpointSpec{
				SandboxName: ptr.To(""),
				PodName:     ptr.To("sandbox-from-pod"),
			}},
			want: []string{"sandbox-from-pod"},
		},
		{
			name: "prefers sandbox name when both names are set",
			obj: &agentsv1alpha1.Checkpoint{Spec: agentsv1alpha1.CheckpointSpec{
				SandboxName: ptr.To("sandbox-a"),
				PodName:     ptr.To("pod-a"),
			}},
			want: []string{"sandbox-a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, CheckpointSandboxNameIndexFunc(tt.obj))
		})
	}
}
