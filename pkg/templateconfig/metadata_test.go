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

package templateconfig

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func TestCustomMetadataValidation(t *testing.T) {
	for _, tt := range []struct {
		name     string
		metadata Metadata
		valid    bool
	}{
		{name: "omitted", valid: true},
		{name: "business and standard app keys", metadata: Metadata{Labels: map[string]string{"app.kubernetes.io/name": "worker", "team.example/project": "", "simple": "v1"}, Annotations: map[string]string{"team.example/description": "自由文本"}}, valid: true},
		{name: "bad label key", metadata: Metadata{Labels: map[string]string{"bad key": "v"}}},
		{name: "bad label value", metadata: Metadata{Labels: map[string]string{"app": "has spaces"}}},
		{name: "long label value", metadata: Metadata{Labels: map[string]string{"app": strings.Repeat("a", 64)}}},
		{name: "bad annotation key", metadata: Metadata{Annotations: map[string]string{"bad key": "v"}}},
		{name: "annotation budget boundary", metadata: Metadata{Annotations: map[string]string{"x": strings.Repeat("a", maxBusinessAnnotationSize-1)}}, valid: true},
		{name: "annotation budget exceeded", metadata: Metadata{Annotations: map[string]string{"x": strings.Repeat("a", maxBusinessAnnotationSize)}}},
		{name: "aggregate annotation budget exceeded", metadata: Metadata{Annotations: map[string]string{"a": strings.Repeat("a", maxBusinessAnnotationSize/2), "b": strings.Repeat("b", maxBusinessAnnotationSize/2-1)}}},
		{name: "multibyte annotation budget exceeded", metadata: Metadata{Annotations: map[string]string{"x": strings.Repeat("界", maxBusinessAnnotationSize/3+1)}}},
		{name: "too large annotations", metadata: Metadata{Annotations: map[string]string{"description": strings.Repeat("a", 256*1024)}}},
		{name: "internal label", metadata: Metadata{Labels: map[string]string{"agents.kruise.io/sandbox-id": "fake"}}},
		{name: "E2B annotation", metadata: Metadata{Annotations: map[string]string{"e2b.agents.kruise.io/secret": "fake"}}},
		{name: "revision hash annotation", metadata: Metadata{Annotations: map[string]string{"sandbox.agents.kruise.io/hash-immutable-part": "fake"}}},
		{name: "case insensitive reserved annotation", metadata: Metadata{Annotations: map[string]string{"AGENTS.KRUISE.IO/secret": "fake"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			errs := ValidateMetadata(tt.metadata, field.NewPath("metadata"))
			if (len(errs) == 0) != tt.valid {
				t.Fatalf("errors=%v", errs)
			}
		})
	}
}

func TestCustomMetadataNormalizationAndIsolation(t *testing.T) {
	source := Metadata{Labels: map[string]string{"project": "analysis"}, Annotations: map[string]string{"description": "worker"}}
	d := Definition{Metadata: source, Pod: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "python:3.12"}}}}
	normalized, err := NormalizeDefinition(d)
	if err != nil {
		t.Fatal(err)
	}
	spec := normalized.TemplateSpec()
	if len(spec.Template.Labels) != 0 || len(spec.Template.Annotations) != 0 {
		t.Fatal("root metadata polluted PodTemplate")
	}
	normalized.Metadata.Labels["project"] = "changed"
	normalized.Metadata.Annotations["description"] = "changed"
	if source.Labels["project"] != "analysis" || source.Annotations["description"] != "worker" {
		t.Fatal("normalization aliased input maps")
	}
	projectionSource := Metadata{Labels: map[string]string{"project": "analysis", "agents.kruise.io/id": "secret"}, Annotations: map[string]string{"description": "worker", "sandbox.agents.kruise.io/hash-immutable-part": "secret"}}
	projected := UserMetadata(projectionSource.Labels, projectionSource.Annotations)
	if len(projected.Labels) != 1 || len(projected.Annotations) != 1 {
		t.Fatalf("projection=%+v", projected)
	}
	projected.Labels["project"] = "other"
	projected.Annotations["description"] = "other"
	if projectionSource.Labels["project"] != "analysis" || projectionSource.Annotations["description"] != "worker" || len(projectionSource.Labels) != 2 || len(projectionSource.Annotations) != 2 {
		t.Fatal("projection mutated source maps")
	}
}
