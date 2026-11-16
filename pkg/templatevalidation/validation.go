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

package templatevalidation

import (
	"fmt"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/kubernetes/pkg/apis/core"
	nativev1 "k8s.io/kubernetes/pkg/apis/core/v1"
	corevalidation "k8s.io/kubernetes/pkg/apis/core/validation"

	api "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/autopause"
	"github.com/openkruise/agents/pkg/poolautoscalervalidation"
	"github.com/openkruise/agents/pkg/templateindex"
)

var namePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
var idPattern = regexp.MustCompile(`^[a-z0-9]{20}$`)

// ValidateName validates a logical name, not a version ID or team UUID.
func ValidateName(name string, path *field.Path) field.ErrorList {
	if !namePattern.MatchString(name) {
		return field.ErrorList{field.Invalid(path, name, "must contain 1-63 lowercase letters, digits or hyphens with alphanumeric ends")}
	}
	return nil
}

// ValidateID validates the concrete random identifier without allocating it.
func ValidateID(id string, path *field.Path) field.ErrorList {
	if !idPattern.MatchString(id) {
		return field.ErrorList{field.Invalid(path, id, "must contain exactly 20 lowercase letters or digits")}
	}
	return nil
}

// PodValidationOptions is the single source of existing PodTemplate options.
func PodValidationOptions() corevalidation.PodValidationOptions {
	return corevalidation.PodValidationOptions{
		AllowInvalidPodDeletionCost:                       true,
		AllowIndivisibleHugePagesValues:                   true,
		AllowInvalidLabelValueInSelector:                  true,
		AllowInvalidTopologySpreadConstraintLabelSelector: true,
		ResourceIsPod: false,
	}
}

// ValidateSpec preserves the existing create-only native template checks.
func ValidateSpec(spec api.SandboxTemplateSpec, path *field.Path) field.ErrorList {
	errs := autopause.ValidateProbes(spec.Probes, path.Child("probes"))
	errs = append(errs, autopause.ValidateAutoPausePolicy(spec.AutoPausePolicy, spec.Probes, path.Child("autoPausePolicy"))...)
	if spec.Template == nil {
		return append(errs, field.Required(path.Child("template"), "template is required"))
	}
	for key := range spec.Template.Labels {
		if strings.HasPrefix(key, api.E2BPrefix) {
			errs = append(errs, field.Invalid(path.Child("template", "labels").Key(key), key, "label cannot start with "+api.E2BPrefix))
		}
	}
	for key := range spec.Template.Annotations {
		if strings.HasPrefix(key, api.E2BPrefix) {
			errs = append(errs, field.Invalid(path.Child("template", "annotations").Key(key), key, "annotation cannot start with "+api.E2BPrefix))
		}
	}
	template := spec.Template.DeepCopy()
	if len(spec.VolumeClaimTemplates) != 0 {
		errs = append(errs, ValidateVolumeClaimTemplateMounts(spec.Template, spec.VolumeClaimTemplates, path)...)
		AppendVolumeClaimTemplateVolumes(template, spec.VolumeClaimTemplates)
	}
	var native core.PodTemplateSpec
	if err := nativev1.Convert_v1_PodTemplateSpec_To_core_PodTemplateSpec(template, &native, nil); err != nil {
		return append(errs, field.Invalid(path.Child("template"), spec.Template, fmt.Sprintf("Convert_v1_PodTemplateSpec_To_core_PodTemplateSpec failed: %v", err)))
	}
	return append(errs, corevalidation.ValidatePodTemplateSpec(&native, path.Child("template"), PodValidationOptions())...)
}

// ValidateBuildOptions checks the new declaration without admitting old build
// commands to the custom API. Native persistence still retains those fields.
func ValidateBuildOptions(options *api.SandboxTemplateBuildOptions, path *field.Path) field.ErrorList {
	if options == nil {
		return nil
	}
	var errs field.ErrorList
	if options.FromTemplate != "" {
		errs = append(errs, field.Forbidden(path.Child("fromTemplate"), "not supported by custom template input"))
	}
	if len(options.Steps) != 0 {
		errs = append(errs, field.Forbidden(path.Child("steps"), "not supported by custom template input"))
	}
	if options.StartCommand != nil {
		errs = append(errs, field.Forbidden(path.Child("startCommand"), "not supported by custom template input"))
	}
	if options.ReadyCommand != nil {
		errs = append(errs, field.Forbidden(path.Child("readyCommand"), "not supported by custom template input"))
	}
	warm := options.WarmPool
	if warm == nil {
		return errs
	}
	warmPath := path.Child("warmPool")
	if warm.Size != nil && *warm.Size < 0 {
		errs = append(errs, field.Invalid(warmPath.Child("size"), *warm.Size, "must be nonnegative"))
	}
	if warm.Enabled != nil && !*warm.Enabled {
		if warm.Size != nil {
			errs = append(errs, field.Forbidden(warmPath.Child("size"), "must be omitted when disabled, including explicit zero"))
		}
		if warm.Autoscaler != nil {
			errs = append(errs, field.Forbidden(warmPath.Child("autoscaler"), "must be omitted when disabled"))
		}
		return errs
	}
	if auto := warm.Autoscaler; auto != nil {
		errs = append(errs, poolautoscalervalidation.ValidatePolicies(auto.MinReplicas, auto.MaxReplicas, auto.CapacityPolicy, auto.CronPolicies, warmPath.Child("autoscaler"))...)
		if warm.Size != nil && *warm.Size > auto.MaxReplicas {
			errs = append(errs, field.Invalid(warmPath.Child("size"), *warm.Size, "must not exceed autoscaler maxReplicas"))
		}
	}
	return errs
}

