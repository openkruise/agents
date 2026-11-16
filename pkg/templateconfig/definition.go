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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"

	api "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/templatevalidation"
)

// Definition is workload and companion intent, not a persisted CR or transport
// model. Existing policy value types are reused without exposing CR metadata.
type Definition struct {
	Metadata             Metadata
	Pod                  corev1.PodSpec
	VolumeClaimTemplates []corev1.PersistentVolumeClaim
	PersistentContents   []string
	Runtimes             []api.RuntimeConfig
	PauseStrategy        *api.PauseStrategy
	Probes               []api.Probe
	AutoPausePolicy      *api.AutoPausePolicy
	Lifecycle            *api.SandboxLifecycle
	WarmPool             *api.SandboxTemplateWarmPoolOptions
}

// TemplateSpec creates an independent persistence projection.
func (d Definition) TemplateSpec() *api.SandboxTemplateSpec {
	s := &api.SandboxTemplateSpec{Backend: api.SandboxTemplateBackendKubernetes,
		Template: &corev1.PodTemplateSpec{Spec: d.Pod}, VolumeClaimTemplates: d.VolumeClaimTemplates,
		PersistentContents: d.PersistentContents, Runtimes: d.Runtimes, PauseStrategy: d.PauseStrategy,
		Probes: d.Probes, AutoPausePolicy: d.AutoPausePolicy, Lifecycle: d.Lifecycle,
		BuildOptions: &api.SandboxTemplateBuildOptions{WarmPool: d.WarmPool}}
	return s.DeepCopy()
}

// NormalizeDefinition is read-free and does not mutate input.
func NormalizeDefinition(d Definition) (Definition, error) {
	s, errs := templatevalidation.NormalizeCustomSpec(d.TemplateSpec(), field.NewPath("spec"))
	errs = append(errs, ValidateMetadata(d.Metadata, field.NewPath("metadata"))...)
	if len(errs) > 0 {
		return Definition{}, errs.ToAggregate()
	}
	return Definition{Metadata: d.Metadata.Clone(), Pod: s.Template.Spec, VolumeClaimTemplates: s.VolumeClaimTemplates,
		PersistentContents: s.PersistentContents, Runtimes: s.Runtimes, PauseStrategy: s.PauseStrategy,
		Probes: s.Probes, AutoPausePolicy: s.AutoPausePolicy, Lifecycle: s.Lifecycle, WarmPool: s.BuildOptions.WarmPool}, nil
}
