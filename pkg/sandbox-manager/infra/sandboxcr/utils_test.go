//goland:noinspection GoDeprecation
package sandboxcr

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

func TestSetSandboxCondition(t *testing.T) {
	tests := []struct {
		name          string
		initialSbx    *v1alpha1.Sandbox
		tp            string
		status        metav1.ConditionStatus
		reason        string
		message       string
		expectedCond  metav1.Condition
		expectedCount int
	}{
		{
			name:       "Add new condition",
			initialSbx: &v1alpha1.Sandbox{},
			tp:         "Ready",
			status:     metav1.ConditionTrue,
			reason:     "PodReady",
			message:    "Pod is ready",
			expectedCond: metav1.Condition{
				Type:   "Ready",
				Status: metav1.ConditionTrue,
				Reason: "PodReady",
			},
			expectedCount: 1,
		},
		{
			name: "Update existing condition",
			initialSbx: &v1alpha1.Sandbox{
				Status: v1alpha1.SandboxStatus{
					Conditions: []metav1.Condition{
						{
							Type:   "Ready",
							Status: metav1.ConditionFalse,
							Reason: "PodNotReady",
						},
					},
				},
			},
			tp:      "Ready",
			status:  metav1.ConditionTrue,
			reason:  "PodReady",
			message: "Pod is ready",
			expectedCond: metav1.Condition{
				Type:   "Ready",
				Status: metav1.ConditionTrue,
				Reason: "PodReady",
			},
			expectedCount: 1,
		},
		{
			name: "Add condition to existing list",
			initialSbx: &v1alpha1.Sandbox{
				Status: v1alpha1.SandboxStatus{
					Conditions: []metav1.Condition{
						{
							Type:   "Initialized",
							Status: metav1.ConditionTrue,
						},
					},
				},
			},
			tp:      "Ready",
			status:  metav1.ConditionTrue,
			reason:  "PodReady",
			message: "Pod is ready",
			expectedCond: metav1.Condition{
				Type:   "Ready",
				Status: metav1.ConditionTrue,
				Reason: "PodReady",
			},
			expectedCount: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Execute test
			SetSandboxCondition(tt.initialSbx, tt.tp, tt.status, tt.reason, tt.message)

			// Verify results
			assert.Equal(t, tt.expectedCount, len(tt.initialSbx.Status.Conditions))

			// Find the condition we set
			var foundCond *metav1.Condition
			for _, cond := range tt.initialSbx.Status.Conditions {
				if cond.Type == tt.tp {
					foundCond = &cond
					break
				}
			}

			assert.NotNil(t, foundCond)
			assert.Equal(t, tt.expectedCond.Type, foundCond.Type)
			assert.Equal(t, tt.expectedCond.Status, foundCond.Status)
			assert.Equal(t, tt.expectedCond.Reason, foundCond.Reason)
			assert.Equal(t, tt.message, foundCond.Message)
			assert.False(t, foundCond.LastTransitionTime.IsZero())
		})
	}
}

func TestGetSandboxCondition(t *testing.T) {
	tests := []struct {
		name         string
		sbx          *v1alpha1.Sandbox
		tp           v1alpha1.SandboxConditionType
		expectedCond metav1.Condition
	}{
		{
			name: "Find condition",
			sbx: &v1alpha1.Sandbox{
				Status: v1alpha1.SandboxStatus{
					Conditions: []metav1.Condition{
						{
							Type:   "Ready",
							Status: metav1.ConditionTrue,
							Reason: "PodReady",
						},
					},
				},
			},
			tp: "Ready",
			expectedCond: metav1.Condition{
				Type:   "Ready",
				Status: metav1.ConditionTrue,
				Reason: "PodReady",
			},
		},
		{
			name: "Condition not found",
			sbx: &v1alpha1.Sandbox{
				Status: v1alpha1.SandboxStatus{
					Conditions: []metav1.Condition{
						{
							Type:   "Initialized",
							Status: metav1.ConditionTrue,
						},
					},
				},
			},
			tp:           "Ready",
			expectedCond: metav1.Condition{},
		},
		{
			name:         "Empty condition list",
			sbx:          &v1alpha1.Sandbox{},
			tp:           "Ready",
			expectedCond: metav1.Condition{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Execute test
			cond := GetSandboxCondition(tt.sbx, tt.tp)

			// Verify results
			assert.Equal(t, tt.expectedCond, cond)
		})
	}
}

