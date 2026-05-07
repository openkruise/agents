/*
Copyright 2025.

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

package sandboxset

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func newSandbox(name, hash string, state string, claimed bool, createdAt time.Time) *v1alpha1.Sandbox {
	sbx := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(createdAt),
			Labels: map[string]string{
				v1alpha1.LabelTemplateHash:     hash,
				v1alpha1.LabelSandboxIsClaimed: "false",
				v1alpha1.LabelSandboxPool:      "test",
			},
			Annotations: map[string]string{},
		},
	}
	if claimed {
		sbx.Labels[v1alpha1.LabelSandboxIsClaimed] = "true"
	}
	// Set owner reference to SandboxSet
	sbx.OwnerReferences = []metav1.OwnerReference{
		*metav1.NewControllerRef(&v1alpha1.SandboxSet{
			ObjectMeta: metav1.ObjectMeta{Name: "test", UID: "uid-123"},
		}, v1alpha1.SandboxSetControllerKind),
	}
	switch state {
	case v1alpha1.SandboxStateCreating:
		sbx.Status.Phase = v1alpha1.SandboxRunning
	case v1alpha1.SandboxStateAvailable:
		sbx.Status.Phase = v1alpha1.SandboxRunning
		sbx.Status.PodInfo.PodIP = "1.2.3.4"
		sbx.Status.Conditions = []metav1.Condition{
			{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue},
		}
	}
	return sbx
}

func newSandboxSetForUpdate(replicas int32, maxUnavailable *intstr.IntOrString) *v1alpha1.SandboxSet {
	sbs := &v1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "default",
		},
		Spec: v1alpha1.SandboxSetSpec{
			Replicas: replicas,
			EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "main", Image: "test"}},
					},
				},
			},
		},
	}
	if maxUnavailable != nil {
		sbs.Spec.UpdateStrategy.MaxUnavailable = maxUnavailable
	}
	return sbs
}

func intOrStringPtr(v intstr.IntOrString) *intstr.IntOrString {
	return &v
}

func TestBuildUpdateGroups(t *testing.T) {
	now := time.Now()
	updateRev := "rev-new"
	oldRev := "rev-old"

	tests := []struct {
		name                  string
		groups                GroupedSandboxes
		updateRevision        string
		expectNil             bool
		expectUpdatedCreating int
		expectUpdatedAvail    int
		expectOldCreating     int
		expectOldAvail        int
	}{
		{
			name:           "empty groups",
			groups:         GroupedSandboxes{},
			updateRevision: updateRev,
		},
		{
			name: "all updated",
			groups: GroupedSandboxes{
				Creating:  []*v1alpha1.Sandbox{newSandbox("c1", updateRev, v1alpha1.SandboxStateCreating, false, now)},
				Available: []*v1alpha1.Sandbox{newSandbox("a1", updateRev, v1alpha1.SandboxStateAvailable, false, now)},
			},
			updateRevision:        updateRev,
			expectUpdatedCreating: 1,
			expectUpdatedAvail:    1,
		},
		{
			name: "all old",
			groups: GroupedSandboxes{
				Creating:  []*v1alpha1.Sandbox{newSandbox("c1", oldRev, v1alpha1.SandboxStateCreating, false, now)},
				Available: []*v1alpha1.Sandbox{newSandbox("a1", oldRev, v1alpha1.SandboxStateAvailable, false, now)},
			},
			updateRevision:    updateRev,
			expectOldCreating: 1,
			expectOldAvail:    1,
		},
		{
			name: "mixed old and new",
			groups: GroupedSandboxes{
				Creating: []*v1alpha1.Sandbox{
					newSandbox("c1", updateRev, v1alpha1.SandboxStateCreating, false, now),
					newSandbox("c2", oldRev, v1alpha1.SandboxStateCreating, false, now),
				},
				Available: []*v1alpha1.Sandbox{
					newSandbox("a1", updateRev, v1alpha1.SandboxStateAvailable, false, now),
					newSandbox("a2", oldRev, v1alpha1.SandboxStateAvailable, false, now),
					newSandbox("a3", oldRev, v1alpha1.SandboxStateAvailable, false, now),
				},
			},
			updateRevision:        updateRev,
			expectUpdatedCreating: 1,
			expectUpdatedAvail:    1,
			expectOldCreating:     1,
			expectOldAvail:        2,
		},
		{
			name: "claimed sandboxes are excluded",
			groups: GroupedSandboxes{
				Creating: []*v1alpha1.Sandbox{
					newSandbox("c1", oldRev, v1alpha1.SandboxStateCreating, true, now),
				},
				Available: []*v1alpha1.Sandbox{
					newSandbox("a1", oldRev, v1alpha1.SandboxStateAvailable, true, now),
					newSandbox("a2", oldRev, v1alpha1.SandboxStateAvailable, false, now),
				},
			},
			updateRevision: updateRev,
			expectOldAvail: 1, // only the unclaimed one
		},
		{
			name: "used and dead sandboxes are not in groups",
			groups: GroupedSandboxes{
				Used: []*v1alpha1.Sandbox{newSandbox("u1", oldRev, v1alpha1.SandboxStateRunning, false, now)},
				Dead: []*v1alpha1.Sandbox{newSandbox("d1", oldRev, v1alpha1.SandboxStateDead, false, now)},
			},
			updateRevision: updateRev,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ug := buildUpdateGroups(tt.groups, tt.updateRevision)
			if tt.expectNil {
				assert.Nil(t, ug)
			} else {
				assert.NotNil(t, ug)
				assert.Equal(t, tt.expectUpdatedCreating, len(ug.UpdatedCreating))
				assert.Equal(t, tt.expectUpdatedAvail, len(ug.UpdatedAvailable))
				assert.Equal(t, tt.expectOldCreating, len(ug.OldCreating))
				assert.Equal(t, tt.expectOldAvail, len(ug.OldAvailable))
			}
		})
	}
}

func TestNeedsUpdate(t *testing.T) {
	assert.False(t, needsUpdate(&UpdateGroupedSandboxes{}))
	assert.True(t, needsUpdate(&UpdateGroupedSandboxes{
		OldCreating: []*v1alpha1.Sandbox{{}},
	}))
	assert.True(t, needsUpdate(&UpdateGroupedSandboxes{
		OldAvailable: []*v1alpha1.Sandbox{{}},
	}))
	assert.False(t, needsUpdate(&UpdateGroupedSandboxes{
		UpdatedCreating:  []*v1alpha1.Sandbox{{}},
		UpdatedAvailable: []*v1alpha1.Sandbox{{}},
	}))
}

func TestIsSandboxClaimed(t *testing.T) {
	now := time.Now()
	// unclaimed sandbox
	unclaimed := newSandbox("s1", "rev", v1alpha1.SandboxStateAvailable, false, now)
	assert.False(t, isSandboxClaimed(unclaimed))

	// claimed via label
	claimed := newSandbox("s2", "rev", v1alpha1.SandboxStateAvailable, true, now)
	assert.True(t, isSandboxClaimed(claimed))

	// claimed via removed owner reference
	noOwner := newSandbox("s3", "rev", v1alpha1.SandboxStateAvailable, false, now)
	noOwner.OwnerReferences = nil
	assert.True(t, isSandboxClaimed(noOwner))
}

func TestIsUpdateComplete(t *testing.T) {
	assert.True(t, isUpdateComplete(&UpdateInfo{ToUpdate: 0}))
	assert.False(t, isUpdateComplete(&UpdateInfo{ToUpdate: 1}))
}

func TestCalculateUpdateInfo(t *testing.T) {
	now := time.Now()
	updateRev := "rev-new"
	oldRev := "rev-old"

	tests := []struct {
		name                     string
		replicas                 int32
		maxUnavailable           *intstr.IntOrString
		updatedCreating          int
		updatedAvailable         int
		oldCreating              int
		oldAvailable             int
		expectCurrentUpdated     int
		expectToUpdate           int
		expectAllowedUnavailable int
	}{
		{
			name:                 "all updated, no update needed",
			replicas:             5,
			updatedAvailable:     5,
			expectCurrentUpdated: 5,
			expectToUpdate:       0,
		},
		{
			name:                     "all old available, default strategy",
			replicas:                 10,
			maxUnavailable:           intOrStringPtr(intstr.FromString("20%")), // Explicitly set default value
			oldAvailable:             10,
			expectToUpdate:           10,
			expectAllowedUnavailable: 2, // 20% of 10
		},
		{
			name:                 "mixed: some updated, some old",
			replicas:             10,
			updatedAvailable:     6,
			oldAvailable:         4,
			expectCurrentUpdated: 6,
			expectToUpdate:       4,
		},
		{
			// OldCreating are deleted freely; AllowedUnavailable accounts for UpdatedCreating.
			name:                     "old creating can be deleted freely, no old available",
			replicas:                 5,
			maxUnavailable:           intOrStringPtr(intstr.FromString("20%")), // Explicitly set default value
			updatedAvailable:         3,
			oldCreating:              2,
			expectCurrentUpdated:     3,
			expectToUpdate:           2,
			expectAllowedUnavailable: 1, // 20% of 5 = 1, no UpdatedCreating to subtract
		},
		{
			// Key regression test: with maxUnavailable=0,
			// OldCreating must still be deletable (free), but OldAvailable must NOT be touched.
			name:                     "old creating free + old available blocked when budget is zero",
			replicas:                 5,
			maxUnavailable:           intOrStringPtr(intstr.FromInt(0)),
			updatedAvailable:         1,
			oldCreating:              2,
			oldAvailable:             2,
			expectCurrentUpdated:     1,
			expectAllowedUnavailable: 0,
			expectToUpdate:           4,
		},
		{
			// With real unavailable budget, OldAvailable can also be replaced.
			name:                     "old creating free + old available with real unavailable budget",
			replicas:                 5,
			maxUnavailable:           intOrStringPtr(intstr.FromInt(2)),
			updatedAvailable:         1,
			oldCreating:              1,
			oldAvailable:             3,
			expectCurrentUpdated:     1,
			expectAllowedUnavailable: 2,
			expectToUpdate:           4,
		},
		{
			name:                     "custom maxUnavailable",
			replicas:                 10,
			maxUnavailable:           intOrStringPtr(intstr.FromInt(2)),
			oldAvailable:             10,
			expectToUpdate:           10,
			expectAllowedUnavailable: 2,
		},
		{
			// UpdatedCreating consumes unavailable budget.
			name:                     "updated creating consumes unavailable budget",
			replicas:                 10,
			maxUnavailable:           intOrStringPtr(intstr.FromInt(3)),
			updatedCreating:          2,
			oldAvailable:             8,
			expectCurrentUpdated:     2,
			expectToUpdate:           8,
			expectAllowedUnavailable: 1, // maxUnavailable(3) - UpdatedCreating(2) = 1
		},
		{
			// Regression test when a unavailable pod is already in-flight (UpdatedCreating=1).
			name:                     "updated creating blocks further unavailable",
			replicas:                 4,
			maxUnavailable:           intOrStringPtr(intstr.FromString("20%")), // floor(4*0.2)=0
			updatedCreating:          1,                                        // surge pod already created
			oldAvailable:             4,
			expectCurrentUpdated:     1,
			expectAllowedUnavailable: 0, // max(0 - 1, 0) = 0
			expectToUpdate:           4, // OldCreating(0) + OldAvailable(4) = 4
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbs := newSandboxSetForUpdate(tt.replicas, tt.maxUnavailable)
			ug := &UpdateGroupedSandboxes{}
			for i := 0; i < tt.updatedCreating; i++ {
				ug.UpdatedCreating = append(ug.UpdatedCreating, newSandbox("uc"+string(rune('0'+i)), updateRev, v1alpha1.SandboxStateCreating, false, now))
			}
			for i := 0; i < tt.updatedAvailable; i++ {
				ug.UpdatedAvailable = append(ug.UpdatedAvailable, newSandbox("ua"+string(rune('0'+i)), updateRev, v1alpha1.SandboxStateAvailable, false, now))
			}
			for i := 0; i < tt.oldCreating; i++ {
				ug.OldCreating = append(ug.OldCreating, newSandbox("oc"+string(rune('0'+i)), oldRev, v1alpha1.SandboxStateCreating, false, now))
			}
			for i := 0; i < tt.oldAvailable; i++ {
				ug.OldAvailable = append(ug.OldAvailable, newSandbox("oa"+string(rune('0'+i)), oldRev, v1alpha1.SandboxStateAvailable, false, now))
			}

			info := calculateUpdateInfo(sbs, ug)
			assert.Equal(t, tt.expectCurrentUpdated, info.CurrentUpdated, "CurrentUpdated")
			assert.Equal(t, tt.expectToUpdate, info.ToUpdate, "ToUpdate")
			if tt.expectAllowedUnavailable > 0 || tt.maxUnavailable != nil {
				assert.Equal(t, tt.expectAllowedUnavailable, info.AllowedUnavailable, "AllowedUnavailable")
			}
		})
	}
}

func TestGetMaxUnavailablePods(t *testing.T) {
	tests := []struct {
		name           string
		maxUnavailable *intstr.IntOrString
		replicas       int
		expected       int
	}{
		{"percentage normal: 20% of 10 = 2", intOrStringPtr(intstr.FromString("20%")), 10, 2},
		{"percentage round up: 20% of 3 = ceil(0.6) = 1", intOrStringPtr(intstr.FromString("20%")), 3, 1},
		{"percentage 100%: 100% of 5 = 5", intOrStringPtr(intstr.FromString("100%")), 5, 5},
		{"percentage 1%: 1% of 1 = ceil(0.01) = 1", intOrStringPtr(intstr.FromString("1%")), 1, 1},
		{"integer value: 3", intOrStringPtr(intstr.FromInt(3)), 10, 3},
		{"integer value 0", intOrStringPtr(intstr.FromInt(0)), 10, 0},
		{"nil maxUnavailable defaults to 0", nil, 5, 0},
		{"replicas 0 with percentage", intOrStringPtr(intstr.FromString("20%")), 0, 0},
		{"replicas 1 with 50%: ceil(0.5) = 1", intOrStringPtr(intstr.FromString("50%")), 1, 1},
		{"large replicas: 30% of 100 = 30", intOrStringPtr(intstr.FromString("30%")), 100, 30},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbs := newSandboxSetForUpdate(int32(tt.replicas), tt.maxUnavailable)
			got := getMaxUnavailablePods(sbs, tt.replicas)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestCalculateUpdateInfo_OldCreatingFreeDelete(t *testing.T) {
	// Regression test: OldCreating sandboxes can be deleted freely even when maxUnavailable=0.
	// AllowedUnavailable must be 0 (real maxUnavailable), not inflated.
	now := time.Now()
	sbs := newSandboxSetForUpdate(5, intOrStringPtr(intstr.FromInt(0)))
	ug := &UpdateGroupedSandboxes{
		UpdatedAvailable: []*v1alpha1.Sandbox{
			newSandbox("ua1", "new", v1alpha1.SandboxStateAvailable, false, now),
			newSandbox("ua2", "new", v1alpha1.SandboxStateAvailable, false, now),
			newSandbox("ua3", "new", v1alpha1.SandboxStateAvailable, false, now),
		},
		OldCreating: []*v1alpha1.Sandbox{
			newSandbox("oc1", "old", v1alpha1.SandboxStateCreating, false, now),
			newSandbox("oc2", "old", v1alpha1.SandboxStateCreating, false, now),
		},
	}

	info := calculateUpdateInfo(sbs, ug)
	assert.Equal(t, 2, info.ToUpdate)
	// AllowedUnavailable must be 0 (real maxUnavailable), NOT inflated.
	assert.Equal(t, 0, info.AllowedUnavailable, "AllowedUnavailable must not be inflated by OldCreating")
}

func TestReconcile_RollingUpdate(t *testing.T) {
	tests := []struct {
		name             string
		replicas         int32
		request          createSandboxRequest
		oldHash          string
		expectOldRemain  int
		expectNewCreated int
	}{
		{
			name:     "rolling update deletes old creating sandboxes",
			replicas: 3,
			request: createSandboxRequest{
				createCreatingSandboxes: 3,
			},
			oldHash:          "old-hash",
			expectOldRemain:  0,
			expectNewCreated: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			k8sClient := NewClient()
			eventRecorder := record.NewFakeRecorder(20)
			reconciler := &Reconciler{
				Client:   k8sClient,
				Scheme:   testScheme,
				Recorder: eventRecorder,
				Codec:    codec,
			}
			sbs := getSandboxSet(tt.replicas)
			assert.NoError(t, k8sClient.Create(ctx, sbs))
			newStatus, err := reconciler.initNewStatus(sbs)
			assert.NoError(t, err)

			// Use an old hash to create sandboxes (simulate old revision)
			if tt.oldHash != "" {
				newStatus.UpdateRevision = tt.oldHash
			}
			sbs.Status = *newStatus
			CreateSandboxes(t, tt.request, sbs, k8sClient)

			// Now update the SandboxSet template to trigger update
			sbs.Status.UpdateRevision = "" // reset so initNewStatus recalculates
			scaleUpExpectation.DeleteExpectations(GetControllerKey(sbs))
			scaleDownExpectation.DeleteExpectations(GetControllerKey(sbs))

			// Reconcile - should detect old sandboxes and perform rolling update
			_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sbs)})
			assert.NoError(t, err)

			// Check that old sandboxes were deleted and new ones potentially created
			var sandboxList v1alpha1.SandboxList
			assert.NoError(t, k8sClient.List(ctx, &sandboxList))
			var oldCount, newCount int
			for _, sbx := range sandboxList.Items {
				if sbx.DeletionTimestamp != nil {
					continue
				}
				if sbx.Labels[v1alpha1.LabelTemplateHash] == tt.oldHash {
					oldCount++
				} else {
					newCount++
				}
			}
			// After one reconcile round, some old should be deleted and new created
			// The exact count depends on maxSurge/maxUnavailable defaults (20%)
			t.Logf("old=%d, new=%d", oldCount, newCount)
			assert.True(t, newCount > 0 || oldCount < int(tt.replicas), "rolling update should make progress")
		})
	}
}

// TestReconcile_RollingUpdate_SurgeGate is a regression test for the bug where
// performRollingUpdate was not gated by scaleUpExpectation, causing multiple reconcile
// cycles to each create a surge sandbox before any are observed — resulting in a total
// surge far exceeding maxSurge.
func TestReconcile_RollingUpdate_SurgeGate(t *testing.T) {
	ctx := t.Context()
	now := time.Now()
	oldHash := "old-hash"

	k8sClient := NewClient()
	eventRecorder := record.NewFakeRecorder(20)
	reconciler := &Reconciler{
		Client:   k8sClient,
		Scheme:   testScheme,
		Recorder: eventRecorder,
		Codec:    codec,
	}

	// replicas=4, default maxSurge=20% (ceil→1), maxUnavailable=20% (floor→0)
	sbs := getSandboxSet(4)
	assert.NoError(t, k8sClient.Create(ctx, sbs))
	newStatus, err := reconciler.initNewStatus(sbs)
	assert.NoError(t, err)
	newHash := newStatus.UpdateRevision

	// Create 4 old-revision available sandboxes (simulating pre-edit state)
	newStatus.UpdateRevision = oldHash
	sbs.Status = *newStatus
	CreateSandboxes(t, createSandboxRequest{createAvailableSandboxes: 4}, sbs, k8sClient)

	// Simulate scaleUpExpectation being unsatisfied: a previous rolling update create
	// has been issued but not yet observed by the informer.
	controllerKey := GetControllerKey(sbs)
	scaleDownExpectation.DeleteExpectations(controllerKey)
	scaleUpExpectation.DeleteExpectations(controllerKey)
	scaleUpExpectation.ExpectScale(controllerKey, "create", "pending-sandbox")

	// Reconcile while scaleUpExpectation is NOT satisfied.
	// Rolling update must be SKIPPED to prevent surge > maxSurge.
	sbs.Status.UpdateRevision = ""
	_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sbs)})
	assert.NoError(t, err)

	var sandboxList v1alpha1.SandboxList
	assert.NoError(t, k8sClient.List(ctx, &sandboxList))
	newCount := 0
	for _, sbx := range sandboxList.Items {
		if sbx.DeletionTimestamp != nil {
			continue
		}
		if sbx.Labels[v1alpha1.LabelTemplateHash] != oldHash {
			newCount++
		}
	}
	// Rolling update must have been skipped: no new-revision sandboxes should be created.
	assert.Equal(t, 0, newCount, "rolling update must be skipped when scaleUpExpectation is not satisfied")

	// Now simulate the pending sandbox being observed (expectation cleared).
	scaleUpExpectation.ObserveScale(controllerKey, "create", "pending-sandbox")
	// Add the "observed" sandbox to the fake client so groupAllSandboxes sees it.
	observed := newSandbox("pending-sandbox", newHash, v1alpha1.SandboxStateCreating, false, now)
	observed.OwnerReferences = []metav1.OwnerReference{
		*metav1.NewControllerRef(sbs, v1alpha1.SandboxSetControllerKind),
	}
	assert.NoError(t, k8sClient.Create(ctx, observed))

	// Reconcile again — now scaleUpSatisfied=true, rolling update may proceed.
	// State going in: 1 UpdatedCreating (pending-sandbox) + 4 OldAvailable = 5 (delta=-1).
	// Step 1 (scale): delta=-1 → scaleDown deletes 1 old sandbox.
	// Step 3 (rolling update): AllowedSurge = max(1 - UpdatedCreating(1), 0) = 0
	//   → createCount=0, no more surge pod is created (budget already consumed by pending-sandbox).
	_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sbs)})
	assert.NoError(t, err)

	sandboxList = v1alpha1.SandboxList{}
	assert.NoError(t, k8sClient.List(ctx, &sandboxList))
	newCount = 0
	var oldCount int
	for _, sbx := range sandboxList.Items {
		if sbx.DeletionTimestamp != nil {
			continue
		}
		if sbx.Labels[v1alpha1.LabelTemplateHash] != oldHash {
			newCount++
		} else {
			oldCount++
		}
	}
	// Expected:
	// - 1 new: only pending-sandbox (rolling update blocked by AllowedSurge=0)
	// - 3 old: 4 - 1 deleted by scaleDown (delta was -1 due to 5 total > 4 replicas)
	// Total surge = 1, within maxSurge=1. No additional create happens because the
	// existing UpdatedCreating pod already exhausts the surge budget.
	t.Logf("new-revision sandboxes after gate-cleared reconcile: %d, old remaining: %d", newCount, oldCount)
	assert.Equal(t, 1, newCount, "only pending-sandbox should exist; rolling update must not create when AllowedSurge=0")
	assert.Equal(t, 3, oldCount, "scaleDown should have deleted 1 old sandbox (delta=-1), leaving 3")
}

func TestFindOldestSandboxes(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name      string
		sandboxes []*v1alpha1.Sandbox
		k         int
		wantNames []string
	}{
		{
			name: "k equals length - returns all sandboxes",
			sandboxes: []*v1alpha1.Sandbox{
				newSandbox("sbx-1", "hash1", v1alpha1.SandboxStateAvailable, false, now.Add(-3*time.Hour)),
				newSandbox("sbx-2", "hash2", v1alpha1.SandboxStateAvailable, false, now.Add(-1*time.Hour)),
				newSandbox("sbx-3", "hash3", v1alpha1.SandboxStateAvailable, false, now.Add(-2*time.Hour)),
			},
			k:         3,
			wantNames: []string{"sbx-1", "sbx-3", "sbx-2"},
		},
		{
			name: "k greater than length - returns all sandboxes",
			sandboxes: []*v1alpha1.Sandbox{
				newSandbox("sbx-1", "hash1", v1alpha1.SandboxStateAvailable, false, now.Add(-2*time.Hour)),
				newSandbox("sbx-2", "hash2", v1alpha1.SandboxStateAvailable, false, now.Add(-1*time.Hour)),
			},
			k:         5,
			wantNames: []string{"sbx-1", "sbx-2"},
		},
		{
			name: "k equals 1 - returns oldest sandbox",
			sandboxes: []*v1alpha1.Sandbox{
				newSandbox("sbx-1", "hash1", v1alpha1.SandboxStateAvailable, false, now.Add(-2*time.Hour)),
				newSandbox("sbx-2", "hash2", v1alpha1.SandboxStateAvailable, false, now.Add(-3*time.Hour)),
				newSandbox("sbx-3", "hash3", v1alpha1.SandboxStateAvailable, false, now.Add(-1*time.Hour)),
			},
			k:         1,
			wantNames: []string{"sbx-2"},
		},
		{
			name: "k less than length - returns k oldest sandboxes",
			sandboxes: []*v1alpha1.Sandbox{
				newSandbox("sbx-1", "hash1", v1alpha1.SandboxStateAvailable, false, now.Add(-5*time.Hour)),
				newSandbox("sbx-2", "hash2", v1alpha1.SandboxStateAvailable, false, now.Add(-2*time.Hour)),
				newSandbox("sbx-3", "hash3", v1alpha1.SandboxStateAvailable, false, now.Add(-4*time.Hour)),
				newSandbox("sbx-4", "hash4", v1alpha1.SandboxStateAvailable, false, now.Add(-1*time.Hour)),
				newSandbox("sbx-5", "hash5", v1alpha1.SandboxStateAvailable, false, now.Add(-3*time.Hour)),
			},
			k:         3,
			wantNames: []string{"sbx-1", "sbx-3", "sbx-5"},
		},
		{
			name:      "empty sandbox list - returns empty",
			sandboxes: []*v1alpha1.Sandbox{},
			k:         2,
			wantNames: []string{},
		},
		{
			name: "k equals 0 - returns empty",
			sandboxes: []*v1alpha1.Sandbox{
				newSandbox("sbx-1", "hash1", v1alpha1.SandboxStateAvailable, false, now.Add(-1*time.Hour)),
			},
			k:         0,
			wantNames: []string{},
		},
		{
			name: "sandboxes with same creation time - maintains order",
			sandboxes: []*v1alpha1.Sandbox{
				newSandbox("sbx-1", "hash1", v1alpha1.SandboxStateAvailable, false, now),
				newSandbox("sbx-2", "hash2", v1alpha1.SandboxStateAvailable, false, now),
				newSandbox("sbx-3", "hash3", v1alpha1.SandboxStateAvailable, false, now),
			},
			k:         2,
			wantNames: []string{"sbx-1", "sbx-2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := findOldestSandboxes(tt.sandboxes, tt.k, oldestFirst)

			assert.Equal(t, len(tt.wantNames), len(result), "result length mismatch")

			for i, sbx := range result {
				assert.Equal(t, tt.wantNames[i], sbx.Name, "sandbox name at index %d mismatch", i)
			}

			if len(result) > 1 {
				for i := 1; i < len(result); i++ {
					assert.True(t,
						result[i-1].CreationTimestamp.Before(&result[i].CreationTimestamp) ||
							result[i-1].CreationTimestamp.Equal(&result[i].CreationTimestamp),
						"result should be sorted by creation time, but %s (created at %v) is after %s (created at %v)",
						result[i].Name, result[i].CreationTimestamp,
						result[i-1].Name, result[i-1].CreationTimestamp,
					)
				}
			}
		})
	}
}
