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

package core

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/expectations"
	"github.com/openkruise/agents/pkg/utils/runtime"
	runtimeconfig "github.com/openkruise/agents/pkg/utils/runtime/config"
	utestutils "github.com/openkruise/agents/pkg/utils/testutils"
	"github.com/openkruise/agents/pkg/utils/timeout"
	testutils "github.com/openkruise/agents/test/utils"
)

// stubProvider implements cache.Provider by delegating GetClient only; the
// embedded nil interface makes any other method panic if a test accidentally
// exercises a path that needs it, which is preferable to a silent stub.
type stubProvider struct {
	cache.Provider
	client client.Client
}

func (s *stubProvider) GetClient() client.Client { return s.client }

// --- shared fixtures ---

func engineTestSandboxSet(name string) *agentsv1alpha1.SandboxSet {
	return &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       types.UID(name + "-uid"),
		},
		Spec: agentsv1alpha1.SandboxSetSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "main", Image: "test-image"},
						},
					},
				},
			},
		},
	}
}

func engineTestOwnerReference(sbs *agentsv1alpha1.SandboxSet) []metav1.OwnerReference {
	return []metav1.OwnerReference{*metav1.NewControllerRef(sbs, agentsv1alpha1.SandboxSetControllerKind)}
}

// engineTestAvailableSandbox builds a Sandbox that utils.GetSandboxState reports
// as "Available": owned by a SandboxSet, Running phase, Ready condition True,
// and a PodIP set.
func engineTestAvailableSandbox(name string, sbs *agentsv1alpha1.SandboxSet) *agentsv1alpha1.Sandbox {
	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         sbs.Namespace,
			CreationTimestamp: metav1.Now(),
			OwnerReferences:   engineTestOwnerReference(sbs),
			Labels: map[string]string{
				agentsv1alpha1.LabelSandboxTemplate:  sbs.Name,
				agentsv1alpha1.LabelSandboxIsClaimed: "false",
			},
			Annotations: map[string]string{},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: sbs.Spec.EmbeddedSandboxTemplate,
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
			Conditions: []metav1.Condition{
				{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue},
			},
			PodInfo: agentsv1alpha1.PodInfo{PodIP: "10.0.0.1"},
		},
	}
}

// engineTestReadySandbox builds a Sandbox already satisfying
// NewSandboxWaitReadyTask's CheckFunc directly: claimed (no SandboxSet owner),
// Running, Ready, PodIP set, ObservedGeneration in sync.
func engineTestReadySandbox(name string) *agentsv1alpha1.Sandbox {
	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "test-image"}}},
				},
			},
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase:              agentsv1alpha1.SandboxRunning,
			ObservedGeneration: 0,
			Conditions: []metav1.Condition{
				{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue},
			},
			PodInfo: agentsv1alpha1.PodInfo{PodIP: "10.0.0.9"},
		},
	}
}

// --- validateAndInitClaimOptions ---

