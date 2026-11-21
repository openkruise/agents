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

package sandboxtemplate

import api "github.com/openkruise/agents/api/v1alpha1"

// BuildContainerName names the business container that a template Build
// provisions and captures.
const BuildContainerName = "runtime"

// ExecutionIdentityAnnotation stores the execution identity of the build
// worker on the Version, in the E2B key domain.
const ExecutionIdentityAnnotation = api.E2BPrefix + "template-execution-identity"

func IsBuildVersion(v *api.SandboxTemplate) bool {
	return v != nil && v.Labels[api.LabelTemplateName] != ""
}

// IsSubmitted reports whether the business-container image has been set.
// Submission writes exactly that image, so it is the marker between a
// reserved Draft and a submitted definition. Init containers are injected
// by the runtime and infra layers and may exist on an unsubmitted Draft,
// so checking them would misclassify Drafts as submitted.
func IsSubmitted(v *api.SandboxTemplate) bool {
	if v.Spec.Template != nil {
		for _, container := range v.Spec.Template.Spec.Containers {
			if container.Image != "" {
				return true
			}
		}
	}
	return false
}

func IsStaticReady(v *api.SandboxTemplate) bool {
	return v.Status.Phase == api.SandboxTemplatePhaseReady && v.Status.ObservedGeneration == v.Generation && len(v.Status.Artifacts) == 0
}