func TestGetCsiMountExtensionRequest(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		expectNil   bool
		expectError bool
		errorMsg    string
		expectCount int
	}{
		{
			name:        "no csi mount annotation",
			annotations: map[string]string{},
			expectNil:   true,
			expectError: false,
			expectCount: 0,
		},
		{
			name: "empty csi mount annotation",
			annotations: map[string]string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig: "",
			},
			expectNil:   true,
			expectError: false,
			expectCount: 0,
		},
		{
			name: "valid csi mount config with multiple entries",
			annotations: map[string]string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig: `[{"mountID":"","pvName":"oss-pv-sandbox-system-hangzhou","mountPath":"/dir1/u1/v1","subPath":"jicheng-1","readOnly":true},{"mountID":"","pvName":"oss-pv-sandbox-system-hangzhou","mountPath":"/dir2/u2","subPath":"jicheng-2","readOnly":false},{"mountID":"","pvName":"oss-pv-sandbox-system-hangzhou","mountPath":"/dir3","subPath":"jicheng-3","readOnly":true}]`,
			},
			expectNil:   false,
			expectError: false,
			expectCount: 3,
		},
		{
			name: "valid csi mount config with single entry",
			annotations: map[string]string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig: `[{"mountID":"mount-1","pvName":"pv-1","mountPath":"/mnt/data","subPath":"subpath-1","readOnly":false}]`,
			},
			expectNil:   false,
			expectError: false,
			expectCount: 1,
		},
		{
			name: "invalid json format",
			annotations: map[string]string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig: `invalid-json-format`,
			},
			expectNil:   true,
			expectError: true,
			errorMsg:    "failed to unmarshal csi mount options",
		},
		{
			name: "empty array",
			annotations: map[string]string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig: `[]`,
			},
			expectNil:   true,
			expectError: false,
			expectCount: 0,
		},
		{
			name: "valid csi mount with all fields populated",
			annotations: map[string]string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig: `[{"mountID":"mount-123","pvName":"nfs-pv-data","mountPath":"/var/lib/data","subPath":"user/project","readOnly":true}]`,
			},
			expectNil:   false,
			expectError: false,
			expectCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandbox := &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: tt.annotations,
				},
			}

			result, err := getCsiMountExtensionRequest(sandbox)

			if tt.expectError {
				assert.Error(t, err)
				if tt.errorMsg != "" {
					assert.Contains(t, err.Error(), tt.errorMsg)
				}
				assert.Nil(t, result)
				return
			}

			assert.NoError(t, err)

			if tt.expectNil {
				assert.Empty(t, result)
			} else {
				assert.NotNil(t, result)
				assert.Len(t, result, tt.expectCount)
			}

			if len(result) > 0 {
				for i, config := range result {
					assert.NotEmpty(t, config.PvName, "pvName should not be empty at index %d", i)
					assert.NotEmpty(t, config.MountPath, "mountPath should not be empty at index %d", i)
				}
			}
		})
	}
}

func TestGetCsiMountExtensionRequest_v2(t *testing.T) {
	const csiVolumeConfigAnnotation = `[{"mountID":"","pvName":"oss-pv-sandbox-system-hangzhou","mountPath":"/dir1/u1/v1","subPath":"jicheng-1","readOnly":true},{"mountID":"","pvName":"oss-pv-sandbox-system-hangzhou","mountPath":"/dir2/u2","subPath":"jicheng-2","readOnly":false},{"mountID":"","pvName":"oss-pv-sandbox-system-hangzhou","mountPath":"/dir3","subPath":"jicheng-3","readOnly":true}]`

	tests := []struct {
		name        string
		annotations map[string]string
		expectNil   bool
		expectError bool
		errorMsg    string
		expectCount int
	}{
		{
			name:        "no csi mount annotation",
			annotations: map[string]string{},
			expectNil:   true,
			expectError: false,
			expectCount: 0,
		},
		{
			name: "empty csi mount annotation",
			annotations: map[string]string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig: "",
			},
			expectNil:   true,
			expectError: false,
			expectCount: 0,
		},
		{
			name: "valid csi mount config with multiple entries from real scenario",
			annotations: map[string]string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig: csiVolumeConfigAnnotation,
			},
			expectNil:   false,
			expectError: false,
			expectCount: 3,
		},
		{
			name: "valid csi mount config with single entry",
			annotations: map[string]string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig: `[{"mountID":"mount-1","pvName":"pv-1","mountPath":"/mnt/data","subPath":"subpath-1","readOnly":false}]`,
			},
			expectNil:   false,
			expectError: false,
			expectCount: 1,
		},
		{
			name: "invalid json format",
			annotations: map[string]string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig: `invalid-json-format`,
			},
			expectNil:   true,
			expectError: true,
			errorMsg:    "failed to unmarshal csi mount options",
		},
		{
			name: "empty array",
			annotations: map[string]string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig: `[]`,
			},
			expectNil:   true,
			expectError: false,
			expectCount: 0,
		},
		{
			name: "valid csi mount with all fields populated",
			annotations: map[string]string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig: `[{"mountID":"mount-123","pvName":"nfs-pv-data","mountPath":"/var/lib/data","subPath":"user/project","readOnly":true}]`,
			},
			expectNil:   false,
			expectError: false,
			expectCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandbox := &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: tt.annotations,
				},
			}

			result, err := getCsiMountExtensionRequest(sandbox)

			if tt.expectError {
				assert.Error(t, err)
				if tt.errorMsg != "" {
					assert.Contains(t, err.Error(), tt.errorMsg)
				}
				assert.Nil(t, result)
				return
			}

			assert.NoError(t, err)

			if tt.expectNil {
				assert.Empty(t, result)
			} else {
				assert.NotNil(t, result)
				assert.Len(t, result, tt.expectCount)
			}

			if len(result) > 0 {
				for i, config := range result {
					assert.NotEmpty(t, config.PvName, "pvName should not be empty at index %d", i)
					assert.NotEmpty(t, config.MountPath, "mountPath should not be empty at index %d", i)
				}
			}

			if tt.name == "valid csi mount config with multiple entries from real scenario" {
				assert.Equal(t, "oss-pv-sandbox-system-hangzhou", result[0].PvName)
				assert.Equal(t, "/dir1/u1/v1", result[0].MountPath)
				assert.Equal(t, "jicheng-1", result[0].SubPath)
				assert.True(t, result[0].ReadOnly)

				assert.Equal(t, "oss-pv-sandbox-system-hangzhou", result[1].PvName)
				assert.Equal(t, "/dir2/u2", result[1].MountPath)
				assert.Equal(t, "jicheng-2", result[1].SubPath)
				assert.False(t, result[1].ReadOnly)

				assert.Equal(t, "oss-pv-sandbox-system-hangzhou", result[2].PvName)
				assert.Equal(t, "/dir3", result[2].MountPath)
				assert.Equal(t, "jicheng-3", result[2].SubPath)
				assert.True(t, result[2].ReadOnly)
			}
		})
	}
}