// ValidateCustomSpec checks only Claim-build definition data. It performs no
// dependency, image-registry, scheduling, live read or command execution checks.
func ValidateCustomSpec(spec api.SandboxTemplateSpec, path *field.Path) field.ErrorList {
	errs := ValidateSpec(spec, path)
	if spec.Backend != "" && spec.Backend != api.SandboxTemplateBackendKubernetes {
		errs = append(errs, field.NotSupported(path.Child("backend"), spec.Backend, []string{api.SandboxTemplateBackendKubernetes}))
	}
	errs = append(errs, ValidateBuildOptions(spec.BuildOptions, path.Child("buildOptions"))...)
	errs = append(errs, validateCustomVolumeClaimTemplates(spec.VolumeClaimTemplates, path.Child("volumeClaimTemplates"))...)
	errs = append(errs, validateCustomRuntimes(spec.Runtimes, path.Child("runtimes"))...)
	errs = append(errs, validateCustomPersistentContents(spec.PersistentContents, path.Child("persistentContents"))...)
	errs = append(errs, validateCustomPauseStrategy(spec.PauseStrategy, path.Child("pauseStrategy"))...)
	errs = append(errs, validateCustomProbes(spec, path.Child("probes"))...)
	errs = append(errs, validateCustomLifecycle(spec.Lifecycle, path.Child("lifecycle"))...)
	return errs
}

func validateCustomVolumeClaimTemplates(claims []corev1.PersistentVolumeClaim, claimsPath *field.Path) field.ErrorList {
	var errs field.ErrorList
	seenClaims := map[string]bool{}
	for i, claim := range claims {
		claimPath := claimsPath.Index(i)
		for _, message := range utilvalidation.IsDNS1123Label(claim.Name) {
			errs = append(errs, field.Invalid(claimPath.Child("metadata", "name"), claim.Name, message))
		}
		if seenClaims[claim.Name] {
			errs = append(errs, field.Duplicate(claimPath.Child("metadata", "name"), claim.Name))
		}
		seenClaims[claim.Name] = true
		metadata := claim.ObjectMeta.DeepCopy()
		metadata.Name = ""
		if !equality.Semantic.DeepEqual(*metadata, metav1.ObjectMeta{}) {
			errs = append(errs, field.Forbidden(claimPath.Child("metadata"), "only name is supported; labels, annotations and system metadata are not copied"))
		}
		if claim.Kind != "" || claim.APIVersion != "" {
			errs = append(errs, field.Forbidden(claimPath, "PVC type metadata is not accepted"))
		}
		if !equality.Semantic.DeepEqual(claim.Status, corev1.PersistentVolumeClaimStatus{}) {
			errs = append(errs, field.Forbidden(claimPath.Child("status"), "PVC status is not accepted"))
		}
		var native core.PersistentVolumeClaim
		if err := nativev1.Convert_v1_PersistentVolumeClaim_To_core_PersistentVolumeClaim(&claim, &native, nil); err != nil {
			errs = append(errs, field.InternalError(claimPath, err))
		} else {
			errs = append(errs, corevalidation.ValidatePersistentVolumeClaimSpec(&native.Spec, claimPath.Child("spec"), corevalidation.PersistentVolumeClaimSpecValidationOptions{})...)
		}
	}
	return errs
}

func validateCustomRuntimes(runtimes []api.RuntimeConfig, runtimesPath *field.Path) field.ErrorList {
	var errs field.ErrorList
	supportedRuntimes := []string{api.RuntimeConfigForInjectCsiMount, api.RuntimeConfigForInjectAgentRuntime, api.RuntimeConfigForInjectTrafficProxy}
	seenRuntime := map[string]bool{}
	for i, runtime := range runtimes {
		runtimePath := runtimesPath.Index(i).Child("name")
		if runtime.Name != supportedRuntimes[0] && runtime.Name != supportedRuntimes[1] && runtime.Name != supportedRuntimes[2] {
			errs = append(errs, field.NotSupported(runtimePath, runtime.Name, supportedRuntimes))
		}
		if seenRuntime[runtime.Name] {
			errs = append(errs, field.Duplicate(runtimePath, runtime.Name))
		}
		seenRuntime[runtime.Name] = true
	}
	return errs
}

