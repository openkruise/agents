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

// Package autopause validates the probe and auto-pause configuration shared by
// Sandbox, SandboxSet and SandboxTemplate. The generated CRD schema constrains
// field types and ranges, but the cross-field rules this package enforces -
// unique probe names, exec-only handlers, and policy rules that must reference
// a defined probe - can only be checked by webhooks and the controller.
//
// Admission covers SandboxSet and SandboxTemplate only, so the controller runs
// the same rules on every Sandbox and reports the outcome on the ProbeValid
// condition. The rules must therefore agree with what the reconciler actually
// does: rejecting a configuration the reconciler would have handled would make
// the same object valid for a Sandbox and invalid for a SandboxSet.
package autopause

import (
	"fmt"
	"regexp"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// ValidateProbes validates the probe list, rejecting duplicate names and probes
// whose handler the controller cannot execute.
func ValidateProbes(probes []agentsv1alpha1.Probe, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	seen := make(map[string]struct{}, len(probes))
	for i := range probes {
		idxPath := fldPath.Index(i)
		allErrs = append(allErrs, ValidateProbe(&probes[i], idxPath)...)
		if probes[i].Name == "" {
			continue
		}
		if _, ok := seen[probes[i].Name]; ok {
			allErrs = append(allErrs, field.Duplicate(idxPath.Child("name"), probes[i].Name))
			continue
		}
		seen[probes[i].Name] = struct{}{}
	}
	return allErrs
}

// ValidateProbe validates a single probe configuration. corev1.Probe is embedded
// inline, so its fields are addressed directly under fldPath rather than under a
// "probe" child.
func ValidateProbe(probe *agentsv1alpha1.Probe, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if probe.Name == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("name"), "probe name is required"))
	} else {
		// The probe name becomes the suffix of the Condition type written to
		// both the Pod and the Sandbox, so an invalid name makes every status
		// update fail instead of only breaking this probe.
		condType := agentsv1alpha1.ProbeConditionType(probe.Name)
		for _, msg := range validation.IsQualifiedName(condType) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("name"), probe.Name,
				fmt.Sprintf("%s: the name is reported as condition type %q", msg, condType)))
		}
	}

	// Count how many handler types are set.
	handler := probe.ProbeHandler
	handlers := 0
	if handler.Exec != nil {
		handlers++
	}
	if handler.HTTPGet != nil {
		handlers++
	}
	if handler.TCPSocket != nil {
		handlers++
	}
	if handler.GRPC != nil {
		handlers++
	}

	switch {
	case handlers == 0:
		allErrs = append(allErrs, field.Required(fldPath, "must specify exactly one probe handler"))
	case handlers > 1:
		allErrs = append(allErrs, field.Forbidden(fldPath, "only one probe handler can be specified"))
	case handler.Exec == nil:
		// Currently only exec is supported.
		allErrs = append(allErrs, field.NotSupported(fldPath, handlerName(handler), []string{"exec"}))
	case len(handler.Exec.Command) == 0:
		allErrs = append(allErrs, field.Required(fldPath.Child("exec", "command"), "exec command is required"))
	}

	if probe.PeriodSeconds < 0 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("periodSeconds"), probe.PeriodSeconds, "must be greater than or equal to 0"))
	}
	if probe.TimeoutSeconds < 0 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("timeoutSeconds"), probe.TimeoutSeconds, "must be greater than or equal to 0"))
	}
	if probe.FailureThreshold < 0 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("failureThreshold"), probe.FailureThreshold, "must be greater than or equal to 0"))
	}

	return allErrs
}

// handlerName returns the JSON name of the handler set on a probe, so a rejection
// names the field the user actually wrote.
func handlerName(handler corev1.ProbeHandler) string {
	switch {
	case handler.Exec != nil:
		return "exec"
	case handler.HTTPGet != nil:
		return "httpGet"
	case handler.TCPSocket != nil:
		return "tcpSocket"
	case handler.GRPC != nil:
		return "grpc"
	}
	return ""
}

