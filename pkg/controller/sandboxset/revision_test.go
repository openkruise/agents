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

package sandboxset

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openkruise/agents/api/v1alpha1"
)

// newRevisionTestReconciler builds a Reconciler backed by a fake client for
// revision-related unit tests.
func newRevisionTestReconciler(objs ...client.Object) *Reconciler {
	c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(objs...).Build()
	return &Reconciler{
		Client: c,
		Scheme: testScheme,
		Codec:  serializer.NewCodecFactory(testScheme).LegacyCodec(v1alpha1.SchemeGroupVersion),
	}
}

// samplePodTemplate returns a minimal pod template with the given image and
// optional labels, used to compose SandboxSet / SandboxTemplate fixtures.
func samplePodTemplate(image string, labels map[string]string) *corev1.PodTemplateSpec {
	return &corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: labels},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: image}},
		},
	}
}

func TestReconciler_buildSandboxTemplateSpec(t *testing.T) {
	tests := []struct {
		name        string
		sbs         *v1alpha1.SandboxSet
		objects     []client.Object
		wantErr     bool
		errContains string
		verify      func(t *testing.T, spec *v1alpha1.SandboxTemplateSpec)
	}{
		{
			name: "inline template builds spec with all fields",
			sbs: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{Name: "inline", Namespace: "default"},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 3,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: samplePodTemplate("img:v1", map[string]string{"app": "inline"}),
						VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
							ObjectMeta: metav1.ObjectMeta{Name: "data"},
							Spec: corev1.PersistentVolumeClaimSpec{
								AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
								Resources: corev1.VolumeResourceRequirements{
									Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
								},
							},
						}},
					},
					PersistentContents: []string{"/data"},
					Runtimes:           []v1alpha1.RuntimeConfig{{Name: "python"}},
				},
			},
			verify: func(t *testing.T, spec *v1alpha1.SandboxTemplateSpec) {
				require.NotNil(t, spec.Template)
				assert.Equal(t, "img:v1", spec.Template.Spec.Containers[0].Image)
				assert.Len(t, spec.VolumeClaimTemplates, 1)
				assert.Equal(t, "data", spec.VolumeClaimTemplates[0].Name)
				assert.Equal(t, []string{"/data"}, spec.PersistentContents)
				assert.Len(t, spec.Runtimes, 1)
				assert.Equal(t, "python", spec.Runtimes[0].Name)
			},
		},
		{
			name: "templateRef resolves template and combines with SandboxSet fields",
			sbs: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{Name: "ref-sbs", Namespace: "default"},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 1,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{Name: "tpl-a"},
					},
					PersistentContents: []string{"ip"},
					Runtimes:           []v1alpha1.RuntimeConfig{{Name: "node"}},
				},
			},
			objects: []client.Object{
				&v1alpha1.SandboxTemplate{
					ObjectMeta: metav1.ObjectMeta{Name: "tpl-a", Namespace: "default"},
					Spec: v1alpha1.SandboxTemplateSpec{
						Template: samplePodTemplate("img:v1", map[string]string{"app": "ref"}),
					},
				},
			},
			verify: func(t *testing.T, spec *v1alpha1.SandboxTemplateSpec) {
				require.NotNil(t, spec.Template)
				assert.Equal(t, "img:v1", spec.Template.Spec.Containers[0].Image)
				assert.Equal(t, []string{"ip"}, spec.PersistentContents)
				assert.Len(t, spec.Runtimes, 1)
				assert.Equal(t, "node", spec.Runtimes[0].Name)
			},
		},
		{
			name: "templateRef with nil Template in referenced SandboxTemplate",
			sbs: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{Name: "ref-nil-tpl", Namespace: "default"},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 1,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{Name: "tpl-nil"},
					},
				},
			},
			objects: []client.Object{
				&v1alpha1.SandboxTemplate{
					ObjectMeta: metav1.ObjectMeta{Name: "tpl-nil", Namespace: "default"},
					Spec:       v1alpha1.SandboxTemplateSpec{},
				},
			},
			verify: func(t *testing.T, spec *v1alpha1.SandboxTemplateSpec) {
				assert.Nil(t, spec.Template)
				assert.Nil(t, spec.VolumeClaimTemplates)
				assert.Nil(t, spec.PersistentContents)
				assert.Nil(t, spec.Runtimes)
			},
		},
		{
			name: "templateRef not found propagates a resolve error",
			sbs: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{Name: "ref-missing", Namespace: "default"},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 1,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{Name: "does-not-exist"},
					},
				},
			},
			wantErr:     true,
			errContains: "failed to resolve sandbox template",
		},
		{
			name: "neither template nor templateRef returns an error",
			sbs: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: "default"},
			},
			wantErr:     true,
			errContains: "has neither spec.templateRef nor spec.template",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRevisionTestReconciler(tt.objects...)
			spec, err := r.buildSandboxTemplateSpec(context.Background(), tt.sbs)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
				return
			}
			require.NoError(t, err)
			require.NotNil(t, spec)
			if tt.verify != nil {
				tt.verify(t, spec)
			}
		})
	}
}

