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

package infra_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
)

func newPodMetadataSandbox(hasTemplate bool, labels, annotations map[string]string) *sandboxcr.Sandbox {
	sbx := &agentsv1alpha1.Sandbox{}
	if hasTemplate {
		sbx.Spec.Template = &corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels:      labels,
				Annotations: annotations,
			},
		}
	}
	return sandboxcr.AsSandbox(sbx, nil)
}

func TestMergePodLabels(t *testing.T) {
	tests := []struct {
		name           string
		noTemplate     bool
		existingLabels map[string]string
		inputLabels    map[string]string
		wantLabels     map[string]string
	}{
		{
			name:        "nil template is a safe no-op",
			noTemplate:  true,
			inputLabels: map[string]string{"app": "sandbox", "env": "prod"},
		},
		{
			name:           "nil existing labels - initializes and sets all",
			existingLabels: nil,
			inputLabels:    map[string]string{"app": "sandbox", "env": "prod"},
			wantLabels:     map[string]string{"app": "sandbox", "env": "prod"},
		},
		{
			name:           "empty existing labels - initializes and sets all",
			existingLabels: map[string]string{},
			inputLabels:    map[string]string{"app": "sandbox"},
			wantLabels:     map[string]string{"app": "sandbox"},
		},
		{
			name:           "empty input labels - no change",
			existingLabels: map[string]string{"app": "sandbox"},
			inputLabels:    map[string]string{},
			wantLabels:     map[string]string{"app": "sandbox"},
		},
		{
			name:           "nil input labels - no change",
			existingLabels: map[string]string{"app": "sandbox"},
			inputLabels:    nil,
			wantLabels:     map[string]string{"app": "sandbox"},
		},
		{
			name:           "overwrite existing label with same key",
			existingLabels: map[string]string{"app": "old", "env": "dev"},
			inputLabels:    map[string]string{"app": "new"},
			wantLabels:     map[string]string{"app": "new", "env": "dev"},
		},
		{
			name:           "add new labels to existing",
			existingLabels: map[string]string{"app": "sandbox"},
			inputLabels:    map[string]string{"env": "prod", "tier": "frontend"},
			wantLabels:     map[string]string{"app": "sandbox", "env": "prod", "tier": "frontend"},
		},
		{
			name:           "both nil - no change",
			existingLabels: nil,
			inputLabels:    nil,
			wantLabels:     nil,
		},
		{
			name:           "both empty maps - no change",
			existingLabels: map[string]string{},
			inputLabels:    map[string]string{},
			wantLabels:     map[string]string{},
		},
		{
			name:           "empty string value - valid label",
			existingLabels: map[string]string{"app": "sandbox"},
			inputLabels:    map[string]string{"note": ""},
			wantLabels:     map[string]string{"app": "sandbox", "note": ""},
		},
		{
			name:           "overwrite all existing labels",
			existingLabels: map[string]string{"app": "old", "env": "dev"},
			inputLabels:    map[string]string{"app": "new", "env": "prod"},
			wantLabels:     map[string]string{"app": "new", "env": "prod"},
		},
		{
			name:           "kubernetes-style dotted label keys",
			existingLabels: map[string]string{"app.kubernetes.io/name": "sandbox"},
			inputLabels:    map[string]string{"app.kubernetes.io/instance": "prod", "app.kubernetes.io/managed-by": "kruise"},
			wantLabels:     map[string]string{"app.kubernetes.io/name": "sandbox", "app.kubernetes.io/instance": "prod", "app.kubernetes.io/managed-by": "kruise"},
		},
		{
			name:           "single label added to multiple existing",
			existingLabels: map[string]string{"app": "sandbox", "env": "prod", "tier": "frontend"},
			inputLabels:    map[string]string{"version": "v1"},
			wantLabels:     map[string]string{"app": "sandbox", "env": "prod", "tier": "frontend", "version": "v1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := newPodMetadataSandbox(!tt.noTemplate, tt.existingLabels, nil)
			assert.NotPanics(t, func() {
				infra.MergePodLabels(sbx, tt.inputLabels)
			})
			if tt.noTemplate {
				assert.Nil(t, sbx.Spec.Template)
			}
			assert.Equal(t, tt.wantLabels, sbx.GetPodLabels())
		})
	}
}

func TestMergePodLabels_Idempotent(t *testing.T) {
	sbx := newPodMetadataSandbox(true, map[string]string{"app": "sandbox"}, nil)
	input := map[string]string{"env": "prod", "tier": "frontend"}
	infra.MergePodLabels(sbx, input)
	infra.MergePodLabels(sbx, input)
	assert.Equal(t, map[string]string{
		"app":  "sandbox",
		"env":  "prod",
		"tier": "frontend",
	}, sbx.GetPodLabels())
}

func TestMergePodAnnotations(t *testing.T) {
	tests := []struct {
		name                string
		noTemplate          bool
		existingAnnotations map[string]string
		inputAnnotations    map[string]string
		wantAnnotations     map[string]string
	}{
		{
			name:             "nil template is a safe no-op",
			noTemplate:       true,
			inputAnnotations: map[string]string{"a": "1", "b": "2"},
		},
		{
			name:                "nil existing annotations - initializes and sets all",
			existingAnnotations: nil,
			inputAnnotations:    map[string]string{"a": "1", "b": "2"},
			wantAnnotations:     map[string]string{"a": "1", "b": "2"},
		},
		{
			name:                "empty input annotations - no change",
			existingAnnotations: map[string]string{"a": "1"},
			inputAnnotations:    map[string]string{},
			wantAnnotations:     map[string]string{"a": "1"},
		},
		{
			name:                "nil input annotations - no change",
			existingAnnotations: map[string]string{"a": "1"},
			inputAnnotations:    nil,
			wantAnnotations:     map[string]string{"a": "1"},
		},
		{
			name:                "overwrite existing annotation with same key",
			existingAnnotations: map[string]string{"a": "old", "b": "keep"},
			inputAnnotations:    map[string]string{"a": "new"},
			wantAnnotations:     map[string]string{"a": "new", "b": "keep"},
		},
		{
			name:                "add new annotations to existing",
			existingAnnotations: map[string]string{"a": "1"},
			inputAnnotations:    map[string]string{"b": "2", "c": "3"},
			wantAnnotations:     map[string]string{"a": "1", "b": "2", "c": "3"},
		},
		{
			name:                "both nil - no change",
			existingAnnotations: nil,
			inputAnnotations:    nil,
			wantAnnotations:     nil,
		},
		{
			name:                "kubernetes-style dotted annotation keys",
			existingAnnotations: map[string]string{"agents.kruise.io/a": "1"},
			inputAnnotations:    map[string]string{"agents.kruise.io/b": "2"},
			wantAnnotations:     map[string]string{"agents.kruise.io/a": "1", "agents.kruise.io/b": "2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := newPodMetadataSandbox(!tt.noTemplate, nil, tt.existingAnnotations)
			assert.NotPanics(t, func() {
				infra.MergePodAnnotations(sbx, tt.inputAnnotations)
			})
			if tt.noTemplate {
				assert.Nil(t, sbx.Spec.Template)
			}
			assert.Equal(t, tt.wantAnnotations, sbx.GetPodAnnotations())
		})
	}
}
