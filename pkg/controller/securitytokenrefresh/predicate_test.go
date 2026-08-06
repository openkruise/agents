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

package securitytokenrefresh

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
)

// newSandbox builds a *Sandbox with the given knobs.
func newSandbox(name string, claimed bool, tokenStatus string, deleted bool) *agentsv1alpha1.Sandbox {
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "default",
			Labels:      map[string]string{},
			Annotations: map[string]string{},
		},
	}
	if claimed {
		sbx.Labels[agentsv1alpha1.LabelSandboxIsClaimed] = agentsv1alpha1.True
	}
	if tokenStatus != "" {
		sbx.Annotations[identity.AgentKeyTokenRefreshStatus] = tokenStatus
	}
	if deleted {
		now := metav1.Now()
		sbx.DeletionTimestamp = &now
		sbx.Finalizers = []string{"keep"}
	}
	return sbx
}

func TestIsRefreshTarget(t *testing.T) {
	tests := []struct {
		name string
		obj  *agentsv1alpha1.Sandbox
		want bool
	}{
		{
			name: "claimed with token status -> true",
			obj:  newSandbox("a", true, `{"accessTokenExpiration":"2099-01-01T00:00:00Z"}`, false),
			want: true,
		},
		{
			name: "claimed without token status -> false",
			obj:  newSandbox("b", true, "", false),
			want: false,
		},
		{
			name: "unclaimed with token status -> false",
			obj:  newSandbox("c", false, `{"accessTokenExpiration":"2099-01-01T00:00:00Z"}`, false),
			want: false,
		},
		{
			name: "claimed but deleting -> false",
			obj:  newSandbox("d", true, `{"accessTokenExpiration":"2099-01-01T00:00:00Z"}`, true),
			want: false,
		},
		{
			name: "nil object -> false",
			obj:  nil,
			want: false,
		},
		{
			name: "non-true claimed label -> false",
			obj: func() *agentsv1alpha1.Sandbox {
				s := newSandbox("e", false, `{"accessTokenExpiration":"2099-01-01T00:00:00Z"}`, false)
				s.Labels[agentsv1alpha1.LabelSandboxIsClaimed] = "yes" // not exactly "true"
				return s
			}(),
			want: false,
		},
		{
			name: "claimed with empty status object -> false",
			obj:  newSandbox("f", true, `{}`, false),
			want: false,
		},
		{
			name: "claimed with status missing expiration -> false",
			obj:  newSandbox("g", true, `{"otherField":"x"}`, false),
			want: false,
		},
		{
			name: "claimed with malformed status -> true (let reconciler surface event)",
			obj:  newSandbox("h", true, `not-json`, false),
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got bool
			if tt.obj == nil {
				got = isRefreshTarget(nil)
			} else {
				got = isRefreshTarget(tt.obj)
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNeedsRefreshPredicate(t *testing.T) {
	const sampleStatus = `{"accessTokenExpiration":"2099-01-01T00:00:00Z"}`
	const otherStatus = `{"accessTokenExpiration":"2098-01-01T00:00:00Z"}`

	target := newSandbox("a", true, sampleStatus, false)
	nonTarget := newSandbox("b", false, sampleStatus, false)
	deleting := newSandbox("c", true, sampleStatus, true)
	updatedAnnotation := newSandbox("a", true, otherStatus, false)
	unrelatedUpdate := newSandbox("a", true, sampleStatus, false)
	unrelatedUpdate.Spec.Paused = true // a non-annotation mutation

	// notServing/serving pair share the same (unchanged) annotation so only the
	// serving-runtime transition can drive the predicate decision. A serving
	// runtime requires Phase==Running; the RuntimeInitialized condition gates
	// only resume/recreate re-init (absent or True == serving). Here notServing
	// leaves Phase unset so it is not serving, while serving sets Phase==Running
	// and RuntimeInitialized==True. The gate is intentionally token-independent
	// (never the aggregate Ready condition).
	notServing := newSandbox("a", true, sampleStatus, false)
	serving := newSandbox("a", true, sampleStatus, false)
	serving.Status.Phase = agentsv1alpha1.SandboxRunning
	serving.Status.Conditions = []metav1.Condition{{
		Type:   string(agentsv1alpha1.RuntimeInitialized),
		Status: metav1.ConditionTrue,
	}}

	p := needsRefreshPredicate()

	t.Run("create on refresh target", func(t *testing.T) {
		assert.True(t, p.Create(event.CreateEvent{Object: target}))
	})
	t.Run("create on non-target", func(t *testing.T) {
		assert.False(t, p.Create(event.CreateEvent{Object: nonTarget}))
	})
	t.Run("delete is always dropped", func(t *testing.T) {
		assert.False(t, p.Delete(event.DeleteEvent{Object: target}))
	})
	t.Run("generic on refresh target", func(t *testing.T) {
		assert.True(t, p.Generic(event.GenericEvent{Object: target}))
	})
	t.Run("update with annotation change", func(t *testing.T) {
		assert.True(t, p.Update(event.UpdateEvent{ObjectOld: target, ObjectNew: updatedAnnotation}))
	})
	t.Run("update without annotation change", func(t *testing.T) {
		assert.False(t, p.Update(event.UpdateEvent{ObjectOld: target, ObjectNew: unrelatedUpdate}))
	})
	t.Run("update onto deleting sandbox", func(t *testing.T) {
		assert.False(t, p.Update(event.UpdateEvent{ObjectOld: target, ObjectNew: deleting}))
	})
	t.Run("gain serving runtime (none -> serving) with unchanged annotation re-enqueues", func(t *testing.T) {
		assert.True(t, p.Update(event.UpdateEvent{ObjectOld: notServing, ObjectNew: serving}))
	})
	t.Run("lose serving runtime (serving -> none) with unchanged annotation is dropped", func(t *testing.T) {
		assert.False(t, p.Update(event.UpdateEvent{ObjectOld: serving, ObjectNew: notServing}))
	})
}

// sampleServingStatus is a valid token-status annotation reused by the
// hasServingRuntime table. The annotation content is irrelevant to the gate
// (which looks only at Phase and the RuntimeInitialized condition) but keeps
// the helper sandboxes shaped like real refresh targets.
const sampleServingStatus = `{"accessTokenExpiration":"2099-01-01T00:00:00Z"}`

// TestHasServingRuntime pins down the token-independent serving gate. The key
// regression it guards is the freshly-claimed case: a Running sandbox whose
// RuntimeInitialized condition is ABSENT (the condition is only ever written
// during a resume/recreate re-init cycle) must still count as serving, so its
// token refresh is not deferred forever.
func TestHasServingRuntime(t *testing.T) {
	// withInitCond returns a claimed sandbox at the given phase carrying a
	// RuntimeInitialized condition with the given status.
	withInitCond := func(phase agentsv1alpha1.SandboxPhase, status metav1.ConditionStatus) *agentsv1alpha1.Sandbox {
		s := newSandbox("a", true, sampleServingStatus, false)
		s.Status.Phase = phase
		s.Status.Conditions = []metav1.Condition{{
			Type:   string(agentsv1alpha1.RuntimeInitialized),
			Status: status,
		}}
		return s
	}
	// runningNoCond is a Running sandbox with no RuntimeInitialized condition,
	// mirroring a freshly-claimed sandbox that never went through a
	// resume/recreate re-init cycle.
	runningNoCond := newSandbox("a", true, sampleServingStatus, false)
	runningNoCond.Status.Phase = agentsv1alpha1.SandboxRunning

	tests := []struct {
		name string
		obj  client.Object
		want bool
	}{
		{
			name: "running with RuntimeInitialized True -> serving",
			obj:  withInitCond(agentsv1alpha1.SandboxRunning, metav1.ConditionTrue),
			want: true,
		},
		{
			name: "running with RuntimeInitialized condition absent -> serving (freshly claimed)",
			obj:  runningNoCond,
			want: true,
		},
		{
			name: "running with RuntimeInitialized False (resume re-init in progress) -> not serving",
			obj:  withInitCond(agentsv1alpha1.SandboxRunning, metav1.ConditionFalse),
			want: false,
		},
		{
			name: "paused phase -> not serving even with RuntimeInitialized True",
			obj:  withInitCond(agentsv1alpha1.SandboxPaused, metav1.ConditionTrue),
			want: false,
		},
		{
			name: "resuming phase -> not serving",
			obj:  withInitCond(agentsv1alpha1.SandboxResuming, metav1.ConditionFalse),
			want: false,
		},
		{
			name: "empty phase (pending/creating) -> not serving",
			obj:  newSandbox("a", true, sampleServingStatus, false),
			want: false,
		},
		{
			name: "non-Sandbox object -> not serving",
			obj:  &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, hasServingRuntime(tt.obj))
		})
	}
}

func TestTokenStatusAnnotation(t *testing.T) {
	tests := []struct {
		name string
		obj  *corev1.Pod
		want string
	}{
		{name: "nil object", obj: nil, want: ""},
		{
			name: "missing annotation",
			obj:  &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}},
			want: "",
		},
		{
			name: "annotation present",
			obj: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name:        "p",
				Annotations: map[string]string{identity.AgentKeyTokenRefreshStatus: "raw"},
			}},
			want: "raw",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.obj == nil {
				assert.Equal(t, tt.want, tokenStatusAnnotation(nil))
				return
			}
			assert.Equal(t, tt.want, tokenStatusAnnotation(tt.obj))
		})
	}
}