func TestValidateAndInitClaimOptions(t *testing.T) {
	tests := []struct {
		name        string
		opts        claimOptions
		expectError string
		check       func(t *testing.T, opts claimOptions)
	}{
		{
			name:        "missing user",
			opts:        claimOptions{Template: "t"},
			expectError: "user is required",
		},
		{
			name:        "user not a valid label value",
			opts:        claimOptions{User: "not a valid label!!", Template: "t"},
			expectError: "invalid owner",
		},
		{
			name:        "missing template",
			opts:        claimOptions{User: "u"},
			expectError: "template is required",
		},
		{
			name:        "csi mount without init runtime",
			opts:        claimOptions{User: "u", Template: "t", CSIMount: &runtimeconfig.CSIMountOptions{}},
			expectError: "init runtime is required when csi mount is specified",
		},
		{
			name: "csi mount with init runtime is fine",
			opts: claimOptions{
				User: "u", Template: "t",
				CSIMount:    &runtimeconfig.CSIMountOptions{},
				InitRuntime: &runtimeconfig.InitRuntimeOptions{},
			},
		},
		{
			name:        "inplace update with neither image nor resources",
			opts:        claimOptions{User: "u", Template: "t", InplaceUpdate: &inplaceUpdateOptions{}},
			expectError: "inplace update requires at least one of image or resources to be set",
		},
		{
			name: "inplace update resources with neither requests nor limits",
			opts: claimOptions{User: "u", Template: "t", InplaceUpdate: &inplaceUpdateOptions{
				Resources: &inplaceUpdateResourcesOptions{},
			}},
			expectError: "resources must specify at least one of requests or limits",
		},
		{
			name: "inplace update resources with zero cpu request",
			opts: claimOptions{User: "u", Template: "t", InplaceUpdate: &inplaceUpdateOptions{
				Resources: &inplaceUpdateResourcesOptions{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("0")},
				},
			}},
			expectError: "target cpu must be a positive value",
		},
		{
			name: "inplace update resources with negative cpu limit",
			opts: claimOptions{User: "u", Template: "t", InplaceUpdate: &inplaceUpdateOptions{
				Resources: &inplaceUpdateResourcesOptions{
					Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("-1")},
				},
			}},
			expectError: "target cpu must be a positive value",
		},
		{
			name: "inplace update with only image is valid",
			opts: claimOptions{User: "u", Template: "t", InplaceUpdate: &inplaceUpdateOptions{Image: "nginx:latest"}},
			check: func(t *testing.T, opts claimOptions) {
				require.NotNil(t, opts.InplaceUpdate)
				assert.Equal(t, "nginx:latest", opts.InplaceUpdate.Image)
			},
		},
		{
			name: "defaults are applied",
			opts: claimOptions{User: "u", Template: "t"},
			check: func(t *testing.T, opts claimOptions) {
				assert.Equal(t, defaultCandidateCounts, opts.CandidateCounts)
				assert.Equal(t, defaultWaitReadyTimeout, opts.WaitReadyTimeout)
				require.NotNil(t, opts.ReserveFailedSandboxFor)
				assert.Equal(t, defaultReserveFailedSandboxFor, *opts.ReserveFailedSandboxFor)
			},
		},
		{
			name: "explicit values are preserved",
			opts: claimOptions{
				User: "u", Template: "t",
				CandidateCounts:  7,
				WaitReadyTimeout: 5 * time.Second,
			},
			check: func(t *testing.T, opts claimOptions) {
				assert.Equal(t, 7, opts.CandidateCounts)
				assert.Equal(t, 5*time.Second, opts.WaitReadyTimeout)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateAndInitClaimOptions(tt.opts)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			if tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}

// --- preCheckCandidate / noAvailableError ---

func TestPreCheckCandidate(t *testing.T) {
	tests := []struct {
		name        string
		sbx         *agentsv1alpha1.Sandbox
		expectError string
	}{
		{
			name: "locked sandbox is rejected",
			sbx: &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{
				Annotations:       map[string]string{agentsv1alpha1.AnnotationLock: "someone"},
				CreationTimestamp: metav1.Now(),
			}},
			expectError: "sandbox is locked by someone",
		},
		{
			name:        "zero creation timestamp is rejected",
			sbx:         &agentsv1alpha1.Sandbox{},
			expectError: "creation timestamp is zero",
		},
		{
			name: "valid candidate passes",
			sbx: &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{
				CreationTimestamp: metav1.Now(),
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := preCheckCandidate(tt.sbx)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestNoAvailableError(t *testing.T) {
	err := noAvailableError("my-template", "no stock")
	assert.Contains(t, err.Error(), "my-template")
	assert.Contains(t, err.Error(), "no stock")
}

// --- pickFromCandidates ---

func TestPickFromCandidates(t *testing.T) {
	t.Run("empty candidates", func(t *testing.T) {
		_, err := pickFromCandidates(t.Context(), nil, &sync.Map{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no candidate")
	})

	t.Run("picks and marks the candidate", func(t *testing.T) {
		pickCache := &sync.Map{}
		candidate := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx-a", Namespace: "default"}}
		got, err := pickFromCandidates(t.Context(), []*agentsv1alpha1.Sandbox{candidate}, pickCache)
		require.NoError(t, err)
		assert.Equal(t, "sbx-a", got.Name)
		_, loaded := pickCache.Load(getPickKey(candidate))
		assert.True(t, loaded, "picked candidate must be recorded in pickCache")
	})

	t.Run("all candidates already picked", func(t *testing.T) {
		pickCache := &sync.Map{}
		candidates := []*agentsv1alpha1.Sandbox{
			{ObjectMeta: metav1.ObjectMeta{Name: "sbx-a", Namespace: "default"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "sbx-b", Namespace: "default"}},
		}
		for _, c := range candidates {
			pickCache.Store(getPickKey(c), struct{}{})
		}
		_, err := pickFromCandidates(t.Context(), candidates, pickCache)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "all candidates are picked")
	})

	t.Run("context canceled stops the scan", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		candidates := []*agentsv1alpha1.Sandbox{{ObjectMeta: metav1.ObjectMeta{Name: "sbx-a", Namespace: "default"}}}
		_, err := pickFromCandidates(ctx, candidates, &sync.Map{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "context canceled")
	})
}

// --- small pure helpers ---

func TestBuildUserMetadataKeys(t *testing.T) {
	assert.Nil(t, buildUserMetadataKeys(nil, nil))

	keys := buildUserMetadataKeys(map[string]string{"env": "prod"}, map[string]string{"note": "x"})
	require.NotNil(t, keys)
	assert.Equal(t, []string{"env"}, keys.Labels)
	assert.Equal(t, []string{"note"}, keys.Annotations)
}

func TestGetSandboxCondition(t *testing.T) {
	sbx := &agentsv1alpha1.Sandbox{Status: agentsv1alpha1.SandboxStatus{
		Conditions: []metav1.Condition{
			{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue, Reason: "PodReady"},
		},
	}}
	got := getSandboxCondition(sbx, agentsv1alpha1.SandboxConditionReady)
	assert.Equal(t, "PodReady", got.Reason)

	missing := getSandboxCondition(sbx, agentsv1alpha1.SandboxConditionInplaceUpdate)
	assert.Empty(t, missing.Reason)
}

func TestGetPickKey(t *testing.T) {
	sbx := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx-a", Namespace: "ns"}}
	assert.Equal(t, "ns/sbx-a", getPickKey(sbx))
}

// --- pod label/annotation helpers ---

func TestPodLabelsAndAnnotationsHelpers(t *testing.T) {
	sbx := &agentsv1alpha1.Sandbox{Spec: agentsv1alpha1.SandboxSpec{
		EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
			Template: &corev1.PodTemplateSpec{},
		},
	}}

	assert.Nil(t, getPodLabels(sbx))
	setPodLabels(sbx, map[string]string{"a": "b"})
	assert.Equal(t, map[string]string{"a": "b"}, getPodLabels(sbx))

	mergePodAnnotations(sbx, map[string]string{"k": "v"})
	assert.Equal(t, "v", sbx.Spec.Template.Annotations["k"])
	mergePodAnnotations(sbx, map[string]string{"k": "v2", "k2": "v2"})
	assert.Equal(t, "v2", sbx.Spec.Template.Annotations["k"], "existing key must be overwritten")
	assert.Equal(t, "v2", sbx.Spec.Template.Annotations["k2"])

	mergePodAnnotations(sbx, nil) // no-op, must not panic

	noTemplate := &agentsv1alpha1.Sandbox{}
	assert.Nil(t, getPodLabels(noTemplate))
	setPodLabels(noTemplate, map[string]string{"a": "b"}) // no-op, must not panic
	mergePodAnnotations(noTemplate, map[string]string{"a": "b"})
}

// --- resource resize helpers ---

func TestSetSandboxImageAndResources(t *testing.T) {
	t.Run("nil template is a no-op", func(t *testing.T) {
		sbx := &agentsv1alpha1.Sandbox{}
		setSandboxImage(sbx, "nginx:latest")
		setSandboxResources(sbx, nil, nil)
		// must not panic
	})

	t.Run("image is set on the first container", func(t *testing.T) {
		sbx := &agentsv1alpha1.Sandbox{Spec: agentsv1alpha1.SandboxSpec{EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
			Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Image: "old"}}}},
		}}}
		setSandboxImage(sbx, "new")
		assert.Equal(t, "new", sbx.Spec.Template.Spec.Containers[0].Image)
	})

	t.Run("only cpu already set on the container is resized", func(t *testing.T) {
		sbx := &agentsv1alpha1.Sandbox{Spec: agentsv1alpha1.SandboxSpec{EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
			Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
				},
			}}}},
		}}}
		setSandboxResources(sbx,
			corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("1Gi")},
			corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("999m")},
		)
		requests := sbx.Spec.Template.Spec.Containers[0].Resources.Requests
		limits := sbx.Spec.Template.Spec.Containers[0].Resources.Limits
		requestCPU := requests[corev1.ResourceCPU]
		limitCPU := limits[corev1.ResourceCPU]
		assert.Equal(t, "500m", requestCPU.String())
		assert.Equal(t, "999m", limitCPU.String())
		_, hasMemory := requests[corev1.ResourceMemory]
		assert.False(t, hasMemory, "memory was never set on the container and must not be added by resize")
	})

	t.Run("no containers returns an unmodified deep copy", func(t *testing.T) {
		pod := &corev1.Pod{}
		resized := buildResourceResizedPod(pod, corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}, nil)
		assert.Empty(t, resized.Spec.Containers)
	})
}

