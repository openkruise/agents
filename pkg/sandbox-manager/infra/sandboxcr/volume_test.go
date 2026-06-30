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
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/api/v1alpha1"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

// makeAvailablePV creates a PV in Available phase with the given capacity.
func makeAvailablePV(name string, storageGi int) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse(fmt.Sprintf("%dGi", storageGi)),
			},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeAvailable},
	}
}

// randString generates a random lowercase alpha string of length n for test data.
func randString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}

// makeClaimedSandboxWithMount creates a SandboxClaim with DynamicVolumesMount referencing
// pvName, plus a claimed Sandbox labeled with LabelSandboxClaimName. This simulates
// the state after a successful claim — no PV annotation needed.
func makeClaimedSandboxWithMount(t *testing.T, c client.Client, namespace, claimName, sandboxName, pvName string) {
	t.Helper()
	// Create the SandboxClaim with DynamicVolumesMount.
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: namespace},
		Spec: v1alpha1.SandboxClaimSpec{
			TemplateName: "test-template",
			DynamicVolumesMount: []v1alpha1.CSIMountConfig{
				{PvName: pvName, MountPath: "/data"},
			},
		},
	}
	require.NoError(t, c.Create(t.Context(), claim))

	// Create the claimed Sandbox labeled with the claim name.
	sbx := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: namespace,
			Labels: map[string]string{
				v1alpha1.LabelSandboxIsClaimed: v1alpha1.True,
				v1alpha1.LabelSandboxClaimName: claimName,
			},
		},
		Status: v1alpha1.SandboxStatus{Phase: v1alpha1.SandboxRunning},
	}
	CreateSandboxWithStatus(t, c, sbx)
}

// ---------------------------------------------------------------------------
// Unit tests — RegisterVolume
// ---------------------------------------------------------------------------

