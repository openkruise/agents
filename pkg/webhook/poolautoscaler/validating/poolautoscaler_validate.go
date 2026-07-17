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
	"time"

	"github.com/robfig/cron/v3"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// PoolAutoscalerValidatingHandler handles admission validation for PoolAutoscaler.
type PoolAutoscalerValidatingHandler struct {
	Client  client.Client
	Decoder admission.Decoder
}

// +kubebuilder:webhook:path=/validate-poolautoscaler,mutating=false,failurePolicy=fail,sideEffects=None,admissionReviewVersions=v1;v1beta1,groups=agents.kruise.io,resources=poolautoscalers,verbs=create;update,versions=v1alpha1,name=v-pa.kb.io

func (h *PoolAutoscalerValidatingHandler) Path() string {
	return "/validate-poolautoscaler"
}

func (h *PoolAutoscalerValidatingHandler) Enabled() bool {
	return true
}

func (h *PoolAutoscalerValidatingHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	obj := &agentsv1alpha1.PoolAutoscaler{}
	if err := h.Decoder.Decode(req, obj); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	var errList field.ErrorList
	errList = append(errList, validatePoolAutoscalerSpec(obj.Spec, field.NewPath("spec"))...)
	errList = append(errList, h.validateOneToOne(ctx, obj)...)
	if len(errList) > 0 {
		return admission.Errored(http.StatusUnprocessableEntity, errList.ToAggregate())
	}
	return admission.Allowed("")
}

func validatePoolAutoscalerSpec(spec agentsv1alpha1.PoolAutoscalerSpec, fldPath *field.Path) field.ErrorList {
	var errList field.ErrorList

	// Validate scaleTargetRef
	refPath := fldPath.Child("scaleTargetRef")
	if spec.ScaleTargetRef.Kind == "" {
		errList = append(errList, field.Required(refPath.Child("kind"), "kind is required"))
	}
	if spec.ScaleTargetRef.Name == "" {
		errList = append(errList, field.Required(refPath.Child("name"), "name is required"))
	}
	if spec.ScaleTargetRef.Kind != "" && spec.ScaleTargetRef.Kind != "SandboxSet" {
		errList = append(errList, field.Invalid(refPath.Child("kind"), spec.ScaleTargetRef.Kind, "only SandboxSet is supported"))
	}

	// Validate maxReplicas
	if spec.MaxReplicas <= 0 {
		errList = append(errList, field.Invalid(fldPath.Child("maxReplicas"), spec.MaxReplicas, "must be greater than 0"))
	}

	// Validate minReplicas
	if spec.MinReplicas < 0 {
		errList = append(errList, field.Invalid(fldPath.Child("minReplicas"), spec.MinReplicas, "must be >= 0"))
	}
	if spec.MinReplicas > 0 && spec.MinReplicas >= spec.MaxReplicas {
		errList = append(errList, field.Invalid(fldPath.Child("minReplicas"), spec.MinReplicas,
			fmt.Sprintf("must be < maxReplicas (%d)", spec.MaxReplicas)))
	}

	// Validate cron policies
	if len(spec.CronPolicies) > 0 {
		errList = append(errList, validateCronPolicies(spec.CronPolicies, fldPath.Child("cronPolicies"))...)
	}

	// Validate capacity policy
	if spec.CapacityPolicy != nil {
		errList = append(errList, validateCapacityPolicy(spec.CapacityPolicy, fldPath.Child("capacityPolicy"))...)
	}

	return errList
}

