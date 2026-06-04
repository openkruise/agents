/*
Copyright 2025 The Kruise Authors.

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

package controller

import (
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/openkruise/agents/pkg/controller/sandbox"
	"github.com/openkruise/agents/pkg/controller/sandboxclaim"
	"github.com/openkruise/agents/pkg/controller/sandboxset"
	"github.com/openkruise/agents/pkg/controller/sandboxupdateops"
	"github.com/openkruise/agents/pkg/controller/securitytokenrefresh"
)

// Deps bundles process-wide dependencies passed to controller Add funcs.
// New dependencies should be appended here rather than introducing extra
// AddFunc parameters across all controllers.
type Deps struct {
	MetricsCleanup sandbox.Enqueuer
}

func SetupWithManager(m manager.Manager, deps Deps) error {
	if err := sandbox.Add(m, deps.MetricsCleanup); err != nil {
		return err
	}
	if err := sandboxset.Add(m); err != nil {
		return err
	}
	if err := sandboxclaim.Add(m); err != nil {
		return err
	}
	if err := sandboxupdateops.Add(m); err != nil {
		return err
	}
	if err := securitytokenrefresh.Add(m); err != nil {
		return err
	}
	return nil
}
