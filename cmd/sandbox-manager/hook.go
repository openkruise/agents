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

package main

import (
	"github.com/openkruise/agents/pkg/identity/secureprovider"
)

// defaultStartupHook installs the remote identity provider selected by
// IDENTITY_PROVIDER_CREDENTIAL_SOURCE. It is a no-op when the variable is unset.
var defaultStartupHook = secureprovider.InstallProviderFromEnv

// startupHook runs once after the user-cluster rest.Config is established and
// the optional secret config has been overlaid onto process settings,
// but before the e2b controller or any listener starts.
var startupHook = defaultStartupHook
