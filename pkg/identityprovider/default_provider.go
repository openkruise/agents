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

package identityprovider

// DefaultProvider is the global IdentityProvider instance used for issuing sandbox access tokens
// and propagating security tokens to sandbox runtimes.
//
// Community default: UUID-based provider with no-op propagation.
// Internal deployment: Overridden by init() in inner_config.go to use secureIdentityProvider
// with HTTPS token issuance and registered propagators.
//
// IMPORTANT: This variable MUST only be set during init() phase. It is NOT safe
// to modify at runtime due to concurrent access from multiple goroutines.
//
// All callers should use this variable directly:
//
//	identityprovider.DefaultProvider.IssueToken(ctx, req)
//	identityprovider.DefaultProvider.PropagateSecurityToken(ctx, sbx, tokenResp)
var DefaultProvider IdentityProvider = NewUUIDIdentityProvider()
