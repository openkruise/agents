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

package validating

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/kubernetes/pkg/apis/core"
	corev1 "k8s.io/kubernetes/pkg/apis/core/v1"
	corevalidation "k8s.io/kubernetes/pkg/apis/core/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/autopause"
	"github.com/openkruise/agents/pkg/sandboxtemplate"
	webhookutils "github.com/openkruise/agents/pkg/webhook/utils"
)

type ValidatingHandler struct {
	Client  client.Client
	Decoder admission.Decoder
}

// +kubebuilder:webhook:path=/validate-sandboxtemplate,mutating=false,failurePolicy=fail,sideEffects=None,admissionReviewVersions=v1;v1beta1,groups=agents.kruise.io,resources=sandboxtemplates;sandboxtemplates/status,verbs=create;update,versions=v1alpha1,name=v-sbt.kb.io

func (h *ValidatingHandler) Path() string {
	return "/validate-sandboxtemplate"
}

func (h *ValidatingHandler) Enabled() bool {
	return true
}

func (h *ValidatingHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	obj := &agentsv1alpha1.SandboxTemplate{}
	if err := h.Decoder.Decode(req, obj); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	var errs field.ErrorList
	if req.Operation == admissionv1.Update {
		old := &agentsv1alpha1.SandboxTemplate{}
		if err := h.Decoder.DecodeRaw(req.OldObject, old); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		errs = validateManagedUpdate(old, obj)
		if len(errs) > 0 {
			return admission.Errored(http.StatusUnprocessableEntity, errs.ToAggregate())
		}
		if !sandboxtemplate.IsBuildVersion(old) || req.SubResource == "status" || sandboxtemplate.IsSubmitted(old) || equality.Semantic.DeepEqual(old.Spec, obj.Spec) {
			return admission.Allowed("")
		}
	}
	errs = append(errs, validateSandboxTemplateMetadata(obj.ObjectMeta, field.NewPath("metadata"))...)
	if sandboxtemplate.IsBuildVersion(obj) && !sandboxtemplate.IsSubmitted(obj) {
		errs = append(errs, validateDraftSpec(obj.Spec)...)
	} else {
		errs = append(errs, validateSandboxTemplateSpec(obj.Spec, field.NewPath("spec"))...)
		if sandboxtemplate.IsBuildVersion(obj) {
			errs = append(errs, validateBuildOptions(obj.Spec)...)
		}
	}
	if len(errs) > 0 {
		return admission.Errored(http.StatusUnprocessableEntity, errs.ToAggregate())
	}
	if req.Operation == admissionv1.Create && sandboxtemplate.IsBuildVersion(obj) {
		key := client.ObjectKey{Namespace: obj.Namespace, Name: obj.Labels[agentsv1alpha1.LabelTemplateName]}
		for _, resource := range []struct {
			kind   string
			object client.Object
		}{{"SandboxSet", &agentsv1alpha1.SandboxSet{}}, {"Checkpoint", &agentsv1alpha1.Checkpoint{}}} {
			if err := h.Client.Get(ctx, key, resource.object); err == nil {
				return admission.Errored(http.StatusConflict, fmt.Errorf("template name %q is occupied by %s", key.Name, resource.kind))
			} else if !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
				return admission.Errored(http.StatusServiceUnavailable, fmt.Errorf("check template name: %w", err))
			}
		}
	}
	return admission.Allowed("")
}

func validateManagedUpdate(old, obj *agentsv1alpha1.SandboxTemplate) field.ErrorList {
	oldName, oldBuild := old.Labels[agentsv1alpha1.LabelTemplateName]
	newName, newBuild := obj.Labels[agentsv1alpha1.LabelTemplateName]
	if !oldBuild && !newBuild {
		return nil
	}
	if !oldBuild || !newBuild || oldName != newName {
		return field.ErrorList{field.Forbidden(field.NewPath("metadata", "labels").Key(agentsv1alpha1.LabelTemplateName), "template name is immutable; existing templates cannot be adopted")}
	}
	var errs field.ErrorList
	complete := sandboxtemplate.IsSubmitted(old)
	if !equality.Semantic.DeepEqual(old.Spec, obj.Spec) {
		switch {
		case complete || old.DeletionTimestamp != nil:
			errs = append(errs, field.Forbidden(field.NewPath("spec"), "finalized or deleting template spec is immutable"))
		case !sandboxtemplate.IsSubmitted(obj):
			errs = append(errs, field.Forbidden(field.NewPath("spec"), "Draft spec can change only during finalization"))
		case !equality.Semantic.DeepEqual(containerResources(old.Spec.Template), containerResources(obj.Spec.Template)):
			errs = append(errs, field.Forbidden(field.NewPath("spec", "template", "spec", "containers"), "reserved resource envelope is immutable"))
		case !equality.Semantic.DeepEqual(old.Spec.Runtimes, obj.Spec.Runtimes):
			errs = append(errs, field.Forbidden(field.NewPath("spec", "runtimes"), "reserved runtimes are immutable"))
		}
	}
	if len(old.Status.Artifacts) > 0 && !equality.Semantic.DeepEqual(old.Status.Artifacts, obj.Status.Artifacts) {
		errs = append(errs, field.Forbidden(field.NewPath("status", "artifacts"), "artifact references are immutable"))
	}
	return errs
}

