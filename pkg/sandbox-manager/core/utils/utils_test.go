package utils

import (
	"context"
	"testing"

	"github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	infra2 "github.com/openkruise/agents/pkg/sandbox-manager/core/infra"
	utils2 "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

func TestValidatedCustomLabelKey(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{
			name:    "valid key",
			key:     "app",
			wantErr: false,
		},
		{
			name:    "reserved key",
			key:     consts.InternalPrefix,
			wantErr: true,
		},
		{
			name:    "key with reserved prefix",
			key:     consts.InternalPrefix + "test",
			wantErr: false, // This should pass because the check is for exact prefix match
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatedCustomLabelKey(tt.key)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidatedCustomLabelKey() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSandboxClaimed(t *testing.T) {
	tests := []struct {
		name  string
		state string
		want  bool
	}{
		{
			name:  "running state",
			state: consts.SandboxStateRunning,
			want:  true,
		},
		{
			name:  "paused state",
			state: consts.SandboxStatePaused,
			want:  true,
		},
		{
			name:  "other state",
			state: "other",
			want:  false,
		},
		{
			name:  "empty state",
			state: "",
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := utils2.FakeSandbox{State: tt.state}
			if got := SandboxClaimed(sbx); got != tt.want {
				t.Errorf("SandboxClaimed() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCalculateExpectPoolSize(t *testing.T) {
	tests := []struct {
		name    string
		total   int32
		pending int32
		minSize int32
		maxSize int32
		usage   string
		expect  int32
		wantErr bool
	}{
		{
			name:    "no scale",
			total:   4,
			pending: 2,
			minSize: 1,
			maxSize: 10,
			usage:   "50%",
			expect:  4,
		},
		{
			name:    "scale up",
			total:   4,
			pending: 0,
			minSize: 1,
			maxSize: 10,
			usage:   "50%",
			expect:  6,
		},
		{
			name:    "limited by max size",
			total:   4,
			pending: 0,
			minSize: 1,
			maxSize: 5,
			usage:   "50%",
			expect:  5,
		},
		{
			name:    "exceeds max size",
			total:   4,
			pending: 0,
			minSize: 1,
			maxSize: 3,
			usage:   "50%",
			expect:  3,
		},
		{
			name:    "scale down",
			total:   4,
			pending: 4,
			minSize: 0,
			maxSize: 5,
			usage:   "50%",
			expect:  2, // 预期利用数为 2，实际为 0，缩容 offset 2
		},
		{
			name:    "limited by min size",
			total:   4,
			pending: 4,
			minSize: 3,
			maxSize: 5,
			usage:   "50%",
			expect:  3,
		},
		{
			name:    "exceeds min size",
			total:   4,
			pending: 4,
			minSize: 5,
			maxSize: 10,
			usage:   "50%",
			expect:  5,
		},
		{
			name:    "partial scale up",
			total:   2,
			pending: 0,
			minSize: 2,
			maxSize: 5,
			usage:   "50%",
			expect:  3, // 实际利用数为 2，预期利用数为 1，扩容预期的 1
		},
		{
			name:    "bad usage",
			total:   4,
			pending: 2,
			minSize: 1,
			maxSize: 10,
			usage:   "abcd",
			expect:  4,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			size, err := CalculateExpectPoolSize(ctx, tt.total, tt.pending, &infra2.SandboxTemplate{
				Spec: infra2.SandboxTemplateSpec{
					MinPoolSize: tt.minSize,
					MaxPoolSize: tt.maxSize,
					ExpectUsage: ptr.To(intstr.Parse(tt.usage)),
				},
			})
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expect, size)
			}
		})
	}
}

func TestCalculateResourceFromContainers(t *testing.T) {
	cpuQuantity1, _ := resource.ParseQuantity("1000m")
	cpuQuantity2, _ := resource.ParseQuantity("500m")
	memoryQuantity1, _ := resource.ParseQuantity("1024Mi")
	memoryQuantity2, _ := resource.ParseQuantity("512Mi")

	tests := []struct {
		name string
		pod  *corev1.Pod
		want infra2.SandboxResource
	}{
		{
			name: "single container with resources",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    cpuQuantity1,
									corev1.ResourceMemory: memoryQuantity1,
								},
							},
						},
					},
				},
			},
			want: infra2.SandboxResource{
				CPUMilli: 1000,
				MemoryMB: 1024,
			},
		},
		{
			name: "multiple containers with resources",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    cpuQuantity1,
									corev1.ResourceMemory: memoryQuantity1,
								},
							},
						},
						{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    cpuQuantity2,
									corev1.ResourceMemory: memoryQuantity2,
								},
							},
						},
					},
				},
			},
			want: infra2.SandboxResource{
				CPUMilli: 1500,
				MemoryMB: 1536,
			},
		},
		{
			name: "no containers",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{},
				},
			},
			want: infra2.SandboxResource{
				CPUMilli: 0,
				MemoryMB: 0,
			},
		},
		{
			name: "containers without resources",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{},
							},
						},
					},
				},
			},
			want: infra2.SandboxResource{
				CPUMilli: 0,
				MemoryMB: 0,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CalculateResourceFromContainers(tt.pod.Spec.Containers)
			if got.CPUMilli != tt.want.CPUMilli {
				t.Errorf("GetResource().CPUMilli = %v, want %v", got.CPUMilli, tt.want.CPUMilli)
			}
			if got.MemoryMB != tt.want.MemoryMB {
				t.Errorf("GetResource().MemoryMB = %v, want %v", got.MemoryMB, tt.want.MemoryMB)
			}
		})
	}
}
