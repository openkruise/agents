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

// Package templateconfig resolves effective workload data without naming gates,
// controllers, API servers or Manager dependencies.
package templateconfig

import (
	"fmt"

	api "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/templateindex"
)

// ResolveTemplateSpec returns independent effective workload data for both hash
// calculation and materialization. ownerTemplate is the resolved control owner;
// it can be nil when there is no Template controller or the reference itself is
// that owner. This function performs no reads and never changes informer data.
func ResolveTemplateSpec(sbs *api.SandboxSet, reference, ownerTemplate *api.SandboxTemplate) (*api.SandboxTemplateSpec, error) {
	if sbs == nil {
		return nil, fmt.Errorf("sandboxset is required")
	}
	controller := templateindex.VersionOwnerReference(sbs)
	if controller != nil {
		if ownerTemplate == nil && reference != nil && reference.Name == controller.Name {
			ownerTemplate = reference
		}
		if ownerTemplate == nil {
			return nil, fmt.Errorf("cannot resolve sandboxset template controller %s/%s", sbs.Namespace, controller.Name)
		}
		if ownerTemplate.UID != controller.UID || ownerTemplate.Name != controller.Name || ownerTemplate.Namespace != sbs.Namespace {
			return nil, fmt.Errorf("sandboxset template controller identity does not match the actual owner")
		}
		if templateindex.ClaimOwnerReference(ownerTemplate) != nil {
			ref := sbs.Spec.TemplateRef
			if sbs.Spec.Template != nil {
				return nil, fmt.Errorf("managed companion must not contain an inline pod template")
			}
			if sbs.Name != ownerTemplate.Name || ref == nil || ref.Name != ownerTemplate.Name || ref.Kind == nil || *ref.Kind != "SandboxTemplate" || ref.APIVersion == nil || *ref.APIVersion != api.GroupVersion.String() {
				return nil, fmt.Errorf("managed companion name and explicit templateRef must identify its owner version")
			}
			if reference == nil || reference.UID != ownerTemplate.UID || reference.Name != ownerTemplate.Name || reference.Namespace != sbs.Namespace {
				return nil, fmt.Errorf("managed companion reference does not identify the actual owner version")
			}
			if ownerTemplate.Spec.Template == nil {
				return nil, fmt.Errorf("managed template version has no pod template")
			}
			effective := ownerTemplate.Spec.DeepCopy()
			effective.Backend = ""
			effective.BuildOptions = nil
			return effective, nil
		}
	}
	source := sbs.DeepCopy()
	template := source.Spec.Template
	if template == nil && reference != nil && reference.Spec.Template != nil {
		template = reference.Spec.Template.DeepCopy()
	}
	return &api.SandboxTemplateSpec{
		Template:             template,
		VolumeClaimTemplates: source.Spec.VolumeClaimTemplates,
		PersistentContents:   source.Spec.PersistentContents,
		Runtimes:             source.Spec.Runtimes,
		PauseStrategy:        source.Spec.PauseStrategy,
		Probes:               source.Spec.Probes,
		AutoPausePolicy:      source.Spec.AutoPausePolicy,
		Lifecycle:            source.Spec.Lifecycle,
	}, nil
}
