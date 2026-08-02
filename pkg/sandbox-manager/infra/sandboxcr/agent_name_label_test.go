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

package sandboxcr

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

// TestReconcileAgentNameLabels exercises the convergence of the agent-name key
// towards the expected value the caller resolved from its own protocol input: a
// valid value must be written to both the sandbox labels and the pod template
// labels (overwriting whatever was there), an empty value must strip both so a
// pooled sandbox cannot leak the previous claimer's identity, the annotation
// form must never survive either way, and a value violating Kubernetes
// label-value constraints must be rejected before anything is mutated.
func TestReconcileAgentNameLabels(t *testing.T) {
	newTemplate := func(labels map[string]string) *corev1.PodTemplateSpec {
		return &corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
		}
	}

	tests := []struct {
		name           string
		sbx            *v1alpha1.Sandbox
		agentName      string
		expectError    string
		wantLabel      string // expected sandbox label value; "" means the key must be absent
		wantPodLabel   string // expected pod template label value; "" means the key must be absent
		wantAnnotation string // expected annotation value; "" means the key must be absent
		checkPodLabels bool   // whether the sandbox carries an inline pod template to verify
		reason         string
	}{
		{
			name:   "nil sandbox is a no-op",
			sbx:    nil,
			reason: "defensive guard mirroring identity.IsIDTokenRequested's nil tolerance",
		},
		{
			name: "no expected value on a clean sandbox leaves it untouched",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "sbx-a"},
				Spec: v1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: newTemplate(nil),
					},
				},
			},
			checkPodLabels: true,
			reason:         "non-identity sandboxes must stay inert",
		},
		{
			name: "no expected value strips residual labels",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "sbx-b",
					Labels: map[string]string{identity.AnnotationAgentName: "previous-agent"},
				},
				Spec: v1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: newTemplate(map[string]string{identity.AnnotationAgentName: "previous-agent"}),
					},
				},
			},
			checkPodLabels: true,
			reason:         "a pooled sandbox must never inherit the previous claimer's identity",
		},
		{
			name: "annotation input is dropped even without an expected value",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "sbx-c",
					Annotations: map[string]string{identity.AnnotationAgentName: "annotation-agent"},
				},
				Spec: v1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: newTemplate(nil),
					},
				},
			},
			checkPodLabels: true,
			reason:         "only the resolved expected value is authoritative, never the annotation",
		},
		{
			name: "expected value is written to sandbox and pod template labels",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "sbx-d"},
				Spec: v1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: newTemplate(nil),
					},
				},
			},
			agentName:      "my-agent",
			wantLabel:      "my-agent",
			wantPodLabel:   "my-agent",
			checkPodLabels: true,
			reason:         "the canonical convergence path",
		},
		{
			name: "expected value overwrites drifting labels and drops the annotation",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "sbx-e",
					Annotations: map[string]string{identity.AnnotationAgentName: "annotation-agent"},
					Labels:      map[string]string{identity.AnnotationAgentName: "stale-agent"},
				},
				Spec: v1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: newTemplate(map[string]string{identity.AnnotationAgentName: "stale-agent"}),
					},
				},
			},
			agentName:      "my-agent",
			wantLabel:      "my-agent",
			wantPodLabel:   "my-agent",
			checkPodLabels: true,
			reason:         "labels are the carrier and the resolved value is the only source",
		},
		{
			name: "nil pod template writes the sandbox label only",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "sbx-f"},
			},
			agentName: "my-agent",
			wantLabel: "my-agent",
			reason:    "TemplateRef-resolved sandboxes carry no inline template to mutate",
		},
		{
			name: "value exceeding 63 characters is rejected",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "sbx-g",
					Annotations: map[string]string{identity.AnnotationAgentName: strings.Repeat("a", 64)},
				},
				Spec: v1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: newTemplate(nil),
					},
				},
			},
			agentName:      strings.Repeat("a", 64),
			expectError:    "is not a valid label value",
			wantAnnotation: strings.Repeat("a", 64),
			checkPodLabels: true,
			reason:         "fail fast instead of letting the apiserver reject the write",
		},
		{
			name: "value with invalid characters is rejected",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "sbx-h"},
				Spec: v1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: newTemplate(nil),
					},
				},
			},
			agentName:      "agent/with/slashes",
			expectError:    "is not a valid label value",
			checkPodLabels: true,
			reason:         "label values allow only alphanumerics, '-', '_' and '.'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := reconcileAgentNameLabels(tt.sbx, tt.agentName)

			if tt.expectError != "" {
				require.Error(t, err, tt.reason)
				assert.Contains(t, err.Error(), tt.expectError)
				assert.Contains(t, err.Error(), identity.AnnotationAgentName,
					"error must name the offending key for diagnosability")
			} else {
				require.NoError(t, err, tt.reason)
			}
			if tt.sbx == nil {
				return
			}

			if tt.wantLabel != "" {
				assert.Equal(t, tt.wantLabel, tt.sbx.Labels[identity.AnnotationAgentName])
			} else {
				assert.NotContains(t, tt.sbx.Labels, identity.AnnotationAgentName)
			}
			if tt.checkPodLabels && tt.sbx.Spec.Template != nil {
				if tt.wantPodLabel != "" {
					assert.Equal(t, tt.wantPodLabel, tt.sbx.Spec.Template.Labels[identity.AnnotationAgentName])
				} else {
					assert.NotContains(t, tt.sbx.Spec.Template.Labels, identity.AnnotationAgentName)
				}
			}
			// A rejected value must leave the object exactly as it was, so the
			// annotation is only expected to survive on the error paths.
			if tt.wantAnnotation != "" {
				assert.Equal(t, tt.wantAnnotation, tt.sbx.Annotations[identity.AnnotationAgentName])
			} else {
				assert.NotContains(t, tt.sbx.Annotations, identity.AnnotationAgentName,
					"the annotation is an input channel and must never be persisted")
			}
		})
	}
}

// TestResolveCloneAgentName verifies the precedence a clone applies when
// resolving the expected agent name: the request wins so a caller can rebind or
// drop the identity, and the value persisted with the checkpoint keeps a
// snapshot's binding alive when the request stays silent.
func TestResolveCloneAgentName(t *testing.T) {
	newCheckpoint := func(annotations map[string]string) *v1alpha1.Checkpoint {
		return &v1alpha1.Checkpoint{ObjectMeta: metav1.ObjectMeta{Annotations: annotations}}
	}

	tests := []struct {
		name   string
		opts   infra.CloneSandboxOptions
		cp     *v1alpha1.Checkpoint
		want   string
		reason string
	}{
		{
			name:   "request value wins over the checkpoint",
			opts:   infra.CloneSandboxOptions{AgentName: "request-agent"},
			cp:     newCheckpoint(map[string]string{identity.AnnotationAgentName: "checkpoint-agent"}),
			want:   "request-agent",
			reason: "the caller must be able to rebind the clone to another agent",
		},
		{
			name:   "checkpoint value applies when the request is silent",
			cp:     newCheckpoint(map[string]string{identity.AnnotationAgentName: "checkpoint-agent"}),
			want:   "checkpoint-agent",
			reason: "a snapshot keeps its identity binding without the caller repeating it",
		},
		{
			name:   "no value anywhere resolves to empty",
			cp:     newCheckpoint(nil),
			reason: "an unbound clone must have the inherited pod template label stripped",
		},
		{
			name:   "nil checkpoint resolves to empty",
			reason: "defensive guard: callers must not panic on a missing checkpoint",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, resolveCloneAgentName(tt.opts, tt.cp), tt.reason)
		})
	}
}
