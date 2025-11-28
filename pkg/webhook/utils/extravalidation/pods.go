package extravalidation

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
)

type PodTemplateValidator func(spec corev1.PodTemplateSpec, fldPath *field.Path) field.ErrorList

func GetExtraPodTemplateValidators() []PodTemplateValidator {
	return []PodTemplateValidator{
		validateACSPodTemplate,
	}
}

func validateACSPodTemplate(tmpl corev1.PodTemplateSpec, fldPath *field.Path) field.ErrorList {
	var errList field.ErrorList
	errList = append(errList, validateACSPodTemplateSpec(tmpl.Spec, fldPath.Child("spec"))...)
	return errList
}

func validateACSPodTemplateSpec(spec corev1.PodSpec, fldPath *field.Path) field.ErrorList {
	var errList field.ErrorList
	if len(spec.Containers) != 1 {
		errList = append(errList, field.Invalid(fldPath.Child("containers[0]"), spec.Containers, "sandbox template should have only one container"))
	}
	for _, container := range spec.Containers {
		fld := fldPath.Child("containers[0]")
		if container.LivenessProbe != nil {
			errList = append(errList, field.Invalid(fld.Child("livenessProbe"), container.LivenessProbe, "sandbox template should not have liveness probe"))
		}
		if container.ReadinessProbe != nil {
			errList = append(errList, field.Invalid(fld.Child("readinessProbe"), container.ReadinessProbe, "sandbox template should not have readiness probe"))
		}
		if sc := container.SecurityContext; sc != nil {
			fld := fld.Child("securityContext")
			if ptr.Deref(sc.Privileged, false) {
				errList = append(errList, field.Invalid(fld.Child("privileged"), sc.Privileged, "sandbox template should not have privileged == true"))
			}
			if ptr.Deref(sc.AllowPrivilegeEscalation, false) {
				errList = append(errList, field.Invalid(fld.Child("allowPrivilegeEscalation"), sc.AllowPrivilegeEscalation, "sandbox template should not have allowPrivilegeEscalation == true"))
			}
		}
	}
	for i, volume := range spec.Volumes {
		fld := fldPath.Child(fmt.Sprintf("volumes[%d]", i))
		if volume.DownwardAPI != nil {
			errList = append(errList, field.Invalid(fld.Child("downwardAPI"), volume.DownwardAPI, "sandbox template should not have downward api volume"))
		}
		if volume.ConfigMap != nil {
			errList = append(errList, field.Invalid(fld.Child("configMap"), volume.ConfigMap, "sandbox template should not have configmap volume"))
		}
		if volume.Secret != nil {
			errList = append(errList, field.Invalid(fld.Child("secret"), volume.Secret, "sandbox template should not have secret volume"))
		}
		if volume.HostPath != nil {
			errList = append(errList, field.Invalid(fld.Child("hostPath"), volume.HostPath, "sandbox template should not have host path volume"))
		}
		if volume.Projected != nil {
			errList = append(errList, field.Invalid(fld.Child("projected"), volume.Projected, "sandbox template should not have projected volume"))
		}
		if volume.EmptyDir != nil && volume.EmptyDir.Medium == corev1.StorageMediumMemory {
			errList = append(errList, field.Invalid(fld.Child("emptyDir"), volume.EmptyDir, "sandbox template should not have memory emptyDir volume"))
		}
	}
	if ptr.Deref(spec.ShareProcessNamespace, false) {
		errList = append(errList, field.Invalid(fldPath.Child("shareProcessNamespace"), spec.ShareProcessNamespace, "sandbox template should not have shareProcessNamespace == true"))
	}
	if spec.HostPID {
		errList = append(errList, field.Invalid(fldPath.Child("hostPID"), spec.HostPID, "sandbox template should not have hostPID == true"))
	}
	if spec.HostIPC {
		errList = append(errList, field.Invalid(fldPath.Child("hostIPC"), spec.HostIPC, "sandbox template should not have hostIPC == true"))
	}
	if spec.HostNetwork {
		errList = append(errList, field.Invalid(fldPath.Child("hostNetwork"), spec.HostNetwork, "sandbox template should not have hostNetwork == true"))
	}
	if ptr.Deref(spec.AutomountServiceAccountToken, false) {
		// should be set to false explicitly by mutation webhook
		errList = append(errList, field.Invalid(fldPath.Child("automountServiceAccountToken"), spec.AutomountServiceAccountToken, "sandbox template should not have automountServiceAccountToken == true"))
	}
	return errList
}
