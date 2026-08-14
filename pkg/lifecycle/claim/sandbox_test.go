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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/openkruise/agents/api/v1alpha1"
	infracache "github.com/openkruise/agents/pkg/cache"
)

func TestSetMemoryPersistence(t *testing.T) {
	tests := []struct {
		name     string
		contents []string
		enabled  *bool
		want     []string
	}{
		{
			name:     "nil enabled leaves contents untouched",
			contents: []string{v1alpha1.PersistentContentFilesystem},
			enabled:  nil,
			want:     []string{v1alpha1.PersistentContentFilesystem},
		},
		{
			name:     "enable on empty adds memory",
			contents: nil,
			enabled:  ptr.To(true),
			want:     []string{v1alpha1.PersistentContentMemory},
		},
		{
			name:     "enable keeps existing memory without duplicating",
			contents: []string{v1alpha1.PersistentContentMemory},
			enabled:  ptr.To(true),
			want:     []string{v1alpha1.PersistentContentMemory},
		},
		{
			name:     "enable collapses duplicate memory entries",
			contents: []string{v1alpha1.PersistentContentMemory, v1alpha1.PersistentContentMemory},
			enabled:  ptr.To(true),
			want:     []string{v1alpha1.PersistentContentMemory},
		},
		{
			name:     "enable preserves other contents and adds memory",
			contents: []string{v1alpha1.PersistentContentFilesystem, v1alpha1.PersistentContentIp},
			enabled:  ptr.To(true),
			want:     []string{v1alpha1.PersistentContentFilesystem, v1alpha1.PersistentContentIp, v1alpha1.PersistentContentMemory},
		},
		{
			name:     "disable removes memory and keeps other contents",
			contents: []string{v1alpha1.PersistentContentMemory, v1alpha1.PersistentContentFilesystem},
			enabled:  ptr.To(false),
			want:     []string{v1alpha1.PersistentContentFilesystem},
		},
		{
			name:     "disable removes every memory entry",
			contents: []string{v1alpha1.PersistentContentMemory, v1alpha1.PersistentContentMemory, v1alpha1.PersistentContentIp},
			enabled:  ptr.To(false),
			want:     []string{v1alpha1.PersistentContentIp},
		},
		{
			name:     "disable without memory is a no-op on contents",
			contents: []string{v1alpha1.PersistentContentFilesystem},
			enabled:  ptr.To(false),
			want:     []string{v1alpha1.PersistentContentFilesystem},
		},
		{
			name:     "disable on empty stays empty",
			contents: nil,
			enabled:  ptr.To(false),
			want:     []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := &v1alpha1.Sandbox{}
			sbx.Spec.PersistentContents = tt.contents
			SetMemoryPersistence(sbx, tt.enabled)
			assert.ElementsMatch(t, tt.want, sbx.Spec.PersistentContents)
		})
	}
}

func TestEnableWakeOnIngressTraffic(t *testing.T) {
	tests := []struct {
		name         string
		initial      *v1alpha1.AutoPausePolicy
		pauseTimeout time.Duration
		wantTimeout  *time.Duration
	}{
		{
			name:         "nil policy with positive timeout arms rule with pause timeout",
			initial:      nil,
			pauseTimeout: 10 * time.Minute,
			wantTimeout:  ptr.To(10 * time.Minute),
		},
		{
			name:         "nil policy with zero timeout arms rule without pause timeout",
			initial:      nil,
			pauseTimeout: 0,
			wantTimeout:  nil,
		},
		{
			name:         "nil policy with negative timeout arms rule without pause timeout",
			initial:      nil,
			pauseTimeout: -time.Minute,
			wantTimeout:  nil,
		},
		{
			name: "existing pause policy is preserved",
			initial: &v1alpha1.AutoPausePolicy{
				Pause: &v1alpha1.PausePolicy{WhenProbedIdleState: &v1alpha1.ProbedIdleStateRule{Probe: "p", MessageRegex: "idle"}},
			},
			pauseTimeout: 5 * time.Minute,
			wantTimeout:  ptr.To(5 * time.Minute),
		},
		{
			name: "existing sibling resume rule is preserved",
			initial: &v1alpha1.AutoPausePolicy{
				Resume: &v1alpha1.ResumePolicy{WhenProbedScheduleTime: &v1alpha1.ProbedScheduleTimeRule{}},
			},
			pauseTimeout: 0,
			wantTimeout:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := &v1alpha1.Sandbox{}
			// DeepCopy so production mutation cannot corrupt the expected
			// fixture derived from tt.initial below.
			sbx.Spec.AutoPausePolicy = tt.initial.DeepCopy()
			EnableWakeOnIngressTraffic(sbx, tt.pauseTimeout)

			policy := sbx.Spec.AutoPausePolicy
			assert.NotNil(t, policy)
			assert.NotNil(t, policy.Resume)
			rule := policy.Resume.OnIngressTraffic
			assert.NotNil(t, rule)
			if tt.wantTimeout == nil {
				assert.Nil(t, rule.PauseTimeout)
			} else {
				assert.NotNil(t, rule.PauseTimeout)
				assert.Equal(t, *tt.wantTimeout, rule.PauseTimeout.Duration)
			}
			if tt.initial != nil {
				assert.Equal(t, tt.initial.Pause, policy.Pause, "pause policy must be preserved")
				if tt.initial.Resume != nil {
					assert.Equal(t, tt.initial.Resume.WhenProbedScheduleTime, policy.Resume.WhenProbedScheduleTime,
						"sibling resume rule must be preserved")
				}
			}
		})
	}
}