func validateCronPolicies(policies []agentsv1alpha1.CronScalingPolicy, fldPath *field.Path) field.ErrorList {
	var errList field.ErrorList
	names := make(map[string]struct{})

	for i, policy := range policies {
		pPath := fldPath.Index(i)
		if policy.Name == "" {
			errList = append(errList, field.Required(pPath.Child("name"), "name is required"))
		}
		if _, exists := names[policy.Name]; exists {
			errList = append(errList, field.Duplicate(pPath.Child("name"), policy.Name))
		}
		names[policy.Name] = struct{}{}

		if policy.Schedule == "" {
			errList = append(errList, field.Required(pPath.Child("schedule"), "schedule is required"))
		} else if _, err := cronParser.Parse(policy.Schedule); err != nil {
			errList = append(errList, field.Invalid(pPath.Child("schedule"), policy.Schedule,
				fmt.Sprintf("invalid cron expression: %v", err)))
		}

		if policy.TimeZone != nil && *policy.TimeZone != "" {
			if _, err := time.LoadLocation(*policy.TimeZone); err != nil {
				errList = append(errList, field.Invalid(pPath.Child("timeZone"), *policy.TimeZone,
					fmt.Sprintf("invalid timezone: %v", err)))
			}
		}

		if policy.TargetReplicas < 0 {
			errList = append(errList, field.Invalid(pPath.Child("targetReplicas"), policy.TargetReplicas, "must be >= 0"))
		}
	}
	return errList
}

func validateCapacityPolicy(policy *agentsv1alpha1.CapacityPolicy, fldPath *field.Path) field.ErrorList {
	var errList field.ErrorList

	// Validate targetAvailable
	errList = append(errList, validateIntOrPercentNonNegative(policy.TargetAvailable, fldPath.Child("targetAvailable"))...)

	// Validate tolerance
	if policy.Tolerance != nil {
		errList = append(errList, validateIntOrPercentNonNegative(*policy.Tolerance, fldPath.Child("tolerance"))...)
	}

	// Validate stabilization windows
	if policy.ScaleUp != nil && policy.ScaleUp.StabilizationWindowSeconds != nil {
		w := *policy.ScaleUp.StabilizationWindowSeconds
		if w < 0 || w > 3600 {
			errList = append(errList, field.Invalid(fldPath.Child("scaleUp").Child("stabilizationWindowSeconds"),
				w, "must be >= 0 and <= 3600"))
		}
	}
	if policy.ScaleDown != nil && policy.ScaleDown.StabilizationWindowSeconds != nil {
		w := *policy.ScaleDown.StabilizationWindowSeconds
		if w < 0 || w > 3600 {
			errList = append(errList, field.Invalid(fldPath.Child("scaleDown").Child("stabilizationWindowSeconds"),
				w, "must be >= 0 and <= 3600"))
		}
	}

	return errList
}

func validateIntOrPercentNonNegative(val intstr.IntOrString, fldPath *field.Path) field.ErrorList {
	var errList field.ErrorList
	if val.Type == intstr.Int {
		if val.IntVal < 0 {
			errList = append(errList, field.Invalid(fldPath, val.IntVal, "must be >= 0"))
		}
	}
	// Percentage validation: the string format is handled by Kubernetes intstr parsing
	return errList
}

// validateOneToOne ensures at most one PoolAutoscaler targets a given SandboxSet.
func (h *PoolAutoscalerValidatingHandler) validateOneToOne(ctx context.Context, obj *agentsv1alpha1.PoolAutoscaler) field.ErrorList {
	var errList field.ErrorList
	list := &agentsv1alpha1.PoolAutoscalerList{}
	if err := h.Client.List(ctx, list, client.InNamespace(obj.Namespace),
		client.MatchingFields{"spec.scaleTargetRef.name": obj.Spec.ScaleTargetRef.Name}); err != nil {
		errList = append(errList, field.InternalError(field.NewPath("spec").Child("scaleTargetRef"),
			fmt.Errorf("failed to list PoolAutoscalers: %w", err)))
		return errList
	}

	for i := range list.Items {
		existing := &list.Items[i]
		// Skip self on update
		if existing.Name == obj.Name && existing.Namespace == obj.Namespace {
			continue
		}
		if existing.Spec.ScaleTargetRef.Kind == obj.Spec.ScaleTargetRef.Kind {
			errList = append(errList, field.Invalid(field.NewPath("spec").Child("scaleTargetRef").Child("name"),
				obj.Spec.ScaleTargetRef.Name,
				fmt.Sprintf("SandboxSet %q is already managed by PoolAutoscaler %q", obj.Spec.ScaleTargetRef.Name, existing.Name)))
		}
	}
	return errList
}