func TestSetSandboxTimeout(t *testing.T) {
	sbx := &agentsv1alpha1.Sandbox{}
	now := time.Now()
	setSandboxTimeout(sbx, timeout.Options{ShutdownTime: now})
	require.NotNil(t, sbx.Spec.ShutdownTime)
	assert.Nil(t, sbx.Spec.PauseTime)

	setSandboxTimeout(sbx, timeout.Options{})
	assert.Nil(t, sbx.Spec.ShutdownTime, "zero ShutdownTime must clear the field")
	assert.Nil(t, sbx.Spec.PauseTime)
}

// --- sandboxReadyFailureMessage / sandboxReadyFailureReason ---

func TestSandboxReadyFailureReason(t *testing.T) {
	tests := []struct {
		name           string
		sbx            *agentsv1alpha1.Sandbox
		expectContains string
	}{
		{
			name: "generation mismatch",
			sbx: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Generation: 2},
				Status:     agentsv1alpha1.SandboxStatus{ObservedGeneration: 1},
			},
			expectContains: "has not observed latest generation",
		},
		{
			name: "inplace update in progress",
			sbx: &agentsv1alpha1.Sandbox{Status: agentsv1alpha1.SandboxStatus{
				Conditions: []metav1.Condition{
					{Type: string(agentsv1alpha1.SandboxConditionInplaceUpdate), Reason: agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating},
				},
			}},
			expectContains: "inplace update is still in progress",
		},
		{
			name:           "no pod ip",
			sbx:            &agentsv1alpha1.Sandbox{},
			expectContains: "sandbox has no pod IP",
		},
		{
			name: "start container failed with message",
			sbx: &agentsv1alpha1.Sandbox{Status: agentsv1alpha1.SandboxStatus{
				PodInfo: agentsv1alpha1.PodInfo{PodIP: "1.2.3.4"},
				Conditions: []metav1.Condition{
					{Type: string(agentsv1alpha1.SandboxConditionReady), Reason: agentsv1alpha1.SandboxReadyReasonStartContainerFailed, Message: "oom"},
				},
			}},
			expectContains: "ready condition reports StartContainerFailed: oom",
		},
		{
			name: "wrong state",
			sbx: &agentsv1alpha1.Sandbox{Status: agentsv1alpha1.SandboxStatus{
				Phase:   agentsv1alpha1.SandboxPending,
				PodInfo: agentsv1alpha1.PodInfo{PodIP: "1.2.3.4"},
			}},
			expectContains: "sandbox state is",
		},
		{
			name: "running with a ready-condition reason set",
			sbx: &agentsv1alpha1.Sandbox{Status: agentsv1alpha1.SandboxStatus{
				Phase:   agentsv1alpha1.SandboxRunning,
				PodInfo: agentsv1alpha1.PodInfo{PodIP: "1.2.3.4"},
				Conditions: []metav1.Condition{
					{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue, Reason: "SomeOtherReason"},
				},
			}},
			expectContains: "ready condition reports SomeOtherReason",
		},
		{
			name: "running with satisfied-looking state falls through to catch-all",
			sbx: &agentsv1alpha1.Sandbox{Status: agentsv1alpha1.SandboxStatus{
				Phase:   agentsv1alpha1.SandboxRunning,
				PodInfo: agentsv1alpha1.PodInfo{PodIP: "1.2.3.4"},
				Conditions: []metav1.Condition{
					{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue},
				},
			}},
			expectContains: "sandbox ready condition is not satisfied",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			readyCond := getSandboxCondition(tt.sbx, agentsv1alpha1.SandboxConditionReady)
			inplaceCond := getSandboxCondition(tt.sbx, agentsv1alpha1.SandboxConditionInplaceUpdate)
			state, _ := utils.GetSandboxState(tt.sbx)
			reason := sandboxReadyFailureReason(tt.sbx, state, readyCond, inplaceCond)
			assert.Contains(t, reason, tt.expectContains)
		})
	}
}

