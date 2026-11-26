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

package sessionservice

import (
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	agents "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/autopause"
	webhookutils "github.com/openkruise/agents/pkg/webhook/utils"
)

// validateTemplate checks the creation fields supported by SessionService.
func validateTemplate(template *agents.SandboxTemplateSpec) field.ErrorList {
	path := field.NewPath("spec")
	var errs field.ErrorList
	if template.Template == nil {
		return field.ErrorList{field.Required(path.Child("template"), "template is required")}
	}
	if template.Backend != "" && template.Backend != "k8s" {
		errs = append(errs, field.NotSupported(path.Child("backend"), template.Backend, []string{"k8s"}))
	}
	if template.BuildOptions != nil {
		errs = append(errs, field.Forbidden(path.Child("buildOptions"), "buildOptions is not supported by SessionService"))
	}
	errs = append(errs, autopause.ValidateProbes(template.Probes, path.Child("probes"))...)
	errs = append(errs, autopause.ValidateAutoPausePolicy(template.AutoPausePolicy, template.Probes, path.Child("autoPausePolicy"))...)
	names := make(map[string]struct{}, len(template.VolumeClaimTemplates))
	for i := range template.VolumeClaimTemplates {
		name := template.VolumeClaimTemplates[i].Name
		namePath := path.Child("volumeClaimTemplates").Index(i).Child("metadata", "name")
		// Claim template names also become Pod volume and volumeMount names.
		for _, message := range utilvalidation.IsDNS1123Label(name) {
			errs = append(errs, field.Invalid(namePath, name, message))
		}
		if _, exists := names[name]; exists {
			errs = append(errs, field.Duplicate(namePath, name))
		}
		names[name] = struct{}{}
	}
	errs = append(errs, webhookutils.ValidateVolumeClaimTemplateMounts(template.Template, template.VolumeClaimTemplates, path)...)
	if lifecycle := template.Lifecycle; lifecycle != nil {
		lifecyclePath := path.Child("lifecycle")
		if lifecycle.PreUpgrade != nil || lifecycle.PostUpgrade != nil {
			errs = append(errs, field.Forbidden(lifecyclePath, "preUpgrade and postUpgrade are not allowed on SandboxTemplate"))
		}
	}
	return errs
}
