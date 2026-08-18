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

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	intstrutil "k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/features"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
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

	// 3. Validate Lifecycle configuration. All strategies, including InplaceUpdate,
	// run through the sandbox controller's upgrade lifecycle, so PreUpgrade and
	// PostUpgrade hooks apply to every strategy.
	if obj.Spec.Lifecycle != nil {
		lifecyclePath := specPath.Child("lifecycle")
		if obj.Spec.Lifecycle.PreUpgrade != nil && obj.Spec.Lifecycle.PreUpgrade.Exec == nil {
			errList = append(errList, field.Required(lifecyclePath.Child("preUpgrade", "exec"), "exec is required when preUpgrade is specified"))
		}
		if obj.Spec.Lifecycle.PostUpgrade != nil && obj.Spec.Lifecycle.PostUpgrade.Exec == nil {
			errList = append(errList, field.Required(lifecyclePath.Child("postUpgrade", "exec"), "exec is required when postUpgrade is specified"))
		}
	}

	// 4. When using CheckpointRestore strategy, the patch must not modify container images.
	// CheckpointRestore preserves the writable layer of containers whose image is unchanged;
	// changing an image would invalidate the checkpoint.
	if obj.Spec.UpdateStrategy.Type == agentsv1alpha1.SandboxUpdateOpsStrategyCheckpointRestore && len(obj.Spec.Patch.Raw) > 0 {
		patchTmpl := &corev1.PodTemplateSpec{}
		if err := json.Unmarshal(obj.Spec.Patch.Raw, patchTmpl); err != nil {
			errList = append(errList, field.Invalid(specPath.Child("patch"), obj.Spec.Patch, "failed to parse patch as PodTemplateSpec: "+err.Error()))
		} else if msg := validateNoImageChange(patchTmpl); msg != "" {
			errList = append(errList, field.Forbidden(specPath.Child("patch"), msg))
		}
	}

	// 5. When using InplaceUpdate strategy and the corresponding validation feature gate
	// is enabled, the patch may only modify container images, resources, and pod template
	// metadata (labels/annotations). Rejecting other fields (env, volumes, command, ...)
	// at admission time surfaces the error to the user immediately instead of failing
	// later during reconciliation.
	if utilfeature.DefaultFeatureGate.Enabled(features.SandboxUpdateOpsInplacePatchValidationGate) &&
		obj.Spec.UpdateStrategy.Type == agentsv1alpha1.SandboxUpdateOpsStrategyInplaceUpdate && len(obj.Spec.Patch.Raw) > 0 {
		if msg := validateInplacePatchAllowedFields(obj.Spec.Patch.Raw); msg != "" {
			errList = append(errList, field.Forbidden(specPath.Child("patch"), msg))
		}
	}

	// 6. Check for active (non-terminal) SandboxUpdateOps in the same namespace
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

// validateNoImageChange checks whether the given patch template modifies any container
// or init container image. Returns a non-empty string describing the violation if found.
func validateNoImageChange(tmpl *corev1.PodTemplateSpec) string {
	for _, c := range tmpl.Spec.Containers {
		if c.Image != "" {
			return fmt.Sprintf("CheckpointRestore strategy does not support modifying container images (container %q)", c.Name)
		}
	}
	for _, c := range tmpl.Spec.InitContainers {
		if c.Image != "" {
			return fmt.Sprintf("CheckpointRestore strategy does not support modifying init container images (container %q)", c.Name)
		}
	}
	return ""
}

// validateInplacePatchAllowedFields checks that the raw patch only touches fields
// supported by the in-place update path: pod template metadata (labels/annotations)
// and container/initContainer image and resources. It operates on the raw JSON
// instead of a typed struct so that unknown fields and strategic-merge-patch
// directives are also caught. JSON null values are treated as "not set": typed
// clients marshal noise like "creationTimestamp": null, and explicit null deletions
// are still caught by the hash-immutable-part checks during reconciliation.
// Returns a non-empty message describing the first violation found.
func validateInplacePatchAllowedFields(raw []byte) string {
	patch := map[string]interface{}{}
	if err := json.Unmarshal(raw, &patch); err != nil {
		return "failed to parse patch as a JSON object: " + err.Error()
	}
	for key, val := range patch {
		if val == nil {
			continue
		}
		switch key {
		case "metadata":
			meta, ok := val.(map[string]interface{})
			if !ok {
				return "metadata must be a JSON object"
			}
			for mk, mv := range meta {
				if mv == nil {
					continue
				}
				if mk != "labels" && mk != "annotations" {
					return fmt.Sprintf("InplaceUpdate strategy does not support modifying metadata.%s (only labels and annotations are allowed)", mk)
				}
			}
		case "spec":
			spec, ok := val.(map[string]interface{})
			if !ok {
				return "spec must be a JSON object"
			}
			for sk, sv := range spec {
				if sv == nil {
					continue
				}
				if sk != "containers" && sk != "initContainers" {
					return fmt.Sprintf("InplaceUpdate strategy does not support modifying spec.%s (only container images and resources are allowed)", sk)
				}
				containers, ok := sv.([]interface{})
				if !ok {
					return fmt.Sprintf("spec.%s must be a JSON array", sk)
				}
				for i, item := range containers {
					c, ok := item.(map[string]interface{})
					if !ok {
						return fmt.Sprintf("spec.%s[%d] must be a JSON object", sk, i)
					}
					for ck, cv := range c {
						if cv == nil {
							continue
						}
						switch ck {
						case "name", "image", "resources":
						default:
							return fmt.Sprintf("InplaceUpdate strategy does not support modifying spec.%s[%d].%s (only image and resources are allowed)", sk, i, ck)
						}
					}
				}
			}
		default:
			return fmt.Sprintf("InplaceUpdate strategy does not support field %q in patch (only metadata labels/annotations, container images and resources are allowed)", key)
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

	// Only allow changes to UpdateStrategy.MaxUnavailable, Paused, and StateFilter
	if !reflect.DeepEqual(oldObj.Spec.Selector, newObj.Spec.Selector) {
		errList = append(errList, field.Forbidden(specPath.Child("selector"), "selector is immutable"))
	}
	if !reflect.DeepEqual(oldObj.Spec.Patch, newObj.Spec.Patch) {
		errList = append(errList, field.Forbidden(specPath.Child("patch"), "patch is immutable"))
	}
	if !reflect.DeepEqual(oldObj.Spec.Lifecycle, newObj.Spec.Lifecycle) {
		errList = append(errList, field.Forbidden(specPath.Child("lifecycle"), "lifecycle is immutable"))
	}
	// Changing the strategy type mid-flight is semantically incorrect:
	// already-patched sandboxes follow the old strategy while unpatched ones
	// would follow the new one.
	if oldObj.Spec.UpdateStrategy.Type != newObj.Spec.UpdateStrategy.Type {
		errList = append(errList, field.Forbidden(specPath.Child("updateStrategy", "type"), "updateStrategy.type is immutable"))
	}

	if len(errList) > 0 {
		return admission.Errored(http.StatusUnprocessableEntity, errList.ToAggregate())
	}
	return admission.Allowed("")
}
