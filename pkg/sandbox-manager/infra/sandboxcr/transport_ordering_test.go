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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

// newPendingSandboxForTransport builds a freshly created sandbox: no runtime
// TLS stamp and not ready yet. It is the state the manager observes right
// after its own create, before the controller reconciles the pod.
func newPendingSandboxForTransport(name string) *v1alpha1.Sandbox {
	return &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "default",
			Annotations: map[string]string{},
		},
		Spec: v1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "main", Image: "test-image"}},
					},
				},
			},
		},
	}
}

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

// TestRunClaimPostProcesses_ResolvesTransportAfterWaitReady locks the ordering
// invariant between the wait-ready gate and the runtime transport resolution.
//
// The controller stamps AnnotationRuntimeTLSPort onto the sandbox immediately
// before it creates the pod (see PodControl.stampRuntimeTLSAnnotation), so the
// capability is only visible to the manager once waitForSandboxReady has
// refreshed the in-memory object. Resolving the transport any earlier reads a
// pre-stamp snapshot and silently selects plaintext for a TLS-only runtime.
//
// The ordering is asserted through the observable behaviour of
// TransportOptionsFor: a stamped sandbox combined with a missing client TLS
// bundle is a configuration error, and that error can only surface when the
// resolution runs after the refresh. Both cases leave InitRuntime and CSIMount
// nil, so no runtime HTTP call is made.
func TestRunClaimPostProcesses_ResolvesTransportAfterWaitReady(t *testing.T) {
	tests := []struct {
		name string
		// stampedTLSPort is what the controller stamps onto the sandbox after
		// the manager already holds its pre-stamp snapshot.
		stampedTLSPort string
		wantErr        bool
		wantErrPart    string
	}{
		{
			name:           "stamp visible only after refresh surfaces the missing-bundle configuration error",
			stampedTLSPort: "49984",
			wantErr:        true,
			wantErrPart:    "no client TLS bundle is configured",
		},
		{
			name:           "unstamped sandbox resolves to plaintext and proceeds",
			stampedTLSPort: "",
			wantErr:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testInfra, fc := NewTestInfra(t, config.SandboxManagerOptions{
				MaxClaimWorkers:            1,
				MaxCreateQPS:               1000,
				DisableRouteReconciliation: true,
			})
			require.NotNil(t, testInfra)

			created := newPendingSandboxForTransport("claim-transport-order")
			require.NoError(t, fc.Create(t.Context(), created))
			// The snapshot the manager carries into post-processing: taken before
			// the controller stamps anything, hence an older resource version.
			snapshot := created.DeepCopy()

			stampAndMarkReady(t, fc, created, tt.stampedTLSPort)
			require.Eventually(t, func() bool {
				var got v1alpha1.Sandbox
				if err := testInfra.Cache.GetClient().Get(t.Context(), types.NamespacedName{
					Namespace: created.Namespace, Name: created.Name,
				}, &got); err != nil {
					return false
				}
				return got.Status.Phase == v1alpha1.SandboxRunning &&
					got.Annotations[v1alpha1.AnnotationRuntimeTLSPort] == tt.stampedTLSPort
			}, 2*time.Second, 10*time.Millisecond)

			sbx := &Sandbox{Sandbox: snapshot, Cache: testInfra.Cache}
			require.Empty(t, sbx.Sandbox.Annotations[v1alpha1.AnnotationRuntimeTLSPort],
				"the pre-stamp snapshot must not carry the capability")

			opts := infra.ClaimSandboxOptions{
				Namespace:        created.Namespace,
				WaitReadyTimeout: 10 * time.Second,
				// No bundle configured: a stamped sandbox must be reported as a
				// configuration error rather than silently downgraded.
				RuntimeTLSBundle: nil,
			}
			metrics := &infra.ClaimMetrics{}

			err := runClaimPostProcesses(t.Context(), sbx, infra.LockTypeCreate, opts, testInfra.Cache, metrics)

			if tt.wantErr {
				require.Error(t, err, "resolution must run after the refresh and reject the unusable stamp")
				assert.Contains(t, err.Error(), tt.wantErrPart)
				assert.NotErrorIs(t, err, retriableError{},
					"a missing TLS bundle is a configuration error, not a retriable one")
			} else {
				require.NoError(t, err)
			}

			// The refresh performed by the wait-ready gate is what makes the
			// capability observable; without it the resolution above would have
			// silently picked plaintext.
			assert.Equal(t, tt.stampedTLSPort, sbx.Sandbox.Annotations[v1alpha1.AnnotationRuntimeTLSPort],
				"waitForSandboxReady must refresh the sandbox before the transport is resolved")
		})
	}
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
			setFastCloneRetryForTest(t)
			testInfra, fc := NewTestInfra(t, config.SandboxManagerOptions{
				MaxClaimWorkers:            1,
				MaxCreateQPS:               1000,
				DisableRouteReconciliation: true,
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
