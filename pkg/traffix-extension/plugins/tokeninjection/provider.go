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

package tokeninjection

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// expectedTokenProviderGroup is the API group of supported token providers.
const expectedTokenProviderGroup = "agentidentity.alibabacloud.com"

// expectedTokenProviderKind is the supported provider Kind.
const expectedTokenProviderKind = "CredentialProvider"

// ValidateTokenProviderRef checks if the tokenProviderRef points to a supported provider.
func ValidateTokenProviderRef(ref *corev1.TypedLocalObjectReference) error {
	if ref == nil {
		return fmt.Errorf("token provider ref is nil")
	}
	if ref.Kind != expectedTokenProviderKind {
		return fmt.Errorf("unsupported token provider kind %q, expected %s", ref.Kind, expectedTokenProviderKind)
	}
	apiGroup := ""
	if ref.APIGroup != nil {
		apiGroup = *ref.APIGroup
	}
	if apiGroup != expectedTokenProviderGroup {
		return fmt.Errorf("unsupported token provider group %q, expected %s", apiGroup, expectedTokenProviderGroup)
	}
	if ref.Name == "" {
		return fmt.Errorf("token provider name is empty")
	}
	return nil
}