// TestComputeRevisionHash covers the hashing behaviour of computeRevisionHash
// across different SandboxTemplateSpec inputs.
func TestComputeRevisionHash(t *testing.T) {
	samePod := samplePodTemplate("img:v1", map[string]string{"app": "same"})

	tests := []struct {
		name        string
		specA       *v1alpha1.SandboxTemplateSpec
		specB       *v1alpha1.SandboxTemplateSpec
		expectEqual bool
	}{
		{
			name: "same template produces identical hash",
			specA: &v1alpha1.SandboxTemplateSpec{
				Template: samePod.DeepCopy(),
			},
			specB: &v1alpha1.SandboxTemplateSpec{
				Template: samePod.DeepCopy(),
			},
			expectEqual: true,
		},
		{
			name: "different images produce different hash",
			specA: &v1alpha1.SandboxTemplateSpec{
				Template: samplePodTemplate("img:v1", nil),
			},
			specB: &v1alpha1.SandboxTemplateSpec{
				Template: samplePodTemplate("img:v2", nil),
			},
			expectEqual: false,
		},
		{
			name: "persistentContents change produces different hash",
			specA: &v1alpha1.SandboxTemplateSpec{
				Template:           samplePodTemplate("img:v1", nil),
				PersistentContents: []string{"/data"},
			},
			specB: &v1alpha1.SandboxTemplateSpec{
				Template: samplePodTemplate("img:v1", nil),
			},
			expectEqual: false,
		},
		{
			name: "runtimes change produces different hash",
			specA: &v1alpha1.SandboxTemplateSpec{
				Template: samplePodTemplate("img:v1", nil),
				Runtimes: []v1alpha1.RuntimeConfig{{Name: "python"}},
			},
			specB: &v1alpha1.SandboxTemplateSpec{
				Template: samplePodTemplate("img:v1", nil),
			},
			expectEqual: false,
		},
		{
			name: "VolumeClaimTemplates change produces different hash",
			specA: &v1alpha1.SandboxTemplateSpec{
				Template: samplePodTemplate("img:v1", nil),
				VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
					ObjectMeta: metav1.ObjectMeta{Name: "data"},
				}},
			},
			specB: &v1alpha1.SandboxTemplateSpec{
				Template: samplePodTemplate("img:v1", nil),
			},
			expectEqual: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hashA, err := computeRevisionHash(tt.specA)
			require.NoError(t, err)
			require.NotEmpty(t, hashA)

			hashB, err := computeRevisionHash(tt.specB)
			require.NoError(t, err)
			require.NotEmpty(t, hashB)

			if tt.expectEqual {
				assert.Equal(t, hashA, hashB,
					"specs describing the same content must share the same hash")
			} else {
				assert.NotEqual(t, hashA, hashB,
					"specs describing different content must have different hashes")
			}
		})
	}
}