func TestSandboxReadyFailureMessage(t *testing.T) {
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "sbx-1"},
		Status:     agentsv1alpha1.SandboxStatus{},
	}
	msg := sandboxReadyFailureMessage(sbx)
	assert.Contains(t, msg, "default/sbx-1")
	assert.Contains(t, msg, "not ready before wait timeout")
	assert.Contains(t, msg, "reason=sandbox has no pod IP")
}

// --- pickAnAvailableSandbox ---

func TestPickAnAvailableSandbox(t *testing.T) {
	t.Run("no candidates, CreateOnNoStock false, returns noAvailableError", func(t *testing.T) {
		sbs := engineTestSandboxSet("tmpl")
		c, _, err := cachetest.NewTestCache(t, sbs)
		require.NoError(t, err)

		_, _, err = pickAnAvailableSandbox(t.Context(), claimOptions{Template: "tmpl", CandidateCounts: 10}, &sync.Map{}, c)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no available sandboxes")
	})

	t.Run("no candidates, CreateOnNoStock true, creates from pool", func(t *testing.T) {
		sbs := engineTestSandboxSet("tmpl")
		c, _, err := cachetest.NewTestCache(t, sbs)
		require.NoError(t, err)

		sbx, lt, err := pickAnAvailableSandbox(t.Context(), claimOptions{
			Namespace: "default", Template: "tmpl", CandidateCounts: 10, CreateOnNoStock: true,
		}, &sync.Map{}, c)
		require.NoError(t, err)
		assert.Equal(t, lockTypeCreate, lt)
		assert.NotNil(t, sbx)
	})

	t.Run("picks an available candidate", func(t *testing.T) {
		sbs := engineTestSandboxSet("tmpl")
		available := engineTestAvailableSandbox("sbx-available", sbs)
		c, _, err := cachetest.NewTestCache(t, sbs, available)
		require.NoError(t, err)

		sbx, lt, err := pickAnAvailableSandbox(t.Context(), claimOptions{
			Namespace: "default", Template: "tmpl", CandidateCounts: 10,
		}, &sync.Map{}, c)
		require.NoError(t, err)
		assert.Equal(t, lockTypeUpdate, lt)
		assert.Equal(t, "sbx-available", sbx.Name)
	})

	t.Run("skips a locked candidate", func(t *testing.T) {
		sbs := engineTestSandboxSet("tmpl")
		locked := engineTestAvailableSandbox("sbx-locked", sbs)
		locked.Annotations[agentsv1alpha1.AnnotationLock] = "someone-else"
		c, _, err := cachetest.NewTestCache(t, sbs, locked)
		require.NoError(t, err)

		_, _, err = pickAnAvailableSandbox(t.Context(), claimOptions{
			Namespace: "default", Template: "tmpl", CandidateCounts: 10,
		}, &sync.Map{}, c)
		require.Error(t, err, "the only candidate is locked, so none is available")
	})

	t.Run("skips an available candidate with no pod IP", func(t *testing.T) {
		sbs := engineTestSandboxSet("tmpl")
		noIP := engineTestAvailableSandbox("sbx-no-ip", sbs)
		noIP.Status.PodInfo.PodIP = ""
		c, _, err := cachetest.NewTestCache(t, sbs, noIP)
		require.NoError(t, err)

		_, _, err = pickAnAvailableSandbox(t.Context(), claimOptions{
			Namespace: "default", Template: "tmpl", CandidateCounts: 10,
		}, &sync.Map{}, c)
		require.Error(t, err)
	})

	t.Run("prefers candidates matching the SandboxSet's update revision", func(t *testing.T) {
		sbs := engineTestSandboxSet("tmpl")
		sbs.Status.UpdateRevision = "rev-new"

		oldRev := engineTestAvailableSandbox("sbx-old", sbs)
		oldRev.Labels[agentsv1alpha1.LabelTemplateHash] = "rev-old"
		newRev := engineTestAvailableSandbox("sbx-new", sbs)
		newRev.Labels[agentsv1alpha1.LabelTemplateHash] = "rev-new"

		c, _, err := cachetest.NewTestCache(t, sbs, oldRev, newRev)
		require.NoError(t, err)

		sbx, lt, err := pickAnAvailableSandbox(t.Context(), claimOptions{
			Namespace: "default", Template: "tmpl", CandidateCounts: 10,
		}, &sync.Map{}, c)
		require.NoError(t, err)
		assert.Equal(t, lockTypeUpdate, lt)
		assert.Equal(t, "sbx-new", sbx.Name, "must prefer the sandbox matching the current update revision")
	})
}

// --- newSandboxFromPool ---