func validateCustomPersistentContents(contents []string, contentsPath *field.Path) field.ErrorList {
	var errs field.ErrorList
	supported := []string{api.PersistentContentIp, api.PersistentContentMemory, api.PersistentContentFilesystem}
	for i, value := range contents {
		if value != api.PersistentContentIp && value != api.PersistentContentMemory && value != api.PersistentContentFilesystem {
			errs = append(errs, field.NotSupported(contentsPath.Index(i), value, supported))
		}
	}
	return errs
}

func validateCustomPauseStrategy(pause *api.PauseStrategy, pausePath *field.Path) field.ErrorList {
	if pause == nil {
		return nil
	}
	var errs field.ErrorList
	if pause.Type != "" && pause.Type != api.PauseStrategyStop && pause.Type != api.PauseStrategyHibernate {
		errs = append(errs, field.NotSupported(pausePath.Child("type"), pause.Type, []string{string(api.PauseStrategyStop), string(api.PauseStrategyHibernate)}))
	}
	if h := pause.HibernateStrategy; h != nil && h.Type != "" && h.Type != api.HibernateStrategySnapshot {
		errs = append(errs, field.NotSupported(pausePath.Child("hibernateStrategy", "type"), h.Type, []string{string(api.HibernateStrategySnapshot)}))
	}
	return errs
}

func validateCustomProbes(spec api.SandboxTemplateSpec, probesPath *field.Path) field.ErrorList {
	containers := map[string]bool{}
	if spec.Template != nil {
		for _, container := range spec.Template.Spec.Containers {
			containers[container.Name] = true
		}
	}
	var errs field.ErrorList
	for i, probe := range spec.Probes {
		if probe.ContainerName != "" && !containers[probe.ContainerName] {
			errs = append(errs, field.Invalid(probesPath.Index(i).Child("containerName"), probe.ContainerName, "must refer to a business container"))
		}
	}
	return errs
}

func validateCustomLifecycle(lifecycle *api.SandboxLifecycle, lifecyclePath *field.Path) field.ErrorList {
	if lifecycle == nil {
		return nil
	}
	var errs field.ErrorList
	if lifecycle.PreUpgrade != nil {
		errs = append(errs, field.Forbidden(lifecyclePath.Child("preUpgrade"), "not supported by template definitions"))
	}
	if lifecycle.PostUpgrade != nil {
		errs = append(errs, field.Forbidden(lifecyclePath.Child("postUpgrade"), "not supported by template definitions"))
	}
	for _, hook := range []struct {
		name   string
		action *api.LifecycleAction
	}{{"prePause", lifecycle.PrePause}, {"postResume", lifecycle.PostResume}} {
		if hook.action == nil {
			continue
		}
		actionPath := lifecyclePath.Child(hook.name)
		if hook.action.Exec == nil || len(hook.action.Exec.Command) == 0 {
			errs = append(errs, field.Required(actionPath.Child("exec", "command"), "command is required"))
		}
		if hook.action.TimeoutSeconds != 0 && (hook.action.TimeoutSeconds < 1 || hook.action.TimeoutSeconds > 60) {
			errs = append(errs, field.Invalid(actionPath.Child("timeoutSeconds"), hook.action.TimeoutSeconds, "must be between 1 and 60"))
		}
	}
	return errs
}

// ValidateManagedUpdate scopes immutability to Claim-owned versions only; native
// legacy updates remain outside the new custom contract. Owner removal must not
// provide a two-step escape from immutability, and existing objects are not adopted.
func ValidateManagedUpdate(oldTemplate, newTemplate *api.SandboxTemplate, path *field.Path) field.ErrorList {
	oldOwner := templateindex.ClaimOwnerReference(oldTemplate)
	newOwner := templateindex.ClaimOwnerReference(newTemplate)
	if oldOwner == nil && newOwner == nil {
		return nil
	}
	if oldOwner == nil {
		return field.ErrorList{field.Forbidden(field.NewPath("metadata", "ownerReferences"), "existing versions cannot be adopted into a Claim through update")}
	}
	var errs field.ErrorList
	if !equality.Semantic.DeepEqual(oldOwner, newOwner) {
		errs = append(errs, field.Forbidden(field.NewPath("metadata", "ownerReferences"), "Claim controller ownership is immutable"))
	}
	if !equality.Semantic.DeepEqual(oldTemplate.Spec, newTemplate.Spec) {
		errs = append(errs, field.Forbidden(path, "managed template spec and build options are immutable"))
	}
	return errs
}
