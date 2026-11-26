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

package claim

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/tracing"
)

// GetSandboxCondition returns the condition of the given type, or a zero
// condition when absent.
func GetSandboxCondition(sbx *v1alpha1.Sandbox, tp v1alpha1.SandboxConditionType) metav1.Condition {
	for _, condition := range sbx.Status.Conditions {
		if condition.Type == string(tp) {
			return condition
		}
	}
	return metav1.Condition{}
}

// ControlPlaneSourceOrDefault normalizes the control plane that handles a sandbox
// into a v1alpha1.LabelSandboxSource value. agent-sandbox-controller is the
// mandatory layer and sandbox-manager an optional one above it, so anything other
// than an explicit sandbox-manager source belongs to the controller.
func ControlPlaneSourceOrDefault(source string) string {
	if source == v1alpha1.SandboxSourceE2B {
		return v1alpha1.SandboxSourceE2B
	}
	return v1alpha1.SandboxSourceK8s
}

// BuildUserMetadataKeys records the user-specified label/annotation keys added
// during a claim, excluding the manager-owned sandbox ID and claim-method
// labels. It returns nil when there is nothing to record.
func BuildUserMetadataKeys(labels, annotations map[string]string) *v1alpha1.UpdatedMetadataInClaim {
	if len(labels) == 0 && len(annotations) == 0 {
		return nil
	}
	keys := &v1alpha1.UpdatedMetadataInClaim{}
	for k := range labels {
		if k == v1alpha1.LabelSandboxID || k == v1alpha1.LabelSandboxClaimMethod || k == v1alpha1.LabelSandboxSource {
			continue
		}
		keys.Labels = append(keys.Labels, k)
	}
	for k := range annotations {
		keys.Annotations = append(keys.Annotations, k)
	}
	if len(keys.Labels) == 0 && len(keys.Annotations) == 0 {
		return nil
	}
	return keys
}

// errSandboxCreateNotAttempted marks a create that was aborted before it was
// issued (context already done), so the caller can release admission.
var errSandboxCreateNotAttempted = fmt.Errorf("sandbox create not attempted")

// DefaultCreateSandbox is the create hook used to lock a newly built sandbox.
// Tests override it to intercept creation.
var DefaultCreateSandbox = CreateSandbox

// CreateSandbox injects trace context into annotations before creating so the
// sandbox-controller can establish a parent-child span relationship. It is
// exported so the sandbox-manager clone path shares one create hook.
func CreateSandbox(ctx context.Context, sbx *v1alpha1.Sandbox, c client.Client) (*v1alpha1.Sandbox, error) {
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %w", errSandboxCreateNotAttempted, ctx.Err())
	default:
	}
	sbx.Annotations = tracing.InjectTraceContext(ctx, sbx.Annotations)
	if err := c.Create(ctx, sbx); err != nil {
		return nil, err
	}
	return sbx, nil
}