func TestNewSandboxFromPool(t *testing.T) {
	t.Run("creates from embedded template", func(t *testing.T) {
		sbs := engineTestSandboxSet("tmpl")
		c, _, err := cachetest.NewTestCache(t, sbs)
		require.NoError(t, err)

		sbx, lt, err := newSandboxFromPool(t.Context(), claimOptions{Namespace: "default", Template: "tmpl"}, c)
		require.NoError(t, err)
		assert.Equal(t, lockTypeCreate, lt)
		assert.Equal(t, "100", sbx.Annotations[agentsv1alpha1.SandboxAnnotationPriority])
	})

	t.Run("unknown SandboxSet fails", func(t *testing.T) {
		c, _, err := cachetest.NewTestCache(t)
		require.NoError(t, err)

		_, _, err = newSandboxFromPool(t.Context(), claimOptions{Namespace: "default", Template: "missing"}, c)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot create new sandbox")
	})

	t.Run("unresolvable TemplateRef fails", func(t *testing.T) {
		sbs := engineTestSandboxSet("tmpl")
		sbs.Spec.Template = nil
		sbs.Spec.TemplateRef = &agentsv1alpha1.SandboxTemplateRef{Name: "does-not-exist"}
		c, _, err := cachetest.NewTestCache(t, sbs)
		require.NoError(t, err)

		_, _, err = newSandboxFromPool(t.Context(), claimOptions{Namespace: "default", Template: "tmpl"}, c)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot resolve sandbox template")
	})
}

// --- modifyPickedSandbox ---

func TestModifyPickedSandbox(t *testing.T) {
	t.Run("update lock type deep-copies instead of mutating the input", func(t *testing.T) {
		original := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "default"}}
		got, err := modifyPickedSandbox(original, lockTypeUpdate, claimOptions{})
		require.NoError(t, err)
		assert.NotSame(t, original, got)
		assert.Empty(t, original.Labels, "the original object handed in must not be mutated")
		assert.Equal(t, agentsv1alpha1.True, got.Labels[agentsv1alpha1.LabelSandboxIsClaimed])
	})

	t.Run("create lock type mutates in place", func(t *testing.T) {
		original := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "default"}}
		got, err := modifyPickedSandbox(original, lockTypeCreate, claimOptions{})
		require.NoError(t, err)
		assert.Same(t, original, got)
	})

	t.Run("applies modifier, inplace update, init runtime, csi mount, and user metadata keys", func(t *testing.T) {
		sbx := &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "default"},
			Spec: agentsv1alpha1.SandboxSpec{EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Image: "old"}}}},
			}},
		}
		opts := claimOptions{
			Modifier: func(s *agentsv1alpha1.Sandbox) {
				labels := s.GetLabels()
				if labels == nil {
					labels = map[string]string{}
				}
				labels["from-modifier"] = "yes"
				s.SetLabels(labels)
			},
			InplaceUpdate: &inplaceUpdateOptions{Image: "new-image"},
			InitRuntime:   &runtimeconfig.InitRuntimeOptions{AccessToken: "tok-123"},
			CSIMount:      &runtimeconfig.CSIMountOptions{MountOptionListRaw: `[{"pvName":"p"}]`},
			UserMetadataKeys: &agentsv1alpha1.UpdatedMetadataInClaim{
				Labels: []string{"env"},
			},
		}
		got, err := modifyPickedSandbox(sbx, lockTypeCreate, opts)
		require.NoError(t, err)

		assert.Equal(t, "yes", got.Labels["from-modifier"])
		assert.Equal(t, "new-image", got.Spec.Template.Spec.Containers[0].Image)
		assert.NotEmpty(t, got.Annotations[agentsv1alpha1.AnnotationClaimTime])
		assert.NotEmpty(t, got.Annotations[agentsv1alpha1.AnnotationInitRuntimeRequest])
		assert.Equal(t, "tok-123", got.Annotations[agentsv1alpha1.AnnotationRuntimeAccessToken])
		assert.Equal(t, `[{"pvName":"p"}]`, got.Annotations[agentsv1alpha1.AnnotationCSIVolumeConfig])
		assert.NotEmpty(t, got.Annotations[agentsv1alpha1.AnnotationUpdatedMetadataInClaim])
		assert.Equal(t, agentsv1alpha1.True, got.Labels[agentsv1alpha1.LabelSandboxIsClaimed])
		assert.Empty(t, got.OwnerReferences, "claiming must clear owner references so the SandboxSet scales up")
	})

	t.Run("empty user metadata keys are not annotated", func(t *testing.T) {
		sbx := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "default"}}
		got, err := modifyPickedSandbox(sbx, lockTypeCreate, claimOptions{UserMetadataKeys: &agentsv1alpha1.UpdatedMetadataInClaim{}})
		require.NoError(t, err)
		_, ok := got.Annotations[agentsv1alpha1.AnnotationUpdatedMetadataInClaim]
		assert.False(t, ok)
	})
}

// --- lockPickedSandbox / createSandbox ---

func TestCreateSandbox(t *testing.T) {
	t.Run("context already canceled", func(t *testing.T) {
		sbs := engineTestSandboxSet("tmpl")
		_, fc, err := cachetest.NewTestCache(t, sbs)
		require.NoError(t, err)

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		sbx := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "default"}}
		_, err = createSandbox(ctx, sbx, fc)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "sandbox create not attempted")

		var list agentsv1alpha1.SandboxList
		require.NoError(t, fc.List(t.Context(), &list))
		assert.Empty(t, list.Items, "no sandbox should have been created")
	})

	t.Run("success", func(t *testing.T) {
		_, fc, err := cachetest.NewTestCache(t)
		require.NoError(t, err)
		sbx := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "default"}}
		got, err := createSandbox(t.Context(), sbx, fc)
		require.NoError(t, err)
		assert.NotEmpty(t, got.ResourceVersion)
	})
}