func TestClearWakeOnIngressTraffic(t *testing.T) {
	idlePause := &v1alpha1.PausePolicy{WhenProbedIdleState: &v1alpha1.ProbedIdleStateRule{Probe: "p", MessageRegex: "idle"}}
	scheduleResume := &v1alpha1.ResumePolicy{WhenProbedScheduleTime: &v1alpha1.ProbedScheduleTimeRule{}}

	tests := []struct {
		name       string
		initial    *v1alpha1.AutoPausePolicy
		wantPolicy *v1alpha1.AutoPausePolicy
	}{
		{
			name:       "nil policy stays nil",
			initial:    nil,
			wantPolicy: nil,
		},
		{
			name:       "policy without resume stays unchanged",
			initial:    &v1alpha1.AutoPausePolicy{Pause: idlePause},
			wantPolicy: &v1alpha1.AutoPausePolicy{Pause: idlePause},
		},
		{
			name:       "ingress-only resume is pruned back to nil policy",
			initial:    &v1alpha1.AutoPausePolicy{Resume: &v1alpha1.ResumePolicy{OnIngressTraffic: &v1alpha1.IngressTrafficRule{}}},
			wantPolicy: nil,
		},
		{
			name:       "ingress rule removed but pause policy preserved",
			initial:    &v1alpha1.AutoPausePolicy{Pause: idlePause, Resume: &v1alpha1.ResumePolicy{OnIngressTraffic: &v1alpha1.IngressTrafficRule{}}},
			wantPolicy: &v1alpha1.AutoPausePolicy{Pause: idlePause},
		},
		{
			name:       "ingress rule removed but sibling resume rule preserved",
			initial:    &v1alpha1.AutoPausePolicy{Resume: &v1alpha1.ResumePolicy{WhenProbedScheduleTime: scheduleResume.WhenProbedScheduleTime, OnIngressTraffic: &v1alpha1.IngressTrafficRule{}}},
			wantPolicy: &v1alpha1.AutoPausePolicy{Resume: scheduleResume},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := &v1alpha1.Sandbox{}
			sbx.Spec.AutoPausePolicy = tt.initial.DeepCopy()
			ClearWakeOnIngressTraffic(sbx)
			assert.Equal(t, tt.wantPolicy, sbx.Spec.AutoPausePolicy)
		})
	}
}

func TestWakeOnIngressTrafficRoundTripPrunesToFreshSpec(t *testing.T) {
	fresh := &v1alpha1.Sandbox{}
	sbx := &v1alpha1.Sandbox{}
	EnableWakeOnIngressTraffic(sbx, 10*time.Minute)
	assert.NotNil(t, sbx.Spec.AutoPausePolicy, "arming must create the policy")
	ClearWakeOnIngressTraffic(sbx)
	assert.Equal(t, fresh.Spec.AutoPausePolicy, sbx.Spec.AutoPausePolicy,
		"arm then clear must leave a pooled spec byte-identical to a fresh one")
}

func TestSandbox_RefreshFuncKeepsCachedFieldsReadOnly(t *testing.T) {
	cached := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "runtime-refresh-readonly",
			Namespace:       "default",
			UID:             "runtime-refresh-readonly",
			ResourceVersion: "2",
			Annotations:     map[string]string{"token": "old"},
		},
		Spec: v1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"identity": "old"}},
				},
			},
		},
	}
	before := cached.DeepCopy()
	scheme := k8sruntime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	fc := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
			// Match UnsafeDisableDeepCopy: Get copies the top-level struct,
			// while nested maps and the pod template still belong to the cache.
			*obj.(*v1alpha1.Sandbox) = *cached
			return nil
		},
	}).Build()
	initial := cached.DeepCopy()
	initial.ResourceVersion = "1"
	s := NewSandbox(initial, &apiReaderOverrideCache{client: fc})

	refreshed, err := s.RefreshFunc()(t.Context())
	require.NoError(t, err)
	require.Equal(t, "2", refreshed.ResourceVersion)
	require.Equal(t, cached, refreshed)
	refreshed.Annotations["token"] = "fresh"
	refreshed.Spec.Template.Labels["identity"] = "fresh"
	assert.Equal(t, before, cached, "runtime post-processing must not mutate informer fields")
}

