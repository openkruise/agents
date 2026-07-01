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

package lifecycle

import agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"

// IsNotTerminating reports whether sbx is a live sandbox that should count
// towards quota. A sandbox is considered not-terminating when it is non-nil,
// has no DeletionTimestamp, and is not in the Terminating phase.
func IsNotTerminating(sbx *agentsv1alpha1.Sandbox) bool {
	if sbx == nil {
		return false
	}
	return sbx.GetDeletionTimestamp() == nil && sbx.Status.Phase != agentsv1alpha1.SandboxTerminating
}

// IsLiveForQuota reports whether sbx should still occupy API-key quota.
func IsLiveForQuota(sbx *agentsv1alpha1.Sandbox) bool {
	if !IsNotTerminating(sbx) {
		return false
	}
	if sbx.Status.Phase == agentsv1alpha1.SandboxReusing {
		return false
	}
	annotations := sbx.GetAnnotations()
	return !(annotations[agentsv1alpha1.AnnotationReuse] == "true" &&
		annotations[agentsv1alpha1.AnnotationReuseEnabled] == "true")
}