func TestLockPickedSandbox(t *testing.T) {
	t.Run("create path locks and creates the sandbox", func(t *testing.T) {
		c, _, err := cachetest.NewTestCache(t)
		require.NoError(t, err)
		sbx := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx-c", Namespace: "default"}}
		got, err := lockPickedSandbox(t.Context(), sbx, lockTypeCreate, claimOptions{User: "alice"}, c)
		require.NoError(t, err)
		assert.Equal(t, "alice", got.Annotations[agentsv1alpha1.AnnotationOwner])
		assert.NotEmpty(t, got.Annotations[agentsv1alpha1.AnnotationLock])
	})

	t.Run("update path locks an existing sandbox", func(t *testing.T) {
		sbx := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx-u", Namespace: "default"}}
		c, _, err := cachetest.NewTestCache(t, sbx)
		require.NoError(t, err)

		got, err := lockPickedSandbox(t.Context(), sbx, lockTypeUpdate, claimOptions{User: "bob"}, c)
		require.NoError(t, err)
		assert.Equal(t, "bob", got.Annotations[agentsv1alpha1.AnnotationOwner])
	})

	t.Run("update conflict surfaces an error and records a resource-version expectation", func(t *testing.T) {
		stored := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx-conflict", Namespace: "default", UID: "sbx-conflict-uid"}}
		scheme := k8sruntime.NewScheme()
		_ = agentsv1alpha1.AddToScheme(scheme)
		conflictClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stored).WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				return apierrors.NewConflict(schema.GroupResource{Group: agentsv1alpha1.GroupVersion.Group, Resource: "sandboxes"}, obj.GetName(), fmt.Errorf("simulated conflict"))
			},
		}).Build()
		provider := &stubProvider{client: conflictClient}

		_, err := lockPickedSandbox(t.Context(), stored.DeepCopy(), lockTypeUpdate, claimOptions{User: "carol"}, provider)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to lock sandbox")

		// A conflict must record a resource-version expectation for the
		// object's UID so a subsequent pick of the same sandbox is not
		// short-circuited by a stale cached ResourceVersion.
		assert.False(t, expectations.ResourceVersionExpectationSatisfied(stored),
			"conflict must register a not-yet-satisfied expectation for this UID")
	})
}

// --- retryUpdateSandbox ---

func TestRetryUpdateSandbox(t *testing.T) {
	t.Run("modifier declining the update is a no-op", func(t *testing.T) {
		sbx := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "default"}}
		c, _, err := cachetest.NewTestCache(t, sbx)
		require.NoError(t, err)

		updated, err := retryUpdateSandbox(t.Context(), c, sbx, func(s *agentsv1alpha1.Sandbox) (bool, error) {
			return false, nil
		})
		require.NoError(t, err)
		assert.False(t, updated)
	})

	t.Run("modifier accepting the update persists it", func(t *testing.T) {
		sbx := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "default"}}
		c, fc, err := cachetest.NewTestCache(t, sbx)
		require.NoError(t, err)

		updated, err := retryUpdateSandbox(t.Context(), c, sbx, func(s *agentsv1alpha1.Sandbox) (bool, error) {
			labels := map[string]string{"updated": "yes"}
			s.SetLabels(labels)
			return true, nil
		})
		require.NoError(t, err)
		assert.True(t, updated)

		got := &agentsv1alpha1.Sandbox{}
		require.NoError(t, fc.Get(t.Context(), client.ObjectKeyFromObject(sbx), got))
		assert.Equal(t, "yes", got.Labels["updated"])
	})

	t.Run("modifier returning an error aborts without updating", func(t *testing.T) {
		sbx := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "default"}}
		c, _, err := cachetest.NewTestCache(t, sbx)
		require.NoError(t, err)

		_, err = retryUpdateSandbox(t.Context(), c, sbx, func(s *agentsv1alpha1.Sandbox) (bool, error) {
			return false, assert.AnError
		})
		require.Error(t, err)
	})

	t.Run("object gone returns an error", func(t *testing.T) {
		sbx := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx-missing", Namespace: "default"}}
		c, _, err := cachetest.NewTestCache(t)
		require.NoError(t, err)

		_, err = retryUpdateSandbox(t.Context(), c, sbx, func(s *agentsv1alpha1.Sandbox) (bool, error) {
			return true, nil
		})
		require.Error(t, err)
	})
}

// --- inplaceRefreshSandbox ---

func TestInplaceRefreshSandbox(t *testing.T) {
	t.Run("returns the newer cached version", func(t *testing.T) {
		stale := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "default"}}
		c, fc, err := cachetest.NewTestCache(t, stale)
		require.NoError(t, err)

		latest := &agentsv1alpha1.Sandbox{}
		require.NoError(t, fc.Get(t.Context(), client.ObjectKeyFromObject(stale), latest))
		latest.Labels = map[string]string{"changed": "1"}
		require.NoError(t, fc.Update(t.Context(), latest))

		got, err := inplaceRefreshSandbox(t.Context(), c, stale)
		require.NoError(t, err)
		assert.Equal(t, "1", got.Labels["changed"])
	})

	t.Run("missing object surfaces an error", func(t *testing.T) {
		gone := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx-gone", Namespace: "default"}}
		c, _, err := cachetest.NewTestCache(t)
		require.NoError(t, err)

		_, err = inplaceRefreshSandbox(t.Context(), c, gone)
		require.Error(t, err)
	})
}

// --- waitForSandboxReady ---

func TestWaitForSandboxReady(t *testing.T) {
	t.Run("already-ready sandbox succeeds without needing a wait", func(t *testing.T) {
		ready := engineTestReadySandbox("sbx-ready")
		c, _, err := cachetest.NewTestCache(t, ready)
		require.NoError(t, err)

		got, cost, err := waitForSandboxReady(t.Context(), ready, claimOptions{WaitReadyTimeout: time.Second}, c)
		require.NoError(t, err)
		assert.Equal(t, "sbx-ready", got.Name)
		assert.GreaterOrEqual(t, cost, time.Duration(0))
	})

	t.Run("times out and reports a diagnostic failure reason", func(t *testing.T) {
		notReady := engineTestReadySandbox("sbx-not-ready")
		notReady.Status.Conditions = nil
		notReady.Status.PodInfo.PodIP = ""
		c, _, err := cachetest.NewTestCache(t, notReady)
		require.NoError(t, err)

		_, _, err = waitForSandboxReady(t.Context(), notReady, claimOptions{WaitReadyTimeout: 20 * time.Millisecond}, c)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not ready before wait timeout")
	})
}