// TestComputeRevisionHash_InlineAndTemplateRefConsistency verifies that
// an inline template and a templateRef pointing to identical content produce
// the same revision hash through the full buildSandboxTemplateSpec flow.
func TestComputeRevisionHash_InlineAndTemplateRefConsistency(t *testing.T) {
	samePod := samplePodTemplate("img:v1", map[string]string{"app": "same"})

	inlineSBS := &v1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sbs", Namespace: "default"},
		Spec: v1alpha1.SandboxSetSpec{
			Replicas: 1,
			EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
				Template: samePod.DeepCopy(),
			},
			PersistentContents: []string{"/data"},
		},
	}

	refSBS := &v1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sbs", Namespace: "default"},
		Spec: v1alpha1.SandboxSetSpec{
			Replicas: 1,
			EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
				TemplateRef: &v1alpha1.SandboxTemplateRef{Name: "tpl-dup"},
			},
			PersistentContents: []string{"/data"},
		},
	}

	sbt := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "tpl-dup", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Template: samePod.DeepCopy()},
	}

	r := newRevisionTestReconciler(sbt)

	specInline, err := r.buildSandboxTemplateSpec(context.Background(), inlineSBS)
	require.NoError(t, err)
	hashInline, err := computeRevisionHash(specInline)
	require.NoError(t, err)

	specRef, err := r.buildSandboxTemplateSpec(context.Background(), refSBS)
	require.NoError(t, err)
	hashRef, err := computeRevisionHash(specRef)
	require.NoError(t, err)

	assert.Equal(t, hashInline, hashRef,
		"inline and templateRef describing the same content must produce identical hash")
}

// TestComputeLegacyRevisionHash verifies the backward-compatible legacy hash
// computation that replicates the release-v0.3 algorithm.
func TestComputeLegacyRevisionHash(t *testing.T) {
	tests := []struct {
		name        string
		sbs         *v1alpha1.SandboxSet
		expectEmpty bool
	}{
		{
			name: "inline template produces non-empty hash",
			sbs: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 2,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: samplePodTemplate("img:v1", nil),
					},
				},
			},
		},
		{
			name: "nil template returns empty hash",
			sbs: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{Name: "nil", Namespace: "default"},
				Spec:       v1alpha1.SandboxSetSpec{Replicas: 1},
			},
			expectEmpty: true,
		},
		{
			name: "templateRef returns empty (no legacy compat needed)",
			sbs: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{Name: "ref", Namespace: "default"},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 1,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{Name: "tpl-legacy"},
					},
				},
			},
			expectEmpty: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRevisionTestReconciler()
			hash, err := r.computeLegacyRevisionHash(context.Background(), tt.sbs)
			require.NoError(t, err)
			if tt.expectEmpty {
				assert.Empty(t, hash)
			} else {
				assert.NotEmpty(t, hash)
			}
		})
	}
}

// TestLegacyHashDiffersFromNewHash verifies that the legacy hash and the new
// hash produce different values for the same spec (confirming the need for
// backward compat logic).
func TestLegacyHashDiffersFromNewHash(t *testing.T) {
	sbs := &v1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: v1alpha1.SandboxSetSpec{
			Replicas: 2,
			EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
				Template: samplePodTemplate("img:v1", map[string]string{"app": "test"}),
				VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
					ObjectMeta: metav1.ObjectMeta{Name: "data"},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
						},
					},
				}},
			},
			PersistentContents: []string{"filesystem"},
			Runtimes:           []v1alpha1.RuntimeConfig{{Name: "agent-runtime"}},
		},
	}

	r := newRevisionTestReconciler()

	// Legacy hash
	legacyHash, err := r.computeLegacyRevisionHash(context.Background(), sbs)
	require.NoError(t, err)
	require.NotEmpty(t, legacyHash)

	// New hash
	spec, err := r.buildSandboxTemplateSpec(context.Background(), sbs)
	require.NoError(t, err)
	newHash, err := computeRevisionHash(spec)
	require.NoError(t, err)

	assert.NotEqual(t, legacyHash, newHash,
		"legacy and new hash algorithms must differ for the same spec")
}

