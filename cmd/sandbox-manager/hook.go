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
	"context"

	"k8s.io/client-go/rest"
)

// defaultStartupHook is the community no-op startup extension point. Internal
// builds override startupHook (same package) via init() to install bootstrap
// logic such as the hosted IdP client; this build keeps it a no-op.
func defaultStartupHook(context.Context, *rest.Config) error { return nil }

// startupHook runs once after the user-cluster rest.Config is established and
// the optional secret config has been overlaid onto process settings,
// but before the e2b controller or any listener starts.
var startupHook = defaultStartupHook