// ValidateAutoPausePolicy validates the auto-pause policy against the probes it
// references. A rule pointing at an undefined probe never fires, so it is
// rejected at admission time rather than silently doing nothing.
func ValidateAutoPausePolicy(policy *agentsv1alpha1.AutoPausePolicy, probes []agentsv1alpha1.Probe, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if policy == nil {
		return allErrs
	}

	var pauseRule *agentsv1alpha1.ProbedIdleStateRule
	if policy.Pause != nil {
		pauseRule = policy.Pause.WhenProbedIdleState
	}
	var resumeRule *agentsv1alpha1.ProbedScheduleTimeRule
	if policy.Resume != nil {
		resumeRule = policy.Resume.WhenProbedScheduleTime
	}
	// A policy carrying no rule at all can never fire. Setting it is almost
	// always a mistake, and accepting it hides that mistake behind a healthy
	// ProbeValid condition.
	if pauseRule == nil && resumeRule == nil {
		return append(allErrs, field.Required(fldPath,
			"at least one of pause.whenProbedIdleState or resume.whenProbedScheduleTime is required"))
	}

	defined := make(map[string]struct{}, len(probes))
	for i := range probes {
		if probes[i].Name != "" {
			defined[probes[i].Name] = struct{}{}
		}
	}

	if pauseRule != nil {
		rule := pauseRule
		rulePath := fldPath.Child("pause", "whenProbedIdleState")
		allErrs = append(allErrs, validateProbeReference(rule.Probe, defined, rulePath.Child("probe"))...)
		if rule.MessageRegex == "" {
			allErrs = append(allErrs, field.Required(rulePath.Child("messageRegex"), "messageRegex is required"))
		} else if _, err := regexp.Compile(rule.MessageRegex); err != nil {
			allErrs = append(allErrs, field.Invalid(rulePath.Child("messageRegex"), rule.MessageRegex, err.Error()))
		}
		// The reconciler only treats a nil threshold as unusable; zero means pause
		// as soon as the probe reports idle. Rejecting zero here would make the
		// same object valid for a Sandbox and invalid for a SandboxSet.
		switch {
		case rule.ThresholdDuration == nil:
			allErrs = append(allErrs, field.Required(rulePath.Child("thresholdDuration"), "thresholdDuration is required"))
		case rule.ThresholdDuration.Duration < 0:
			allErrs = append(allErrs, field.Invalid(rulePath.Child("thresholdDuration"), rule.ThresholdDuration.Duration.String(), "must be greater than or equal to 0"))
		}
	}

	if resumeRule != nil {
		rule := resumeRule
		rulePath := fldPath.Child("resume", "whenProbedScheduleTime")
		allErrs = append(allErrs, validateProbeReference(rule.Probe, defined, rulePath.Child("probe"))...)
		switch rule.TimeFormat {
		case "", agentsv1alpha1.ProbeTimeFormatUnix, agentsv1alpha1.ProbeTimeFormatDatetime:
		default:
			allErrs = append(allErrs, field.NotSupported(rulePath.Child("timeFormat"), rule.TimeFormat,
				[]string{agentsv1alpha1.ProbeTimeFormatUnix, agentsv1alpha1.ProbeTimeFormatDatetime}))
		}
		// Zero is accepted for the same reason as thresholdDuration: it means
		// "resume exactly at the scheduled time". A negative lead time would resume
		// after the task it exists to wake up for, and resumeLeadTime treats it as
		// unset, so rejecting it here does not diverge from the reconciler.
		if rule.LeadTime != nil && rule.LeadTime.Duration < 0 {
			allErrs = append(allErrs, field.Invalid(rulePath.Child("leadTime"), rule.LeadTime.Duration.String(), "must be greater than or equal to 0"))
		}
	}

	return allErrs
}

// validateProbeReference checks that a policy rule references a probe that is
// actually defined in the same spec.
func validateProbeReference(name string, defined map[string]struct{}, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if name == "" {
		allErrs = append(allErrs, field.Required(fldPath, "probe is required"))
		return allErrs
	}
	if _, ok := defined[name]; !ok {
		allErrs = append(allErrs, field.Invalid(fldPath, name, "must reference a probe name defined in spec.probes"))
	}
	return allErrs
}