// TestSandbox_IncarnationBinding covers the window in #792: authorization
// happens against one Sandbox, but the re-reads resolve by namespace and name,
// so a same-name Sandbox created in between must not be adopted or mutated in
// place of the authorized one.
func TestSandbox_IncarnationBinding(t *testing.T) {
	newSandbox := func(uid types.UID, resourceVersion string) *v1alpha1.Sandbox {
		return &v1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "demo",
				Namespace:       "team-a",
				UID:             uid,
				ResourceVersion: resourceVersion,
			},
		}
	}

	t.Run("assertSameIncarnation", func(t *testing.T) {
		tests := []struct {
			name        string
			want, got   *v1alpha1.Sandbox
			expectError bool
		}{
			{name: "same UID passes", want: newSandbox("uid-a", "1"), got: newSandbox("uid-a", "2")},
			{name: "replacement is rejected", want: newSandbox("uid-a", "1"), got: newSandbox("uid-b", "1"), expectError: true},
			{name: "synthetic caller object is skipped", want: newSandbox("", "1"), got: newSandbox("uid-b", "1")},
			{name: "synthetic read object is skipped", want: newSandbox("uid-a", "1"), got: newSandbox("", "1")},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				err := assertSameIncarnation(tt.want, tt.got)
				if !tt.expectError {
					assert.NoError(t, err)
					return
				}
				require.Error(t, err)
				// NotFound so the API layer reports the authorized Sandbox as
				// gone rather than acting on its replacement.
				assert.True(t, apierrors.IsNotFound(err), "expected NotFound, got %v", err)
			})
		}
	})

	// Each re-read by name, against a store where the name now belongs to a
	// replacement: the authorized Sandbox is reported gone, the replacement is
	// neither adopted nor written, and the same calls still work for the
	// incarnation that is really there.
	readers := []struct {
		name string
		read func(c infracache.Provider, cur *v1alpha1.Sandbox) (*v1alpha1.Sandbox, error)
	}{
		{name: "InplaceRefresh", read: func(c infracache.Provider, cur *v1alpha1.Sandbox) (*v1alpha1.Sandbox, error) {
			return InplaceRefresh(t.Context(), c, cur, true)
		}},
		{name: "RefreshFromAPIReader", read: func(c infracache.Provider, cur *v1alpha1.Sandbox) (*v1alpha1.Sandbox, error) {
			return RefreshFromAPIReader(t.Context(), c, cur)
		}},
		{name: "RetryUpdate", read: func(c infracache.Provider, cur *v1alpha1.Sandbox) (*v1alpha1.Sandbox, error) {
			got, _, err := RetryUpdate(t.Context(), c, cur, func(sbx *v1alpha1.Sandbox) (bool, error) {
				if sbx.Annotations == nil {
					sbx.Annotations = map[string]string{}
				}
				sbx.Annotations["touched"] = "true"
				return true, nil
			})
			return got, err
		}},
	}
	for _, reader := range readers {
		t.Run(reader.name+" refuses a same-name replacement", func(t *testing.T) {
			scheme := k8sruntime.NewScheme()
			require.NoError(t, v1alpha1.AddToScheme(scheme))
			replacement := newSandbox("uid-b", "")
			fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(replacement).Build()
			c := &apiReaderOverrideCache{client: fc, apiReader: fc}
			stored := &v1alpha1.Sandbox{}
			require.NoError(t, fc.Get(t.Context(), client.ObjectKeyFromObject(replacement), stored))

			authorized := newSandbox("uid-a", "1")
			got, err := reader.read(c, authorized)
			assert.True(t, apierrors.IsNotFound(err), "expected NotFound, got %v", err)
			assert.Same(t, authorized, got, "the replacement was adopted")

			after := &v1alpha1.Sandbox{}
			require.NoError(t, fc.Get(t.Context(), client.ObjectKeyFromObject(replacement), after))
			assert.Equal(t, stored.ResourceVersion, after.ResourceVersion, "the replacement was written")

			// The incarnation really there is still read, and written.
			got, err = reader.read(c, stored.DeepCopy())
			require.NoError(t, err)
			assert.Equal(t, types.UID("uid-b"), got.UID)
		})
	}

	// The API server enforces a UID precondition on delete; the fake client
	// does not, so what is checked here is that the precondition is sent.
	t.Run("delete carries a UID precondition", func(t *testing.T) {
		scheme := k8sruntime.NewScheme()
		require.NoError(t, v1alpha1.AddToScheme(scheme))
		var gotPreconditionUID *types.UID
		fc := fake.NewClientBuilder().WithScheme(scheme).
			WithInterceptorFuncs(interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					options := &client.DeleteOptions{}
					for _, opt := range opts {
						opt.ApplyToDelete(options)
					}
					gotPreconditionUID = nil
					if options.Preconditions != nil {
						gotPreconditionUID = options.Preconditions.UID
					}
					return nil
				},
			}).Build()

		require.NoError(t, deleteSandbox(t.Context(), newSandbox("uid-a", "1"), fc))
		require.NotNil(t, gotPreconditionUID, "the delete was not bound to a UID")
		assert.Equal(t, types.UID("uid-a"), *gotPreconditionUID)

		require.NoError(t, deleteSandbox(t.Context(), newSandbox("", "1"), fc))
		assert.Nil(t, gotPreconditionUID, "a synthetic object sends no precondition")
	})
}
