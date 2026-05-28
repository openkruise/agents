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
	"sigs.k8s.io/controller-runtime/pkg/event"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
)

// newSandbox builds a *Sandbox with the given knobs. It mirrors the shape of the
// test helpers used in pkg/controller/bypass-sandbox so the two suites stay
// stylistically aligned.
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
