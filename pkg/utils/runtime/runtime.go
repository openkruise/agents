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

package runtime

// runtime.go holds the addressing and refresh primitives shared by every
// runtime capability group (process, filesystem, storage, init, csi):
// resolving the agent-runtime endpoint for a sandbox and re-resolving the
// sandbox between retries.

import (
	"context"
	"fmt"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

// RefreshFunc is a callback that refreshes the sandbox object to its latest state.
// It returns the updated sandbox object, allowing InitRuntime to use the latest
// annotations (e.g., runtime URL) without depending on the sandboxcr package.
type RefreshFunc func(ctx context.Context) (*agentsv1alpha1.Sandbox, error)

// GetRuntimeURL resolves the agent-runtime endpoint for a Sandbox.
//
// Lookup order:
//  1. AnnotationRuntimeURL on the Sandbox object.
//  2. AnnotationEnvdURL on the Sandbox object (legacy key, kept for backwards compatibility).
//  3. Pod IP from the cached route plus the well-known utils.RuntimePort, used as a fallback
//     while the controller has not yet stamped the URL annotation.
//
// Returns an empty string when none of the sources is usable (e.g. the pod has not been scheduled
// yet). Callers must treat an empty result as "not ready" and either skip or retry.
func GetRuntimeURL(sbx *agentsv1alpha1.Sandbox) string {
	if sbx == nil {
		return ""
	}
	annotations := sbx.GetAnnotations()
	if u := annotations[agentsv1alpha1.AnnotationRuntimeURL]; u != "" {
		return u
	}
	if u := annotations[agentsv1alpha1.AnnotationEnvdURL]; u != "" { // legacy
		return u
	}
	ip := sbx.Status.PodInfo.PodIP
	if ip == "" {
		return ""
	}
	return fmt.Sprintf("http://%s:%d", ip, utils.RuntimePort)
}
