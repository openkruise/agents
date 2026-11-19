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

package sandboxcr

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

// stampAndMarkReady replays what the sandbox controller does between the
// manager's create and the manager's wait-ready gate: it stamps the runtime
// TLS capability (a meta patch issued just before the pod is created, see
// PodControl.stampRuntimeTLSAnnotation) and then reports the sandbox Running
// and Ready. Both writes bump the resource version, so a manager holding the
// pre-stamp snapshot only observes them through a refresh.
func stampAndMarkReady(t *testing.T, c client.Client, sbx *v1alpha1.Sandbox, tlsPort string) {
	t.Helper()
	live := &v1alpha1.Sandbox{}
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(sbx), live))
	if tlsPort != "" {
		if live.Annotations == nil {
			live.Annotations = map[string]string{}
		}
		live.Annotations[v1alpha1.AnnotationRuntimeTLSPort] = tlsPort
		require.NoError(t, c.Update(t.Context(), live))
	}
	live.Status = v1alpha1.SandboxStatus{
		Phase:              v1alpha1.SandboxRunning,
		ObservedGeneration: live.Generation,
		Conditions: []metav1.Condition{
			{
				Type:               string(v1alpha1.SandboxConditionReady),
				Status:             metav1.ConditionTrue,
				Reason:             "PodReady",
				LastTransitionTime: metav1.Now(),
			},
		},
		PodInfo: v1alpha1.PodInfo{PodIP: "1.2.3.4"},
	}
	require.NoError(t, c.Status().Update(t.Context(), live))
}

// TestCloneSandbox_ResolvesTransportAfterWaitReady is the clone-path counterpart
// of TestRunClaimPostProcesses_ResolvesTransportAfterWaitReady.
//
// A checkpoint carries neither the runtime TLS port annotation nor any other
// pod-derived capability, so a freshly cloned sandbox only gets the stamp when
// the controller writes it while creating the pod. The transport must therefore
// be resolved after cloneWaitSandboxReady has refreshed the object; resolving it
// earlier reads the pre-stamp object and silently selects plaintext.
//
// The stamp is applied from inside the DefaultCreateSandbox hook, right after the
// sandbox is persisted, so the object returned to the clone flow is the pre-stamp
// revision while the cluster already holds the stamped one — exactly the split
// the real controller produces.
func TestCloneSandbox_ResolvesTransportAfterWaitReady(t *testing.T) {
	tests := []struct {
		name           string
		stampedTLSPort string
		wantErrPart    string
	}{
		{
			name:           "stamp visible only after refresh surfaces the missing-bundle configuration error",
			stampedTLSPort: "49984",
			wantErrPart:    "no client TLS bundle is configured",
		},
		{
			name:           "unstamped clone resolves to plaintext and completes",
			stampedTLSPort: "",
			wantErrPart:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setFastCreateRetryForTest(t)
			testInfra, fc := NewTestInfra(t, config.SandboxManagerOptions{
				MaxClaimWorkers: 1,
				MaxCreateQPS:    1000,
			})
			require.NotNil(t, testInfra)
			// The Infra owns the bundle and injects it into the clone options;
			// leaving it nil is what turns a stamped sandbox into a
			// configuration error instead of a silent plaintext downgrade.
			require.Nil(t, testInfra.RuntimeTLSBundle)

			checkpointID := "clone-transport-order"
			createCloneTestCheckpoint(t, fc, testInfra.Cache, checkpointID)

			origCreateSandbox := DefaultCreateSandbox
			t.Cleanup(func() { DefaultCreateSandbox = origCreateSandbox })
			DefaultCreateSandbox = func(ctx context.Context, sbx *v1alpha1.Sandbox, c client.Client) (*v1alpha1.Sandbox, error) {
				created, err := origCreateSandbox(ctx, sbx, c)
				if err != nil {
					return nil, err
				}
				// Replay the controller: stamp the capability and report Ready,
				// both bumping the resource version. The pre-stamp object below
				// is what the clone flow keeps working with.
				preStamp := created.DeepCopy()
				stampAndMarkReady(t, c, created, tt.stampedTLSPort)
				return preStamp, nil
			}

			opts, err := ValidateAndInitCloneOptions(infra.CloneSandboxOptions{
				User:             "test-user",
				CheckPointID:     checkpointID,
				WaitReadyTimeout: 5 * time.Second,
				CloneTimeout:     30 * time.Second,
			})
			require.NoError(t, err)

			cloned, _, err := testInfra.CloneSandbox(t.Context(), opts)

			if tt.wantErrPart != "" {
				require.Error(t, err, "resolution must run after the refresh and reject the unusable stamp")
				assert.Contains(t, err.Error(), tt.wantErrPart)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, cloned)
			assert.Empty(t, cloned.(*Sandbox).Annotations[v1alpha1.AnnotationRuntimeTLSPort])
		})
	}
}