// TestLegacyHashChangesWhenTemplateModified verifies that when the SandboxSet
// template is actually modified, the legacy hash changes too. This means old
// sandboxes (carrying the previous legacy hash) won't be falsely compat'd and
// will correctly trigger a rolling update.
func TestLegacyHashChangesWhenTemplateModified(t *testing.T) {
	// Original SandboxSet
	sbsOriginal := &v1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sbs", Namespace: "default"},
		Spec: v1alpha1.SandboxSetSpec{
			Replicas: 3,
			EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
				Template: samplePodTemplate("img:v1", map[string]string{"app": "web"}),
			},
		},
	}

	// Modified SandboxSet (user changed image from v1 to v2)
	sbsModified := &v1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sbs", Namespace: "default"},
		Spec: v1alpha1.SandboxSetSpec{
			Replicas: 3,
			EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
				Template: samplePodTemplate("img:v2", map[string]string{"app": "web"}),
			},
		},
	}

	r := newRevisionTestReconciler()

	// Compute legacy hash for original
	legacyHashOriginal, err := r.computeLegacyRevisionHash(context.Background(), sbsOriginal)
	require.NoError(t, err)
	require.NotEmpty(t, legacyHashOriginal)

	// Compute legacy hash for modified
	legacyHashModified, err := r.computeLegacyRevisionHash(context.Background(), sbsModified)
	require.NoError(t, err)
	require.NotEmpty(t, legacyHashModified)

	// Both new-algorithm hashes
	specOrig, err := r.buildSandboxTemplateSpec(context.Background(), sbsOriginal)
	require.NoError(t, err)
	newHashOriginal, err := computeRevisionHash(specOrig)
	require.NoError(t, err)

	specMod, err := r.buildSandboxTemplateSpec(context.Background(), sbsModified)
	require.NoError(t, err)
	newHashModified, err := computeRevisionHash(specMod)
	require.NoError(t, err)

	// Legacy hash must change when template is modified
	assert.NotEqual(t, legacyHashOriginal, legacyHashModified,
		"legacy hash must change when template is modified")

	// New hash must also change
	assert.NotEqual(t, newHashOriginal, newHashModified,
		"new hash must change when template is modified")

	// Critical: old sandboxes (carrying legacyHashOriginal) should NOT match
	// either the new updateRevision (newHashModified) or the new legacy hash
	// (legacyHashModified). This ensures rolling update is triggered.
	assert.False(t, isRevisionUpdated(legacyHashOriginal, newHashModified, legacyHashModified),
		"old sandboxes must NOT be compat'd after template modification")

	// New sandboxes will carry newHashModified (from updateRevision) which
	// matches the new updateRevision.
	assert.True(t, isRevisionUpdated(newHashModified, newHashModified, legacyHashModified),
		"new sandboxes (carrying new hash) must match updateRevision")
}

// TestLegacyHashStableAcrossCalls verifies idempotency.
func TestLegacyHashStableAcrossCalls(t *testing.T) {
	sbs := &v1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{Name: "stable", Namespace: "default"},
		Spec: v1alpha1.SandboxSetSpec{
			Replicas: 1,
			EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
				Template: samplePodTemplate("img:v1", map[string]string{"app": "stable"}),
			},
		},
	}

	r := newRevisionTestReconciler()
	h1, err := r.computeLegacyRevisionHash(context.Background(), sbs)
	require.NoError(t, err)
	h2, err := r.computeLegacyRevisionHash(context.Background(), sbs)
	require.NoError(t, err)
	assert.Equal(t, h1, h2, "legacy hash must be deterministic")
}

// TestIsRevisionUpdated verifies the helper used in buildUpdateGroups.
func TestIsRevisionUpdated(t *testing.T) {
	tests := []struct {
		name           string
		revision       string
		updateRevision string
		legacyRevision string
		expect         bool
	}{
		{
			name:           "matches updateRevision",
			revision:       "abc123",
			updateRevision: "abc123",
			legacyRevision: "xyz789",
			expect:         true,
		},
		{
			name:           "matches legacyRevision",
			revision:       "xyz789",
			updateRevision: "abc123",
			legacyRevision: "xyz789",
			expect:         true,
		},
		{
			name:           "matches neither",
			revision:       "old-old",
			updateRevision: "abc123",
			legacyRevision: "xyz789",
			expect:         false,
		},
		{
			name:           "empty legacyRevision only checks updateRevision",
			revision:       "abc123",
			updateRevision: "abc123",
			legacyRevision: "",
			expect:         true,
		},
		{
			name:           "empty legacyRevision does not match arbitrary revision",
			revision:       "something",
			updateRevision: "abc123",
			legacyRevision: "",
			expect:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isRevisionUpdated(tt.revision, tt.updateRevision, tt.legacyRevision)
			assert.Equal(t, tt.expect, got)
		})
	}
}

