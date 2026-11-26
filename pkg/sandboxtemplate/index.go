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

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/openkruise/agents/api/v1alpha1"
)

func VersionOwnerReference(obj metav1.Object) *metav1.OwnerReference {
	owner := metav1.GetControllerOfNoCopy(obj)
	if owner == nil || owner.APIVersion != api.GroupVersion.String() || owner.Kind != "SandboxTemplate" || owner.Name == "" || owner.UID == "" {
		return nil
	}
	return owner
}
