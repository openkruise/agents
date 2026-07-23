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

package e2b

import (
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/pkg/servers/web"
)

func withSandboxResource(apiErr *web.ApiError, sandbox metav1.Object) *web.ApiError {
	if apiErr == nil || sandbox == nil {
		return apiErr
	}
	resource := fmt.Sprintf("sandboxResource=%s/%s", sandbox.GetNamespace(), sandbox.GetName())
	if strings.Contains(apiErr.Message, resource) {
		return apiErr
	}
	if apiErr.Message == "" {
		apiErr.Message = resource
	} else {
		apiErr.Message = fmt.Sprintf("%s; %s", apiErr.Message, resource)
	}
	return apiErr
}