// TestBuildUpdateGroupsWithLegacyRevision tests that sandboxes carrying the
// legacy hash are correctly classified as Updated rather than Old.
func TestBuildUpdateGroupsWithLegacyRevision(t *testing.T) {
	now := metav1.Now().Time
	newRev := "new-hash"
	legacyRev := "66df5848ff"
	oldRev := "completely-stale"

	tests := []struct {
		name                  string
		groups                GroupedSandboxes
		expectUpdatedCreating int
		expectUpdatedAvail    int
		expectOldCreating     int
		expectOldAvail        int
	}{
		{
			name: "legacy hash sandboxes treated as updated",
			groups: GroupedSandboxes{
				Available: []*v1alpha1.Sandbox{
					newSandbox("a1", legacyRev, v1alpha1.SandboxStateAvailable, false, now),
					newSandbox("a2", legacyRev, v1alpha1.SandboxStateAvailable, false, now),
				},
			},
			expectUpdatedAvail: 2,
		},
		{
			name: "new hash sandboxes still treated as updated",
			groups: GroupedSandboxes{
				Creating: []*v1alpha1.Sandbox{
					newSandbox("c1", newRev, v1alpha1.SandboxStateCreating, false, now),
				},
			},
			expectUpdatedCreating: 1,
		},
		{
			name: "truly old sandboxes still flagged as old",
			groups: GroupedSandboxes{
				Available: []*v1alpha1.Sandbox{
					newSandbox("a1", oldRev, v1alpha1.SandboxStateAvailable, false, now),
				},
			},
			expectOldAvail: 1,
		},
		{
			name: "mixed legacy and new hashes all treated as updated",
			groups: GroupedSandboxes{
				Available: []*v1alpha1.Sandbox{
					newSandbox("a1", legacyRev, v1alpha1.SandboxStateAvailable, false, now),
					newSandbox("a2", newRev, v1alpha1.SandboxStateAvailable, false, now),
					newSandbox("a3", oldRev, v1alpha1.SandboxStateAvailable, false, now),
				},
			},
			expectUpdatedAvail: 2,
			expectOldAvail:     1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ug := buildUpdateGroups(tt.groups, newRev, legacyRev)
			require.NotNil(t, ug)
			assert.Equal(t, tt.expectUpdatedCreating, len(ug.UpdatedCreating))
			assert.Equal(t, tt.expectUpdatedAvail, len(ug.UpdatedAvailable))
			assert.Equal(t, tt.expectOldCreating, len(ug.OldCreating))
			assert.Equal(t, tt.expectOldAvail, len(ug.OldAvailable))
		})
	}
}