func containerResources(template *v1.PodTemplateSpec) map[string]v1.ResourceRequirements {
	resources := make(map[string]v1.ResourceRequirements)
	if template != nil {
		for _, container := range template.Spec.Containers {
			resources[container.Name] = container.Resources
		}
	}
	return resources
}

func validateDraftSpec(spec agentsv1alpha1.SandboxTemplateSpec) field.ErrorList {
	p := field.NewPath("spec")
	if spec.Template == nil {
		return field.ErrorList{field.Required(p.Child("template"), "template is required")}
	}
	var errs field.ErrorList
	if spec.BuildOptions != nil || len(spec.PersistentContents) > 0 || len(spec.VolumeClaimTemplates) > 0 || spec.PauseStrategy != nil || len(spec.Probes) > 0 || spec.AutoPausePolicy != nil || spec.Lifecycle != nil || spec.StartupMode != "" && spec.StartupMode != agentsv1alpha1.StartupModeImmediate {
		errs = append(errs, field.Forbidden(p, "Draft can contain only reserved resources and runtimes"))
	}
	validationSpec := spec
	validationSpec.Template = spec.Template.DeepCopy()
	for i, container := range spec.Template.Spec.Containers {
		containerPath := p.Child("template", "spec", "containers").Index(i)
		if container.Image != "" || len(container.Command) > 0 || len(container.Args) > 0 || len(container.Env) > 0 {
			errs = append(errs, field.Forbidden(containerPath, "Draft cannot contain build input"))
		}
		for _, name := range []v1.ResourceName{v1.ResourceCPU, v1.ResourceMemory} {
			request, limit := container.Resources.Requests[name], container.Resources.Limits[name]
			if request.Sign() <= 0 || limit.Sign() <= 0 || !request.Equal(limit) {
				errs = append(errs, field.Invalid(containerPath.Child("resources"), container.Resources, "CPU and memory requests and limits must be positive and equal"))
				break
			}
		}
		// Draft omits images, but the remaining Pod template must still be valid.
		validationSpec.Template.Spec.Containers[i].Image = "draft"
	}
	return append(errs, validateSandboxTemplateSpec(validationSpec, p)...)
}

func validateBuildOptions(spec agentsv1alpha1.SandboxTemplateSpec) field.ErrorList {
	options := spec.BuildOptions
	if options == nil {
		return nil
	}
	p := field.NewPath("spec", "buildOptions")
	var errs field.ErrorList
	if options.StartCommand != nil {
		errs = append(errs, field.Forbidden(p.Child("startCommand"), "not supported"))
	}
	if options.ReadyCommand != nil {
		errs = append(errs, field.Forbidden(p.Child("readyCommand"), "not supported"))
	}
	if options.FromTemplate != "" {
		errs = append(errs, field.Forbidden(p.Child("fromTemplate"), "not supported"))
	}
	if len(options.Steps) > 0 && !options.Checkpoint {
		errs = append(errs, field.Forbidden(p.Child("checkpoint"), "steps require artifact capture"))
	}
	if options.Checkpoint {
		if len(spec.PersistentContents) == 0 {
			errs = append(errs, field.Required(field.NewPath("spec", "persistentContents"), "artifact capture requires a non-empty scope"))
		}
		for i, content := range spec.PersistentContents {
			if content != agentsv1alpha1.PersistentContentFilesystem && content != agentsv1alpha1.PersistentContentMemory {
				errs = append(errs, field.NotSupported(field.NewPath("spec", "persistentContents").Index(i), content, []string{agentsv1alpha1.PersistentContentFilesystem, agentsv1alpha1.PersistentContentMemory}))
			}
		}
	}
	for i, step := range options.Steps {
		stepPath := p.Child("steps").Index(i)
		if (step.Command == nil) == (step.Copy == nil) {
			errs = append(errs, field.Invalid(stepPath, nil, "exactly one of command or copy is required"))
		}
		if command := step.Command; command != nil {
			if strings.TrimSpace(command.Command) == "" {
				errs = append(errs, field.Required(stepPath.Child("command", "command"), "command is required"))
			}
			if command.Cwd != "" && !path.IsAbs(command.Cwd) {
				errs = append(errs, field.Invalid(stepPath.Child("command", "cwd"), command.Cwd, "must be an absolute path"))
			}
		}
		if copy := step.Copy; copy != nil {
			if copy.Src == "" || path.IsAbs(copy.Src) || path.Clean(copy.Src) == ".." || strings.HasPrefix(path.Clean(copy.Src), "../") {
				errs = append(errs, field.Invalid(stepPath.Child("copy", "src"), copy.Src, "must remain inside the build context"))
			}
			if !path.IsAbs(copy.Dest) {
				errs = append(errs, field.Invalid(stepPath.Child("copy", "dest"), copy.Dest, "must be an absolute path"))
			}
			if copy.FilesHash == "" {
				errs = append(errs, field.Required(stepPath.Child("copy", "filesHash"), "filesHash is required"))
			}
		}
	}
	return errs
}

