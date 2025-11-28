//goland:noinspection GoDeprecation
package sandboxcr

import (
	"testing"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
			name:       "添加新条件",
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
			name: "更新现有条件",
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
			name: "添加条件到现有条件列表",
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
			// 执行测试
			SetSandboxCondition(tt.initialSbx, tt.tp, tt.status, tt.reason, tt.message)

			// 验证结果
			assert.Equal(t, tt.expectedCount, len(tt.initialSbx.Status.Conditions))

			// 查找我们设置的条件
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
		name          string
		sbx           *v1alpha1.Sandbox
		tp            v1alpha1.SandboxConditionType
		expectedCond  metav1.Condition
		expectedFound bool
	}{
		{
			name: "找到条件",
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
			expectedFound: true,
		},
		{
			name: "未找到条件",
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
			tp:            "Ready",
			expectedCond:  metav1.Condition{},
			expectedFound: false,
		},
		{
			name:          "空条件列表",
			sbx:           &v1alpha1.Sandbox{},
			tp:            "Ready",
			expectedCond:  metav1.Condition{},
			expectedFound: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 执行测试
			cond, found := GetSandboxCondition(tt.sbx, tt.tp)

			// 验证结果
			assert.Equal(t, tt.expectedFound, found)
			if tt.expectedFound {
				assert.Equal(t, tt.expectedCond.Type, cond.Type)
				assert.Equal(t, tt.expectedCond.Status, cond.Status)
				assert.Equal(t, tt.expectedCond.Reason, cond.Reason)
			} else {
				assert.Equal(t, tt.expectedCond, cond)
			}
		})
	}
}

func TestFindSandboxGroup(t *testing.T) {
	currentTime := metav1.Now()
	testHash := "test-hash"

	tests := []struct {
		name           string
		sbx            *v1alpha1.Sandbox
		updateHash     string
		expectedGroup  string
		expectedReason string
	}{
		{
			name: "Failed: ResourceDeleted",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					DeletionTimestamp: &currentTime,
				},
			},
			updateHash:     testHash,
			expectedGroup:  GroupFailed,
			expectedReason: "ResourceDeleted",
		},
		{
			name: "Failed: LegacyHash",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha1.LabelTemplateHash: "old-hash",
					},
				},
			},
			updateHash:     testHash,
			expectedGroup:  GroupFailed,
			expectedReason: "LegacyHash",
		},
		{
			name: "Creating: ResourcePending",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha1.LabelTemplateHash: testHash,
					},
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxPending,
				},
			},
			updateHash:     testHash,
			expectedGroup:  GroupCreating,
			expectedReason: "ResourcePending",
		},
		{
			name: "Failed: ResourceFailed",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha1.LabelTemplateHash: testHash,
					},
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxFailed,
				},
			},
			updateHash:     testHash,
			expectedGroup:  GroupFailed,
			expectedReason: "ResourceFailed",
		},
		{
			name: "Failed: ResourceSucceeded",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha1.LabelTemplateHash: testHash,
					},
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxSucceeded,
				},
			},
			updateHash:     testHash,
			expectedGroup:  GroupFailed,
			expectedReason: "ResourceSucceeded",
		},
		{
			name: "Failed: ResourceTerminating",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha1.LabelTemplateHash: testHash,
					},
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxTerminating,
				},
			},
			updateHash:     testHash,
			expectedGroup:  GroupFailed,
			expectedReason: "ResourceTerminating",
		},
		{
			name: "Claimed: SandboxRunning",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha1.LabelTemplateHash: testHash,
						v1alpha1.LabelSandboxState: v1alpha1.SandboxStateRunning,
					},
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxRunning,
				},
			},
			updateHash:     testHash,
			expectedGroup:  GroupClaimed,
			expectedReason: "SandboxRunning",
		},
		{
			name: "Claimed: SandboxPaused",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha1.LabelTemplateHash: testHash,
						v1alpha1.LabelSandboxState: v1alpha1.SandboxStatePaused,
					},
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxRunning,
				},
			},
			updateHash:     testHash,
			expectedGroup:  GroupClaimed,
			expectedReason: "SandboxPaused",
		},
		{
			name: "Available: ",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha1.LabelTemplateHash: testHash,
						v1alpha1.LabelSandboxState: v1alpha1.SandboxStateAvailable,
					},
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxRunning,
				},
			},
			updateHash:     testHash,
			expectedGroup:  GroupAvailable,
			expectedReason: "SandboxAvailable",
		},
		{
			name: "Failed: SandboxKilling",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha1.LabelTemplateHash: testHash,
						v1alpha1.LabelSandboxState: v1alpha1.SandboxStateKilling,
					},
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxRunning,
				},
			},
			updateHash:     testHash,
			expectedGroup:  GroupFailed,
			expectedReason: "SandboxKilling",
		},
		{
			name: "Creating: ResourcePending",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha1.LabelTemplateHash: testHash,
					},
				},
			},
			updateHash:     testHash,
			expectedGroup:  GroupCreating,
			expectedReason: "ResourcePending",
		},
		{
			name: "Unknown",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha1.LabelTemplateHash: testHash,
						v1alpha1.LabelSandboxState: "unknown-state",
					},
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxRunning,
				},
			},
			updateHash:     testHash,
			expectedGroup:  GroupUnknown,
			expectedReason: "Unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 执行测试
			group, reason := FindSandboxGroup(tt.sbx, tt.updateHash)

			// 验证结果
			assert.Equal(t, tt.expectedGroup, group)
			assert.Equal(t, tt.expectedReason, reason)
		})
	}
}