// TestTemplateModificationTriggersRollingUpdateEndToEnd simulates an end-to-end
// scenario: SandboxSet template is modified, old sandboxes (with the OLD legacy
// hash) should be flagged as Old, and new sandboxes get the new updateRevision.
func TestTemplateModificationTriggersRollingUpdateEndToEnd(t *testing.T) {
	now := metav1.Now().Time

	// Simulate: original SandboxSet with img:v1
	sbsOriginal := &v1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sbs", Namespace: "default"},
		Spec: v1alpha1.SandboxSetSpec{
			Replicas: 3,
			EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
				Template: samplePodTemplate("img:v1", map[string]string{"app": "web"}),
			},
		},
	}

	// User modifies template: img:v1 → img:v2
	sbsModified := &v1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sbs", Namespace: "default"},
		Spec: v1alpha1.SandboxSetSpec{
			Replicas: 3,
			EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
				Template: samplePodTemplate("img:v2", map[string]string{"app": "web"}),
			},
		},
	}

	r := newRevisionTestReconciler()

	// Step 1: Compute the old legacy hash (what existing sandboxes carry)
	oldLegacyHash, err := r.computeLegacyRevisionHash(context.Background(), sbsOriginal)
	require.NoError(t, err)
	require.NotEmpty(t, oldLegacyHash)

	// Step 2: After modification, compute new updateRevision and new legacy hash
	newSpec, err := r.buildSandboxTemplateSpec(context.Background(), sbsModified)
	require.NoError(t, err)
	newUpdateRevision, err := computeRevisionHash(newSpec)
	require.NoError(t, err)

	newLegacyHash, err := r.computeLegacyRevisionHash(context.Background(), sbsModified)
	require.NoError(t, err)

	// Step 3: Old sandboxes carry oldLegacyHash. After modification,
	// buildUpdateGroups should classify them as Old.
	groups := GroupedSandboxes{
		Available: []*v1alpha1.Sandbox{
			newSandbox("old-1", oldLegacyHash, v1alpha1.SandboxStateAvailable, false, now),
			newSandbox("old-2", oldLegacyHash, v1alpha1.SandboxStateAvailable, false, now),
			newSandbox("old-3", oldLegacyHash, v1alpha1.SandboxStateAvailable, false, now),
		},
	}

	ug := buildUpdateGroups(groups, newUpdateRevision, newLegacyHash)
	require.NotNil(t, ug)

	// All old sandboxes should be in OldAvailable (rolling update needed)
	assert.Equal(t, 3, len(ug.OldAvailable),
		"old sandboxes must be flagged for rolling update after template modification")
	assert.Equal(t, 0, len(ug.UpdatedAvailable),
		"no sandbox should be treated as updated")

	// Step 4: After rolling update, new sandboxes carry newUpdateRevision.
	// Verify they're correctly classified as Updated.
	newGroups := GroupedSandboxes{
		Available: []*v1alpha1.Sandbox{
			newSandbox("new-1", newUpdateRevision, v1alpha1.SandboxStateAvailable, false, now),
			newSandbox("new-2", newUpdateRevision, v1alpha1.SandboxStateAvailable, false, now),
			newSandbox("new-3", newUpdateRevision, v1alpha1.SandboxStateAvailable, false, now),
		},
	}

	ugNew := buildUpdateGroups(newGroups, newUpdateRevision, newLegacyHash)
	require.NotNil(t, ugNew)
	assert.Equal(t, 3, len(ugNew.UpdatedAvailable),
		"new sandboxes carrying updateRevision must be classified as Updated")
	assert.Equal(t, 0, len(ugNew.OldAvailable))
}

func TestComputeUpdateRevision(t *testing.T) {
	sbs := &v1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "code-interpreter",
			Namespace: "default",
			UID:       types.UID("test-uid-123"),
		},
		Spec: v1alpha1.SandboxSetSpec{
			Replicas:           3,
			PersistentContents: []string{"filesystem"},
			Runtimes: []v1alpha1.RuntimeConfig{
				{Name: "agent-runtime"},
				{Name: "csi"},
			},
			EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app": "code-interpreter",
							"env": "production",
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "sandbox",
								Image: "registry.example.com/sandbox/code-interpreter:v1.3",
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("1"),
										corev1.ResourceMemory: resource.MustParse("1Gi"),
									},
									Limits: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("2"),
										corev1.ResourceMemory: resource.MustParse("2Gi"),
									},
								},
								Ports: []corev1.ContainerPort{
									{Name: "interpreter", ContainerPort: 49999},
									{Name: "envd", ContainerPort: 49983},
								},
							},
						},
						TerminationGracePeriodSeconds: ptr.To(int64(1)),
					},
				},
			},
		},
	}

	// testUpdateRevision is the golden value produced by the legacy hash algorithm
	// (release-v0.3) for the above SandboxSet spec. It was captured from a real
	// production cluster and MUST NEVER be changed.
	//
	// If this test fails, it almost certainly means the computeLegacyRevisionHash
	// implementation has regressed — fix the code, NOT this constant. Changing
	// this value would break backward compatibility with all existing sandboxes
	// created before the hash algorithm migration.
	const testUpdateRevision = "5bf5fbb5d6"

	r := newRevisionTestReconciler()
	legacyHash, err := r.computeLegacyRevisionHash(context.Background(), sbs)
	require.NoError(t, err)
	assert.Equal(t, testUpdateRevision, legacyHash,
		"computeLegacyRevisionHash must produce the same hash as the legacy algorithm")
}
