/*
Copyright 2025.

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
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	intstrutil "k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// SandboxUpdateOpsValidatingHandler handles validation for SandboxUpdateOps resources.
type SandboxUpdateOpsValidatingHandler struct {
	Client  client.Client
	Decoder admission.Decoder
}

// +kubebuilder:webhook:path=/validate-sandboxupdateops,mutating=false,failurePolicy=fail,sideEffects=None,admissionReviewVersions=v1;v1beta1,groups=agents.kruise.io,resources=sandboxupdateops,verbs=create;update,versions=v1alpha1,name=v-suo.kb.io

func (h *SandboxUpdateOpsValidatingHandler) Path() string {
	return "/validate-sandboxupdateops"
}

func (h *SandboxUpdateOpsValidatingHandler) Enabled() bool {
	return true
}

func (h *SandboxUpdateOpsValidatingHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	obj := &agentsv1alpha1.SandboxUpdateOps{}
	if err := h.Decoder.Decode(req, obj); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	switch req.Operation {
	case admissionv1.Create:
		return h.handleCreate(ctx, obj)
	case admissionv1.Update:
		return h.handleUpdate(req, obj)
	default:
		return admission.Allowed("")
	}
}

func (h *SandboxUpdateOpsValidatingHandler) handleCreate(ctx context.Context, obj *agentsv1alpha1.SandboxUpdateOps) admission.Response {
	var errList field.ErrorList
	specPath := field.NewPath("spec")

	// 1. Validate Selector is non-empty and valid
	if obj.Spec.Selector == nil {
		errList = append(errList, field.Required(specPath.Child("selector"), "selector is required"))
	} else {
		if _, err := metav1.LabelSelectorAsSelector(obj.Spec.Selector); err != nil {
			errList = append(errList, field.Invalid(specPath.Child("selector"), obj.Spec.Selector, err.Error()))
		}
	}

	// 2. Validate MaxUnavailable if specified
	if obj.Spec.UpdateStrategy.MaxUnavailable != nil {
		if _, err := intstrutil.GetScaledValueFromIntOrPercent(
			intstrutil.ValueOrDefault(obj.Spec.UpdateStrategy.MaxUnavailable, intstrutil.FromInt(0)), 100, true); err != nil {
			errList = append(errList, field.Invalid(specPath.Child("updateStrategy", "maxUnavailable"), obj.Spec.UpdateStrategy.MaxUnavailable, "maxUnavailable is invalid"))
		}
	}

	// 3. Validate Lifecycle configuration
	if obj.Spec.Lifecycle != nil {
		lifecyclePath := specPath.Child("lifecycle")
		if obj.Spec.Lifecycle.PrePause != nil {
			errList = append(errList, field.Forbidden(lifecyclePath.Child("prePause"), "prePause is not configurable through SandboxUpdateOps"))
		}
		if obj.Spec.Lifecycle.PostResume != nil {
			errList = append(errList, field.Forbidden(lifecyclePath.Child("postResume"), "postResume is not configurable through SandboxUpdateOps"))
		}
		if obj.Spec.Lifecycle.PreUpgrade != nil && obj.Spec.Lifecycle.PreUpgrade.Exec == nil {
			errList = append(errList, field.Required(lifecyclePath.Child("preUpgrade", "exec"), "exec is required when preUpgrade is specified"))
		}
		if obj.Spec.Lifecycle.PostUpgrade != nil && obj.Spec.Lifecycle.PostUpgrade.Exec == nil {
			errList = append(errList, field.Required(lifecyclePath.Child("postUpgrade", "exec"), "exec is required when postUpgrade is specified"))
		}
	}

	// 4. Validate the update strategy: installer presence and the
	// no-image-change rule for strategies that preserve the writable layer.
	errList = append(errList, h.validateUpdateStrategy(obj, specPath)...)

	// 5. Check for active (non-terminal) SandboxUpdateOps in the same namespace
	opsList := &agentsv1alpha1.SandboxUpdateOpsList{}
	if err := h.Client.List(ctx, opsList, client.InNamespace(obj.Namespace)); err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	for i := range opsList.Items {
		existing := &opsList.Items[i]
		if existing.Name == obj.Name {
			continue
		}
		if existing.Status.Phase != agentsv1alpha1.SandboxUpdateOpsCompleted &&
			existing.Status.Phase != agentsv1alpha1.SandboxUpdateOpsFailed {
			errList = append(errList, field.Forbidden(specPath, "there is an active SandboxUpdateOps in the same namespace: "+existing.Name))
			break
		}
	}

	if len(errList) > 0 {
		return admission.Errored(http.StatusUnprocessableEntity, errList.ToAggregate())
	}
	return admission.Allowed("")
}

// validateUpdateStrategy validates the installer configuration and the
// no-image-change rule shared by the writable-layer-preserving strategies.
func (h *SandboxUpdateOpsValidatingHandler) validateUpdateStrategy(obj *agentsv1alpha1.SandboxUpdateOps, specPath *field.Path) field.ErrorList {
	var errList field.ErrorList
	strategy := obj.Spec.UpdateStrategy
	strategyPath := specPath.Child("updateStrategy")

	if strategy.Type == agentsv1alpha1.SandboxUpdateOpsStrategyInstallerUpdate {
		if strategy.Installer == nil {
			errList = append(errList, field.Required(strategyPath.Child("installer"), "installer is required when updateStrategy.type is InstallerUpdate"))
		} else {
			errList = append(errList, validateInstallerSpec(strategy.Installer, strategyPath.Child("installer"))...)
		}
	} else if strategy.Installer != nil {
		errList = append(errList, field.Forbidden(strategyPath.Child("installer"), "installer is only allowed when updateStrategy.type is InstallerUpdate"))
	}

	// CheckpointRestore and InstallerUpdate both preserve the writable layer;
	// changing an image would invalidate the checkpoint or destroy the layer the
	// install writes into.
	if (strategy.Type == agentsv1alpha1.SandboxUpdateOpsStrategyCheckpointRestore ||
		strategy.Type == agentsv1alpha1.SandboxUpdateOpsStrategyInstallerUpdate) && len(obj.Spec.Patch.Raw) > 0 {
		patchTmpl := &corev1.PodTemplateSpec{}
		if err := json.Unmarshal(obj.Spec.Patch.Raw, patchTmpl); err != nil {
			errList = append(errList, field.Invalid(specPath.Child("patch"), obj.Spec.Patch, "failed to parse patch as PodTemplateSpec: "+err.Error()))
		} else if msg := validateNoImageChange(patchTmpl, strategy.Type); msg != "" {
			errList = append(errList, field.Forbidden(specPath.Child("patch"), msg))
		}
	}

	return errList
}

// validateInstallerSpec validates the fields of an installer configuration.
func validateInstallerSpec(installer *agentsv1alpha1.InstallerSpec, path *field.Path) field.ErrorList {
	var errList field.ErrorList

	if installer.Image == "" {
		errList = append(errList, field.Required(path.Child("image"), "installer.image is required"))
	}
	for i, cmd := range installer.Command {
		if cmd == "" {
			errList = append(errList, field.Invalid(path.Child("command").Index(i), cmd, "command elements must be non-empty"))
		}
	}
	if installer.TimeoutSeconds != nil && *installer.TimeoutSeconds < 1 {
		errList = append(errList, field.Invalid(path.Child("timeoutSeconds"), *installer.TimeoutSeconds, "timeoutSeconds must be positive"))
	}
	for _, env := range installer.Env {
		if strings.HasPrefix(env.Name, "SANDBOX_INSTALLER_") {
			errList = append(errList, field.Forbidden(path.Child("env").Key(env.Name), "env names with the SANDBOX_INSTALLER_ prefix are reserved for the controller"))
		}
	}
	if installer.SecurityContext != nil && installer.SecurityContext.Privileged != nil && *installer.SecurityContext.Privileged {
		errList = append(errList, field.Forbidden(path.Child("securityContext", "privileged"), "privileged installer containers are not allowed"))
	}

	return errList
}

// validateNoImageChange checks whether the given patch template modifies any container
// or init container image. Returns a non-empty string describing the violation if found.
func validateNoImageChange(tmpl *corev1.PodTemplateSpec, strategyType agentsv1alpha1.SandboxUpdateOpsStrategyType) string {
	for _, c := range tmpl.Spec.Containers {
		if c.Image != "" {
			return fmt.Sprintf("%s strategy does not support modifying container images (container %q)", strategyType, c.Name)
		}
	}
	for _, c := range tmpl.Spec.InitContainers {
		if c.Image != "" {
			return fmt.Sprintf("%s strategy does not support modifying init container images (container %q)", strategyType, c.Name)
		}
	}
	return ""
}

func (h *SandboxUpdateOpsValidatingHandler) handleUpdate(req admission.Request, newObj *agentsv1alpha1.SandboxUpdateOps) admission.Response {
	oldObj := &agentsv1alpha1.SandboxUpdateOps{}
	if err := h.Decoder.DecodeRaw(req.OldObject, oldObj); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	var errList field.ErrorList
	specPath := field.NewPath("spec")

	// Only allow changes to UpdateStrategy, Paused, and StateFilter
	if !reflect.DeepEqual(oldObj.Spec.Selector, newObj.Spec.Selector) {
		errList = append(errList, field.Forbidden(specPath.Child("selector"), "selector is immutable"))
	}
	if !reflect.DeepEqual(oldObj.Spec.Patch, newObj.Spec.Patch) {
		errList = append(errList, field.Forbidden(specPath.Child("patch"), "patch is immutable"))
	}
	if !reflect.DeepEqual(oldObj.Spec.Lifecycle, newObj.Spec.Lifecycle) {
		errList = append(errList, field.Forbidden(specPath.Child("lifecycle"), "lifecycle is immutable"))
	}
	if !reflect.DeepEqual(oldObj.Spec.UpdateStrategy.Installer, newObj.Spec.UpdateStrategy.Installer) {
		errList = append(errList, field.Forbidden(specPath.Child("updateStrategy", "installer"), "installer is immutable"))
	}

	// updateStrategy.type stays mutable, so the strategy-level rules must hold
	// on update as well.
	errList = append(errList, h.validateUpdateStrategy(newObj, specPath)...)

	if len(errList) > 0 {
		return admission.Errored(http.StatusUnprocessableEntity, errList.ToAggregate())
	}
	return admission.Allowed("")
}