func TestVolumeInfra_RegisterVolume(t *testing.T) {
	tests := []struct {
		name        string
		setupPV     func() *corev1.PersistentVolume
		opts        infra.RegisterVolumeOptions
		expectError string
		expectCode  managererrors.ErrorCode
		check       func(t *testing.T, info infra.VolumeInfo)
	}{
		{
			name:        "empty namespace — returns BadRequest",
			setupPV:     func() *corev1.PersistentVolume { return nil },
			opts:        infra.RegisterVolumeOptions{Name: "vol1", PvName: "pv-001", SizeGB: 1},
			expectError: "namespace is required",
			expectCode:  managererrors.ErrorBadRequest,
		},
		{
			name:        "empty name — returns BadRequest",
			setupPV:     func() *corev1.PersistentVolume { return nil },
			opts:        infra.RegisterVolumeOptions{Namespace: "ns1", PvName: "pv-001", SizeGB: 1},
			expectError: "name is required",
			expectCode:  managererrors.ErrorBadRequest,
		},
		{
			name:        "empty pvName — returns BadRequest",
			setupPV:     func() *corev1.PersistentVolume { return nil },
			opts:        infra.RegisterVolumeOptions{Namespace: "ns1", Name: "vol1", SizeGB: 1},
			expectError: "pvName is required",
			expectCode:  managererrors.ErrorBadRequest,
		},
		{
			name:        "zero sizeGB — returns BadRequest",
			setupPV:     func() *corev1.PersistentVolume { return nil },
			opts:        infra.RegisterVolumeOptions{Namespace: "ns1", Name: "vol1", PvName: "pv-001", SizeGB: 0},
			expectError: "sizeGB must be a positive integer",
			expectCode:  managererrors.ErrorBadRequest,
		},
		{
			name:        "PV not found",
			setupPV:     func() *corev1.PersistentVolume { return nil },
			opts:        infra.RegisterVolumeOptions{Namespace: "ns1", Name: "vol1", PvName: "nonexistent-pv", SizeGB: 1},
			expectError: "not found",
			expectCode:  managererrors.ErrorNotFound,
		},
		{
			name: "PV not in Available phase",
			setupPV: func() *corev1.PersistentVolume {
				pv := makeAvailablePV("pv-bound", 10)
				pv.Status.Phase = corev1.VolumeBound
				return pv
			},
			opts:        infra.RegisterVolumeOptions{Namespace: "ns1", Name: "vol-bound", PvName: "pv-bound", SizeGB: 1},
			expectError: "not in Available phase",
			expectCode:  managererrors.ErrorBadRequest,
		},
		{
			name:        "PV capacity less than sizeGB",
			setupPV:     func() *corev1.PersistentVolume { return makeAvailablePV("pv-small", 1) },
			opts:        infra.RegisterVolumeOptions{Namespace: "ns1", Name: "vol-small", PvName: "pv-small", SizeGB: 5},
			expectError: "capacity",
			expectCode:  managererrors.ErrorBadRequest,
		},
		{
			name: "PV already owned by same namespace",
			setupPV: func() *corev1.PersistentVolume {
				pv := makeAvailablePV("pv-owned-same", 10)
				pv.Labels = map[string]string{
					v1alpha1.LabelVolumeOwnerNamespace: "ns1",
					v1alpha1.LabelVolumeName:           "existing-vol",
				}
				return pv
			},
			opts:        infra.RegisterVolumeOptions{Namespace: "ns1", Name: "vol-new", PvName: "pv-owned-same", SizeGB: 1},
			expectError: "already registered",
			expectCode:  managererrors.ErrorConflict,
		},
		{
			name: "PV already owned by different namespace",
			setupPV: func() *corev1.PersistentVolume {
				pv := makeAvailablePV("pv-owned-diff", 10)
				pv.Labels = map[string]string{
					v1alpha1.LabelVolumeOwnerNamespace: "other-ns",
					v1alpha1.LabelVolumeName:           "other-vol",
				}
				return pv
			},
			opts:        infra.RegisterVolumeOptions{Namespace: "ns1", Name: "vol-new", PvName: "pv-owned-diff", SizeGB: 1},
			expectError: "different namespace",
			expectCode:  managererrors.ErrorNotAllowed,
		},
		{
			name:    "volume name already in use in namespace",
			setupPV: nil, // handled inline below
		},
		{
			name:    "successful registration",
			setupPV: func() *corev1.PersistentVolume { return makeAvailablePV("pv-ok", 10) },
			opts:    infra.RegisterVolumeOptions{Namespace: "ns1", Name: "vol-ok", PvName: "pv-ok", SizeGB: 5},
			check: func(t *testing.T, info infra.VolumeInfo) {
				assert.Equal(t, "pv-ok", info.VolumeID)
				assert.Equal(t, "pv-ok", info.PvName)
				assert.Equal(t, "vol-ok", info.Name)
				assert.Equal(t, 10, info.SizeGB)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			i, c := NewTestInfra(t)

			if tt.name == "volume name already in use in namespace" {
				existing := makeAvailablePV("pv-existing", 10)
				require.NoError(t, c.Create(t.Context(), existing))
				time.Sleep(50 * time.Millisecond)
				_, err := i.RegisterVolume(t.Context(), infra.RegisterVolumeOptions{
					Namespace: "ns1", Name: "vol-dupe", PvName: "pv-existing", SizeGB: 1,
				})
				require.NoError(t, err)
				second := makeAvailablePV("pv-second", 10)
				require.NoError(t, c.Create(t.Context(), second))
				time.Sleep(50 * time.Millisecond)
				_, err = i.RegisterVolume(t.Context(), infra.RegisterVolumeOptions{
					Namespace: "ns1", Name: "vol-dupe", PvName: "pv-second", SizeGB: 1,
				})
				require.Error(t, err)
				assert.Contains(t, err.Error(), "already in use")
				assert.Equal(t, managererrors.ErrorConflict, managererrors.GetErrCode(err))
				return
			}

			if tt.setupPV != nil {
				pv := tt.setupPV()
				if pv != nil {
					require.NoError(t, c.Create(t.Context(), pv))
					time.Sleep(50 * time.Millisecond)
				}
			}

			info, err := i.RegisterVolume(t.Context(), tt.opts)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				if tt.expectCode != "" {
					assert.Equal(t, tt.expectCode, managererrors.GetErrCode(err))
				}
			} else {
				require.NoError(t, err)
				if tt.check != nil {
					tt.check(t, info)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Unit tests — ListVolumes
// ---------------------------------------------------------------------------

func TestVolumeInfra_ListVolumes(t *testing.T) {
	tests := []struct {
		name        string
		opts        infra.ListVolumesOptions
		expectCount int
		expectError string
	}{
		{
			name:        "no volumes in namespace",
			opts:        infra.ListVolumesOptions{Namespace: "empty-ns"},
			expectCount: 0,
		},
		{
			name:        "lists only volumes in requested namespace",
			opts:        infra.ListVolumesOptions{Namespace: "ns-a"},
			expectCount: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			i, c := NewTestInfra(t)

			if tt.name == "lists only volumes in requested namespace" {
				for idx, pvName := range []string{"pv-list-a1", "pv-list-a2"} {
					pv := makeAvailablePV(pvName, 10)
					require.NoError(t, c.Create(t.Context(), pv))
					time.Sleep(50 * time.Millisecond)
					_, err := i.RegisterVolume(t.Context(), infra.RegisterVolumeOptions{
						Namespace: "ns-a", Name: fmt.Sprintf("vol-a%d", idx), PvName: pvName, SizeGB: 1,
					})
					require.NoError(t, err)
				}
				pvB := makeAvailablePV("pv-list-b1", 10)
				require.NoError(t, c.Create(t.Context(), pvB))
				time.Sleep(50 * time.Millisecond)
				_, err := i.RegisterVolume(t.Context(), infra.RegisterVolumeOptions{
					Namespace: "ns-b", Name: "vol-b0", PvName: "pv-list-b1", SizeGB: 1,
				})
				require.NoError(t, err)
				time.Sleep(50 * time.Millisecond)
			}

			result, err := i.ListVolumes(t.Context(), tt.opts)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				require.NoError(t, err)
				assert.Len(t, result, tt.expectCount)
				assert.NotNil(t, result)
			}
		})
	}
}

// TestVolumeInfra_ListVolumes_MountedBy verifies that volume usage is derived
// from SandboxClaim objects, not from any PV annotation.
func TestVolumeInfra_ListVolumes_MountedBy(t *testing.T) {
	i, c := NewTestInfra(t)
	ns := "ns-mount"

	pv := makeAvailablePV("pv-mounted", 10)
	require.NoError(t, c.Create(t.Context(), pv))
	time.Sleep(50 * time.Millisecond)
	_, err := i.RegisterVolume(t.Context(), infra.RegisterVolumeOptions{
		Namespace: ns, Name: "vol-mounted", PvName: "pv-mounted", SizeGB: 1,
	})
	require.NoError(t, err)

	// Create a SandboxClaim with DynamicVolumesMount referencing pv-mounted.
	makeClaimedSandboxWithMount(t, c, ns, "claim-mounted", "sbx-001", "pv-mounted")
	time.Sleep(50 * time.Millisecond)

	// getVolumeUsers should report the sandbox as using the volume.
	users, err := i.getVolumeUsers(t.Context(), ns, "pv-mounted")
	require.NoError(t, err)
	assert.NotEmpty(t, users)
	assert.Contains(t, users, ns+"--sbx-001")
}

// ---------------------------------------------------------------------------
// Unit tests — GetVolume
// ---------------------------------------------------------------------------

func TestVolumeInfra_GetVolume(t *testing.T) {
	tests := []struct {
		name        string
		opts        infra.GetVolumeOptions
		expectError string
		expectCode  managererrors.ErrorCode
		check       func(t *testing.T, info infra.VolumeInfo)
	}{
		{
			name:        "volume not found",
			opts:        infra.GetVolumeOptions{Namespace: "ns1", VolumeID: "ghost-pv"},
			expectError: "not found",
			expectCode:  managererrors.ErrorNotFound,
		},
		{
			name:        "volume owned by different namespace",
			opts:        infra.GetVolumeOptions{Namespace: "ns-wrong", VolumeID: "pv-get-ok"},
			expectError: "not found",
			expectCode:  managererrors.ErrorNotFound,
		},
		{
			name: "successful get",
			opts: infra.GetVolumeOptions{Namespace: "ns1", VolumeID: "pv-get-ok"},
			check: func(t *testing.T, info infra.VolumeInfo) {
				assert.Equal(t, "pv-get-ok", info.VolumeID)
				assert.Equal(t, "vol-get", info.Name)
				assert.Equal(t, 10, info.SizeGB)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			i, c := NewTestInfra(t)

			pv := makeAvailablePV("pv-get-ok", 10)
			require.NoError(t, c.Create(t.Context(), pv))
			time.Sleep(50 * time.Millisecond)
			_, err := i.RegisterVolume(t.Context(), infra.RegisterVolumeOptions{
				Namespace: "ns1", Name: "vol-get", PvName: "pv-get-ok", SizeGB: 1,
			})
			require.NoError(t, err)
			time.Sleep(50 * time.Millisecond)

			info, err := i.GetVolume(t.Context(), tt.opts)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				if tt.expectCode != "" {
					assert.Equal(t, tt.expectCode, managererrors.GetErrCode(err))
				}
			} else {
				require.NoError(t, err)
				if tt.check != nil {
					tt.check(t, info)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Unit tests — DeleteVolume
// ---------------------------------------------------------------------------

func TestVolumeInfra_DeleteVolume(t *testing.T) {
	tests := []struct {
		name          string
		opts          infra.DeleteVolumeOptions
		mountingSbxNs string // if non-empty, create a Sandbox in this ns mounting the PV
		expectError   string
		expectCode    managererrors.ErrorCode
		check         func(t *testing.T, result infra.DeleteVolumeResult)
	}{
		{
			name:        "PV not found",
			opts:        infra.DeleteVolumeOptions{Namespace: "ns1", VolumeID: "ghost-pv"},
			expectError: "not found",
			expectCode:  managererrors.ErrorNotFound,
		},
		{
			name:        "PV not registered under namespace",
			opts:        infra.DeleteVolumeOptions{Namespace: "ns-wrong", VolumeID: "pv-del-ok"},
			expectError: "not found",
			expectCode:  managererrors.ErrorNotFound,
		},
		{
			name:          "mounted without force — blocked",
			opts:          infra.DeleteVolumeOptions{Namespace: "ns1", VolumeID: "pv-del-ok", Force: false},
			mountingSbxNs: "ns1",
			expectError:   "mounted by",
			expectCode:    managererrors.ErrorConflict,
		},
		{
			name:          "mounted with force — succeeds",
			opts:          infra.DeleteVolumeOptions{Namespace: "ns1", VolumeID: "pv-del-ok", Force: true},
			mountingSbxNs: "ns1",
			check: func(t *testing.T, result infra.DeleteVolumeResult) {
				assert.True(t, result.ForcedDelete)
				assert.NotEmpty(t, result.AffectedSandboxIDs)
			},
		},
		{
			name: "unmounted delete — succeeds",
			opts: infra.DeleteVolumeOptions{Namespace: "ns1", VolumeID: "pv-del-ok", Force: false},
			check: func(t *testing.T, result infra.DeleteVolumeResult) {
				assert.False(t, result.ForcedDelete)
				assert.Empty(t, result.AffectedSandboxIDs)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			i, c := NewTestInfra(t)

			// Pre-register pv-del-ok under ns1.
			if tt.opts.VolumeID == "pv-del-ok" {
				pv := makeAvailablePV("pv-del-ok", 10)
				require.NoError(t, c.Create(t.Context(), pv))
				time.Sleep(50 * time.Millisecond)
				_, err := i.RegisterVolume(t.Context(), infra.RegisterVolumeOptions{
					Namespace: "ns1", Name: "vol-del", PvName: "pv-del-ok", SizeGB: 1,
				})
				require.NoError(t, err)
				time.Sleep(50 * time.Millisecond)
			}

			// Create a claimed Sandbox mounting the PV via a SandboxClaim.
			if tt.mountingSbxNs != "" {
				makeClaimedSandboxWithMount(t, c, tt.mountingSbxNs, "claim-mount-001", "sbx-mount-001", "pv-del-ok")
				time.Sleep(50 * time.Millisecond)
			}

			result, err := i.DeleteVolume(t.Context(), tt.opts)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				if tt.expectCode != "" {
					assert.Equal(t, tt.expectCode, managererrors.GetErrCode(err))
				}
			} else {
				require.NoError(t, err)
				if tt.check != nil {
					tt.check(t, result)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Property-based tests
//
// Each test runs propertyIterations rounds. Mount state is established by
// creating claimed Sandbox objects (not annotations), matching the new design.
//
// TODO: replace math/rand with pgregory.net/rapid once vendored.
// ---------------------------------------------------------------------------

const propertyIterations = 100

// TestProperty1_RegistrationConflictOnDuplicatePvName
// Feature: e2b-volume-management, Property 1: Registration conflict on duplicate pvName
// Validates: Requirements 1.6
func TestProperty1_RegistrationConflictOnDuplicatePvName(t *testing.T) {
	i, c := NewTestInfra(t)
	for iter := 0; iter < propertyIterations; iter++ {
		pvName := fmt.Sprintf("pv-p1-%d-%s", iter, randString(6))
		ns := fmt.Sprintf("ns-%s", randString(4))
		volName := fmt.Sprintf("vol-%s", randString(4))

		pv := makeAvailablePV(pvName, 10)
		require.NoError(t, c.Create(t.Context(), pv), "iter %d", iter)
		time.Sleep(50 * time.Millisecond)

		_, err := i.RegisterVolume(t.Context(), infra.RegisterVolumeOptions{
			Namespace: ns, Name: volName, PvName: pvName, SizeGB: 1,
		})
		require.NoError(t, err, "iter %d: first registration failed", iter)

		_, err = i.RegisterVolume(t.Context(), infra.RegisterVolumeOptions{
			Namespace: ns, Name: fmt.Sprintf("vol2-%s", randString(4)), PvName: pvName, SizeGB: 1,
		})
		require.Error(t, err, "iter %d: expected error on duplicate pvName", iter)
		code := managererrors.GetErrCode(err)
		assert.True(t, code == managererrors.ErrorConflict || code == managererrors.ErrorNotAllowed,
			"iter %d: expected Conflict or NotAllowed, got %s", iter, code)
	}
}

// TestProperty2_NamespaceIsolationOfList
// Feature: e2b-volume-management, Property 2: Namespace isolation of list
// Validates: Requirements 2.3, 3.3
func TestProperty2_NamespaceIsolationOfList(t *testing.T) {
	i, c := NewTestInfra(t)
	for iter := 0; iter < propertyIterations; iter++ {
		ns1 := fmt.Sprintf("p2-a-%d-%s", iter, randString(4))
		ns2 := fmt.Sprintf("p2-b-%d-%s", iter, randString(4))
		pvName := fmt.Sprintf("pv-p2-%d-%s", iter, randString(6))

		pv := makeAvailablePV(pvName, 10)
		require.NoError(t, c.Create(t.Context(), pv), "iter %d", iter)
		time.Sleep(50 * time.Millisecond)

		_, err := i.RegisterVolume(t.Context(), infra.RegisterVolumeOptions{
			Namespace: ns1, Name: fmt.Sprintf("vol-%d-%s", iter, randString(4)), PvName: pvName, SizeGB: 1,
		})
		require.NoError(t, err, "iter %d", iter)
		time.Sleep(50 * time.Millisecond)

		result, err := i.ListVolumes(t.Context(), infra.ListVolumesOptions{Namespace: ns2})
		require.NoError(t, err, "iter %d", iter)
		assert.Empty(t, result, "iter %d: expected empty list for ns2", iter)
	}
}

// TestProperty3_GetAfterRegisterRoundTrip
// Feature: e2b-volume-management, Property 3: Get-after-register round-trip
// Validates: Requirements 1.1, 3.1
func TestProperty3_GetAfterRegisterRoundTrip(t *testing.T) {
	i, c := NewTestInfra(t)
	for iter := 0; iter < propertyIterations; iter++ {
		pvName := fmt.Sprintf("pv-p3-%d-%s", iter, randString(6))
		ns := fmt.Sprintf("ns-p3-%s", randString(4))
		volName := fmt.Sprintf("vol-%d-%s", iter, randString(4))
		pvCapGi := rand.Intn(10) + 2
		reqGi := rand.Intn(pvCapGi) + 1

		pv := makeAvailablePV(pvName, pvCapGi)
		require.NoError(t, c.Create(t.Context(), pv), "iter %d", iter)
		time.Sleep(50 * time.Millisecond)

		_, err := i.RegisterVolume(t.Context(), infra.RegisterVolumeOptions{
			Namespace: ns, Name: volName, PvName: pvName, SizeGB: reqGi,
		})
		require.NoError(t, err, "iter %d", iter)
		time.Sleep(50 * time.Millisecond)

		info, err := i.GetVolume(t.Context(), infra.GetVolumeOptions{Namespace: ns, VolumeID: pvName})
		require.NoError(t, err, "iter %d", iter)
		assert.Equal(t, volName, info.Name, "iter %d: name mismatch", iter)
		assert.Equal(t, pvName, info.PvName, "iter %d: pvName mismatch", iter)
		assert.Equal(t, pvCapGi, info.SizeGB, "iter %d: sizeGB mismatch", iter)
	}
}

// TestProperty4_DeleteRemovesFromList
// Feature: e2b-volume-management, Property 4: Delete removes from list
// Validates: Requirements 4.1
func TestProperty4_DeleteRemovesFromList(t *testing.T) {
	i, c := NewTestInfra(t)
	ns := fmt.Sprintf("ns-p4-%s", randString(4))
	for iter := 0; iter < propertyIterations; iter++ {
		pvName := fmt.Sprintf("pv-p4-%d-%s", iter, randString(6))

		pv := makeAvailablePV(pvName, 10)
		require.NoError(t, c.Create(t.Context(), pv), "iter %d", iter)
		time.Sleep(50 * time.Millisecond)

		_, err := i.RegisterVolume(t.Context(), infra.RegisterVolumeOptions{
			Namespace: ns, Name: fmt.Sprintf("vol-p4-%d-%s", iter, randString(4)), PvName: pvName, SizeGB: 1,
		})
		require.NoError(t, err, "iter %d", iter)
		time.Sleep(50 * time.Millisecond)

		_, err = i.DeleteVolume(t.Context(), infra.DeleteVolumeOptions{Namespace: ns, VolumeID: pvName})
		require.NoError(t, err, "iter %d", iter)
		time.Sleep(50 * time.Millisecond)

		volumes, err := i.ListVolumes(t.Context(), infra.ListVolumesOptions{Namespace: ns})
		require.NoError(t, err, "iter %d", iter)
		for _, v := range volumes {
			assert.NotEqual(t, pvName, v.VolumeID, "iter %d: deleted volume still in list", iter)
		}
	}
}

// TestProperty5_MountedVolumeDeletionBlockedWithoutForce
// Feature: e2b-volume-management, Property 5: Mounted volume deletion is blocked
// Validates: Requirements 4.2
// Mount state is derived from a claimed Sandbox, not from a PV annotation.
func TestProperty5_MountedVolumeDeletionBlockedWithoutForce(t *testing.T) {
	i, c := NewTestInfra(t)
	for iter := 0; iter < propertyIterations; iter++ {
		pvName := fmt.Sprintf("pv-p5-%d-%s", iter, randString(6))
		ns := fmt.Sprintf("ns-p5-%d-%s", iter, randString(4))

		pv := makeAvailablePV(pvName, 10)
		require.NoError(t, c.Create(t.Context(), pv), "iter %d", iter)
		time.Sleep(50 * time.Millisecond)

		_, err := i.RegisterVolume(t.Context(), infra.RegisterVolumeOptions{
			Namespace: ns, Name: fmt.Sprintf("vol-%d-%s", iter, randString(4)), PvName: pvName, SizeGB: 1,
		})
		require.NoError(t, err, "iter %d", iter)

		// Create a SandboxClaim with DynamicVolumesMount referencing the PV.
		makeClaimedSandboxWithMount(t, c,
			ns,
			fmt.Sprintf("claim-%s", randString(6)),
			fmt.Sprintf("sbx-%s", randString(6)),
			pvName,
		)
		time.Sleep(50 * time.Millisecond)

		_, err = i.DeleteVolume(t.Context(), infra.DeleteVolumeOptions{
			Namespace: ns, VolumeID: pvName, Force: false,
		})
		require.Error(t, err, "iter %d: expected error", iter)
		assert.Equal(t, managererrors.ErrorConflict, managererrors.GetErrCode(err),
			"iter %d: expected ErrorConflict", iter)
	}
}

// TestProperty7_VolumeNameUniquenessWithinNamespace
// Feature: e2b-volume-management, Property 7: Volume name uniqueness within namespace
// Validates: Requirements 1.7
func TestProperty7_VolumeNameUniquenessWithinNamespace(t *testing.T) {
	i, c := NewTestInfra(t)
	for iter := 0; iter < propertyIterations; iter++ {
		ns := fmt.Sprintf("ns-p7-%d-%s", iter, randString(4))
		volName := fmt.Sprintf("vol-%d-%s", iter, randString(4))
		pv1Name := fmt.Sprintf("pv-p7-a-%d-%s", iter, randString(6))
		pv2Name := fmt.Sprintf("pv-p7-b-%d-%s", iter, randString(6))

		pv1 := makeAvailablePV(pv1Name, 10)
		pv2 := makeAvailablePV(pv2Name, 10)
		require.NoError(t, c.Create(t.Context(), pv1), "iter %d", iter)
		require.NoError(t, c.Create(t.Context(), pv2), "iter %d", iter)
		time.Sleep(50 * time.Millisecond)

		_, err := i.RegisterVolume(t.Context(), infra.RegisterVolumeOptions{
			Namespace: ns, Name: volName, PvName: pv1Name, SizeGB: 1,
		})
		require.NoError(t, err, "iter %d: first registration failed", iter)
		time.Sleep(50 * time.Millisecond)

		_, err = i.RegisterVolume(t.Context(), infra.RegisterVolumeOptions{
			Namespace: ns, Name: volName, PvName: pv2Name, SizeGB: 1,
		})
		require.Error(t, err, "iter %d: expected conflict", iter)
		assert.Equal(t, managererrors.ErrorConflict, managererrors.GetErrCode(err),
			"iter %d: expected ErrorConflict", iter)
	}
}
