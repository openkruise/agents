package k8s

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestSetPodCondition(t *testing.T) {
	tests := []struct {
		name          string
		initialPod    *corev1.Pod
		conditionType corev1.PodConditionType
		status        corev1.ConditionStatus
		reason        string
		message       string
		wantExists    bool
	}{
		{
			name: "add new condition",
			initialPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{},
				},
			},
			conditionType: corev1.PodReady,
			status:        corev1.ConditionTrue,
			reason:        "Ready",
			message:       "Pod is ready",
			wantExists:    true,
		},
		{
			name: "update existing condition",
			initialPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionFalse,
						},
					},
				},
			},
			conditionType: corev1.PodReady,
			status:        corev1.ConditionTrue,
			reason:        "Ready",
			message:       "Pod is ready",
			wantExists:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			SetPodCondition(tt.initialPod, tt.conditionType, tt.status, tt.reason, tt.message)

			condition, exists := GetPodCondition(tt.initialPod, tt.conditionType)
			if exists != tt.wantExists {
				t.Errorf("Condition existence = %v, want %v", exists, tt.wantExists)
			}

			if tt.wantExists {
				if condition.Type != tt.conditionType {
					t.Errorf("Condition type = %v, want %v", condition.Type, tt.conditionType)
				}
				if condition.Status != tt.status {
					t.Errorf("Condition status = %v, want %v", condition.Status, tt.status)
				}
				if condition.Reason != tt.reason {
					t.Errorf("Condition reason = %v, want %v", condition.Reason, tt.reason)
				}
				if condition.Message != tt.message {
					t.Errorf("Condition message = %v, want %v", condition.Message, tt.message)
				}
				if condition.LastTransitionTime.IsZero() {
					t.Error("LastTransitionTime should not be zero")
				}
			}
		})
	}
}

func TestGetPodCondition(t *testing.T) {
	tests := []struct {
		name          string
		pod           *corev1.Pod
		conditionType corev1.PodConditionType
		wantCondition corev1.PodCondition
		wantExists    bool
	}{
		{
			name: "condition exists",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						},
					},
				},
			},
			conditionType: corev1.PodReady,
			wantCondition: corev1.PodCondition{
				Type:   corev1.PodReady,
				Status: corev1.ConditionTrue,
			},
			wantExists: true,
		},
		{
			name: "condition does not exist",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{},
				},
			},
			conditionType: corev1.PodReady,
			wantCondition: corev1.PodCondition{},
			wantExists:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotCondition, gotExists := GetPodCondition(tt.pod, tt.conditionType)

			if gotExists != tt.wantExists {
				t.Errorf("GetPodCondition() exists = %v, want %v", gotExists, tt.wantExists)
			}

			if tt.wantExists {
				if gotCondition.Type != tt.wantCondition.Type {
					t.Errorf("GetPodCondition() condition type = %v, want %v", gotCondition.Type, tt.wantCondition.Type)
				}
				if gotCondition.Status != tt.wantCondition.Status {
					t.Errorf("GetPodCondition() condition status = %v, want %v", gotCondition.Status, tt.wantCondition.Status)
				}
			}
		})
	}
}