func validateSandboxTemplateMetadata(metadata metav1.ObjectMeta, fldPath *field.Path) field.ErrorList {
	var errList field.ErrorList
	errList = append(errList, validation.ValidateObjectMeta(&metadata, true, validation.NameIsDNSSubdomain, fldPath)...)
	if name, build := metadata.Labels[agentsv1alpha1.LabelTemplateName]; build {
		namePath := fldPath.Child("labels").Key(agentsv1alpha1.LabelTemplateName)
		for _, message := range utilvalidation.IsDNS1123Label(name) {
			errList = append(errList, field.Invalid(namePath, name, message))
		}
		if name == "tags" {
			errList = append(errList, field.Invalid(namePath, name, "name is reserved"))
		}
	}
	return errList
}

func validateLabelsAndAnnotations(metadata metav1.ObjectMeta, fldPath *field.Path) field.ErrorList {
	var errList field.ErrorList
	labelFld := fldPath.Child("labels")
	for k := range metadata.Labels {
		if strings.HasPrefix(k, agentsv1alpha1.E2BPrefix) {
			errList = append(errList, field.Invalid(labelFld.Key(k), k, "label cannot start with "+agentsv1alpha1.E2BPrefix))
		}
	}
	annoFld := fldPath.Child("annotations")
	for k := range metadata.Annotations {
		if strings.HasPrefix(k, agentsv1alpha1.E2BPrefix) {
			errList = append(errList, field.Invalid(annoFld.Key(k), k, "annotation cannot start with "+agentsv1alpha1.E2BPrefix))
		}
	}
	return errList
}

func validateSandboxTemplateSpec(spec agentsv1alpha1.SandboxTemplateSpec, fldPath *field.Path) field.ErrorList {
	var errList field.ErrorList
	errList = append(errList, autopause.ValidateProbes(spec.Probes, fldPath.Child("probes"))...)
	errList = append(errList, autopause.ValidateAutoPausePolicy(spec.AutoPausePolicy, spec.Probes, fldPath.Child("autoPausePolicy"))...)
	if spec.Template == nil {
		return append(errList, field.Required(fldPath.Child("template"), "template is required"))
	}
	errList = append(errList, validateLabelsAndAnnotations(spec.Template.ObjectMeta, fldPath.Child("template"))...)
	errList = append(errList, validateSandboxPodTemplateSpec(spec, fldPath)...)
	return errList
}

func validateSandboxPodTemplateSpec(spec agentsv1alpha1.SandboxTemplateSpec, fldPath *field.Path) field.ErrorList {
	errList := field.ErrorList{}
	template := spec.Template.DeepCopy()
	coreTemplate := &core.PodTemplateSpec{}
	if len(spec.VolumeClaimTemplates) != 0 {
		errList = append(errList, webhookutils.ValidateVolumeClaimTemplateMounts(spec.Template, spec.VolumeClaimTemplates, fldPath)...)
		webhookutils.AppendVolumeClaimTemplateVolumes(template, spec.VolumeClaimTemplates)
	}
	if err := corev1.Convert_v1_PodTemplateSpec_To_core_PodTemplateSpec(template, coreTemplate, nil); err != nil {
		errList = append(errList, field.Invalid(fldPath.Child("template"), spec.Template, fmt.Sprintf("Convert_v1_PodTemplateSpec_To_core_PodTemplateSpec failed: %v", err)))
		return errList
	}
	errList = append(errList, corevalidation.ValidatePodTemplateSpec(coreTemplate, fldPath.Child("template"), webhookutils.DefaultPodValidationOptions)...)
	return errList
}
