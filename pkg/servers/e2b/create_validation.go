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

import "github.com/openkruise/agents/pkg/servers/e2b/models"

// ValidateCreateNetworkConfig exposes the native create validator to API adapters.
// It performs no allocation, backend access, or Controller orchestration.
func ValidateCreateNetworkConfig(input *models.SandboxNetworkConfig) (*models.SandboxNetworkConfig, error) {
	return validateAndBuildNetworkConfig(input)
}

// ResolveCreateSecurityRules validates and normalizes native inline security rules.
// Callers must parse metadata extensions before invoking this pure function.
func ResolveCreateSecurityRules(request *models.NewSandboxRequest) (string, error) {
	return resolveSecurityRules(request)
}