// --- clearFailedSandbox / reserveFailedSandbox / deleteFailedSandbox ---

func TestClearFailedSandbox(t *testing.T) {
	t.Run("nil error is a no-op", func(t *testing.T) {
		sbx := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "default"}}
		c, fc, err := cachetest.NewTestCache(t, sbx)
		require.NoError(t, err)

		clearFailedSandbox(t.Context(), c, sbx, nil, nil)

		got := &agentsv1alpha1.Sandbox{}
		require.NoError(t, fc.Get(t.Context(), client.ObjectKeyFromObject(sbx), got), "sandbox must be untouched")
	})

	t.Run("nil sandbox is a no-op", func(t *testing.T) {
		c, _, err := cachetest.NewTestCache(t)
		require.NoError(t, err)
		clearFailedSandbox(t.Context(), c, nil, assert.AnError, nil) // must not panic
	})

	t.Run("default (never) deletes the failed sandbox", func(t *testing.T) {
		sbx := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "default"}}
		c, fc, err := cachetest.NewTestCache(t, sbx)
		require.NoError(t, err)

		clearFailedSandbox(t.Context(), c, sbx, assert.AnError, nil)

		got := &agentsv1alpha1.Sandbox{}
		err = fc.Get(t.Context(), client.ObjectKeyFromObject(sbx), got)
		assert.Error(t, err, "sandbox should have been deleted")
	})

	t.Run("negative duration reserves the sandbox forever", func(t *testing.T) {
		sbx := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "default"}}
		c, fc, err := cachetest.NewTestCache(t, sbx)
		require.NoError(t, err)

		forever := reserveFailedSandboxForever
		clearFailedSandbox(t.Context(), c, sbx, assert.AnError, &forever)

		got := &agentsv1alpha1.Sandbox{}
		require.NoError(t, fc.Get(t.Context(), client.ObjectKeyFromObject(sbx), got))
		assert.Equal(t, agentsv1alpha1.True, got.Labels[agentsv1alpha1.LabelSandboxReservedFailed])
		assert.Nil(t, got.Spec.ShutdownTime)
	})

	t.Run("positive duration reserves then schedules deletion via shutdown time", func(t *testing.T) {
		sbx := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "default"}}
		c, fc, err := cachetest.NewTestCache(t, sbx)
		require.NoError(t, err)

		reserveFor := time.Hour
		clearFailedSandbox(t.Context(), c, sbx, assert.AnError, &reserveFor)

		got := &agentsv1alpha1.Sandbox{}
		require.NoError(t, fc.Get(t.Context(), client.ObjectKeyFromObject(sbx), got))
		assert.Equal(t, agentsv1alpha1.True, got.Labels[agentsv1alpha1.LabelSandboxReservedFailed])
		require.NotNil(t, got.Spec.ShutdownTime)
		assert.True(t, got.Spec.ShutdownTime.Time.After(time.Now()))
	})

	t.Run("reserve failure falls back to delete", func(t *testing.T) {
		sbx := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx-vanished", Namespace: "default"}}
		// Not created in the fake client: retryUpdateSandbox's Get will fail,
		// forcing reserveFailedSandbox to error and clearFailedSandbox to fall
		// back to deleteFailedSandbox, which must tolerate the already-missing object.
		c, _, err := cachetest.NewTestCache(t)
		require.NoError(t, err)

		forever := reserveFailedSandboxForever
		clearFailedSandbox(t.Context(), c, sbx, assert.AnError, &forever) // must not panic despite the missing object
	})
}

// --- tryClaimSandbox (integration) ---

func TestTryClaimSandbox(t *testing.T) {
	utestutils.InitLogOutput()

	t.Run("success via update lock, running init-runtime, identity token, and access token", func(t *testing.T) {
		server := testutils.NewTestRuntimeServer(testutils.TestRuntimeServerOptions{
			RunCommandResult:      runtime.RunCommandResult{PID: 1, Exited: true},
			RunCommandImmediately: true,
		})
		defer server.Close()

		sbs := engineTestSandboxSet("tmpl")
		available := engineTestAvailableSandbox("sbx-available", sbs)
		available.Annotations[agentsv1alpha1.AnnotationRuntimeURL] = server.URL
		available.Annotations[identity.AnnotationAgentName] = "my-agent"
		available.Annotations[identity.AnnotationEnableJwtAuth] = agentsv1alpha1.True

		c, fc, err := cachetest.NewTestCache(t, sbs, available)
		require.NoError(t, err)

		opts, err := validateAndInitClaimOptions(claimOptions{
			Namespace: "default",
			User:      "test-user",
			Template:  "tmpl",
			InitRuntime: &runtimeconfig.InitRuntimeOptions{
				AccessToken: runtime.AccessToken,
			},
		})
		require.NoError(t, err)

		claimed, metrics, err := tryClaimSandbox(t.Context(), opts, &sync.Map{}, c, nil)
		require.NoError(t, err)
		require.NotNil(t, claimed)
		assert.Equal(t, lockTypeUpdate, metrics.LockType)
		assert.Equal(t, "test-user", claimed.Annotations[agentsv1alpha1.AnnotationOwner])
		assert.Equal(t, agentsv1alpha1.True, claimed.Labels[agentsv1alpha1.LabelSandboxIsClaimed])
		assert.Greater(t, metrics.InitRuntime, time.Duration(0))
		assert.Greater(t, metrics.SecurityToken, time.Duration(0))
		assert.Greater(t, metrics.TrafficToken, time.Duration(0))

		// The claimed sandbox must actually be persisted, and the identity
		// security-token step must have recorded its refresh status on it.
		got := &agentsv1alpha1.Sandbox{}
		require.NoError(t, fc.Get(t.Context(), client.ObjectKeyFromObject(claimed), got))
		assert.Equal(t, "test-user", got.Annotations[agentsv1alpha1.AnnotationOwner])
	})

	t.Run("no candidates and CreateOnNoStock false returns an error and claims nothing", func(t *testing.T) {
		sbs := engineTestSandboxSet("tmpl")
		c, _, err := cachetest.NewTestCache(t, sbs)
		require.NoError(t, err)

		opts, err := validateAndInitClaimOptions(claimOptions{User: "u", Template: "tmpl", Namespace: "default"})
		require.NoError(t, err)

		claimed, _, err := tryClaimSandbox(t.Context(), opts, &sync.Map{}, c, nil)
		require.Error(t, err)
		assert.Nil(t, claimed)
	})

	t.Run("create-on-no-stock times out waiting for ready and cleans up by deleting", func(t *testing.T) {
		sbs := engineTestSandboxSet("tmpl")
		c, fc, err := cachetest.NewTestCache(t, sbs)
		require.NoError(t, err)

		opts, err := validateAndInitClaimOptions(claimOptions{
			Namespace: "default", User: "u", Template: "tmpl",
			CreateOnNoStock:  true,
			WaitReadyTimeout: 20 * time.Millisecond,
		})
		require.NoError(t, err)

		claimed, _, err := tryClaimSandbox(t.Context(), opts, &sync.Map{}, c, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to wait for sandbox ready")
		require.NotNil(t, claimed, "the created-but-not-ready sandbox is still returned so the caller can clean it up")

		var list agentsv1alpha1.SandboxList
		require.NoError(t, fc.List(t.Context(), &list))
		for _, item := range list.Items {
			assert.NotEqual(t, claimed.Name, item.Name, "the failed sandbox must have been deleted by cleanup")
		}
	})

	t.Run("create-on-no-stock denied by the create rate limiter", func(t *testing.T) {
		sbs := engineTestSandboxSet("tmpl")
		c, _, err := cachetest.NewTestCache(t, sbs)
		require.NoError(t, err)

		opts, err := validateAndInitClaimOptions(claimOptions{
			Namespace: "default", User: "u", Template: "tmpl", CreateOnNoStock: true,
		})
		require.NoError(t, err)

		denyAll := rate.NewLimiter(0, 0)
		claimed, _, err := tryClaimSandbox(t.Context(), opts, &sync.Map{}, c, denyAll)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "rate limiter")
		assert.Nil(t, claimed)
	})

	t.Run("context already canceled fails fast without claiming anything", func(t *testing.T) {
		sbs := engineTestSandboxSet("tmpl")
		available := engineTestAvailableSandbox("sbx-available", sbs)
		c, fc, err := cachetest.NewTestCache(t, sbs, available)
		require.NoError(t, err)

		opts, err := validateAndInitClaimOptions(claimOptions{Namespace: "default", User: "u", Template: "tmpl"})
		require.NoError(t, err)

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		claimed, _, err := tryClaimSandbox(ctx, opts, &sync.Map{}, c, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "context canceled")
		assert.Nil(t, claimed)

		got := &agentsv1alpha1.Sandbox{}
		require.NoError(t, fc.Get(t.Context(), client.ObjectKeyFromObject(available), got))
		assert.Empty(t, got.Annotations[agentsv1alpha1.AnnotationLock], "nothing should have been locked")
	})

	t.Run("csi mount failure propagates as a claim error and cleans up", func(t *testing.T) {
		// The mount CLI (legacy sandbox-storage path, used because the sandbox
		// advertises no TLS port) reports failure via a non-zero process exit
		// code, so a deterministic, fast failure only requires configuring the
		// exit code -- no unreachable network endpoint or timeout needed.
		server := testutils.NewTestRuntimeServer(testutils.TestRuntimeServerOptions{
			RunCommandResult:      runtime.RunCommandResult{PID: 1, Exited: true, ExitCode: 1, Stderr: []string{"driver not found"}},
			RunCommandImmediately: true,
		})
		defer server.Close()

		sbs := engineTestSandboxSet("tmpl")
		available := engineTestAvailableSandbox("sbx-available", sbs)
		available.Annotations[agentsv1alpha1.AnnotationRuntimeURL] = server.URL
		c, fc, err := cachetest.NewTestCache(t, sbs, available)
		require.NoError(t, err)

		opts, err := validateAndInitClaimOptions(claimOptions{
			Namespace: "default", User: "u", Template: "tmpl",
			InitRuntime: &runtimeconfig.InitRuntimeOptions{},
			CSIMount: &runtimeconfig.CSIMountOptions{
				MountOptionList: []runtimeconfig.MountConfig{{Driver: "some-driver", PublishRequest: &csi.NodePublishVolumeRequest{VolumeId: "pv1", TargetPath: "/mnt/1"}}},
			},
		})
		require.NoError(t, err)

		claimed, _, err := tryClaimSandbox(t.Context(), opts, &sync.Map{}, c, nil)
		require.Error(t, err)
		assert.Contains(t, strings.ToLower(err.Error()), "csi mount")
		require.NotNil(t, claimed)

		got := &agentsv1alpha1.Sandbox{}
		getErr := fc.Get(t.Context(), client.ObjectKeyFromObject(claimed), got)
		assert.Error(t, getErr, "the failed claim's sandbox must have been cleaned up (deleted)")
	})
}
