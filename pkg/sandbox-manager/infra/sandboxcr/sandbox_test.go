package sandboxcr

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/client/clientset/versioned/fake"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	utils2 "github.com/openkruise/agents/pkg/utils"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func AsSandboxForTest(sbx *v1alpha1.Sandbox, client *fake.Clientset, cache *Cache) *Sandbox {
	s := &Sandbox{
		BaseSandbox: BaseSandbox[*v1alpha1.Sandbox]{
			Sandbox:       sbx,
			Cache:         cache,
			Client:        client,
			SetCondition:  SetSandboxCondition,
			GetConditions: ListSandboxConditions,
			DeepCopy:      DeepCopy,
		},
		Sandbox: sbx,
	}
	if client != nil {
		s.PatchSandbox = client.ApiV1alpha1().Sandboxes("default").Patch
		s.Update = client.ApiV1alpha1().Sandboxes("default").Update
		s.DeleteFunc = client.ApiV1alpha1().Sandboxes("default").Delete
	}
	return s
}

func ConvertPodToSandboxCR(pod *corev1.Pod) *v1alpha1.Sandbox {
	sbx := &v1alpha1.Sandbox{
		ObjectMeta: pod.ObjectMeta,
		Spec: v1alpha1.SandboxSpec{
			SandboxTemplate: v1alpha1.SandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: pod.Spec,
				},
			},
		},
		Status: v1alpha1.SandboxStatus{
			Phase: v1alpha1.SandboxPhase(pod.Status.Phase),
			PodInfo: v1alpha1.PodInfo{
				PodIP: pod.Status.PodIP,
			},
		},
	}
	cond := utils2.GetPodCondition(&pod.Status, corev1.PodReady)
	if cond != nil {
		sbx.Status.Conditions = append(sbx.Status.Conditions, metav1.Condition{
			Type:   string(v1alpha1.SandboxConditionReady),
			Status: metav1.ConditionStatus(cond.Status),
		})
	}
	if strings.HasPrefix(pod.Name, "paused") {
		sbx.Spec.Paused = true
	}
	return sbx
}

func TestSandbox_GetTemplate(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{
			name: "returns sandbox pool label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha1.LabelSandboxPool: "test-template",
					},
				},
			},
			want: "test-template",
		},
		{
			name: "empty template",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha1.LabelSandboxPool: "",
					},
				},
			},
			want: "",
		},
		{
			name: "no template label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{},
				},
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := AsSandboxForTest(ConvertPodToSandboxCR(tt.pod), nil, nil)
			if got := s.GetTemplate(); got != tt.want {
				t.Errorf("GetTemplate() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSandbox_GetResource(t *testing.T) {
	cpuQuantity1, _ := resource.ParseQuantity("1000m")
	cpuQuantity2, _ := resource.ParseQuantity("500m")
	memoryQuantity1, _ := resource.ParseQuantity("1024Mi")
	memoryQuantity2, _ := resource.ParseQuantity("512Mi")

	tests := []struct {
		name string
		pod  *corev1.Pod
		want infra.SandboxResource
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
			want: infra.SandboxResource{
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
			want: infra.SandboxResource{
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
			want: infra.SandboxResource{
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
			want: infra.SandboxResource{
				CPUMilli: 0,
				MemoryMB: 0,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := AsSandboxForTest(ConvertPodToSandboxCR(tt.pod), nil, nil)
			got := s.GetResource()
			if got.CPUMilli != tt.want.CPUMilli {
				t.Errorf("GetResource().CPUMilli = %v, want %v", got.CPUMilli, tt.want.CPUMilli)
			}
			if got.MemoryMB != tt.want.MemoryMB {
				t.Errorf("GetResource().MemoryMB = %v, want %v", got.MemoryMB, tt.want.MemoryMB)
			}
		})
	}
}

func TestSandbox_InplaceRefresh(t *testing.T) {
	initialSandbox := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Labels: map[string]string{
				"initial": "value",
			},
		},
	}

	cache, client := NewTestCache(t)
	_, err := client.ApiV1alpha1().Sandboxes("default").Create(context.Background(), initialSandbox, metav1.CreateOptions{})
	assert.NoError(t, err)
	time.Sleep(10 * time.Millisecond)

	updatedSandbox := initialSandbox.DeepCopy()
	updatedSandbox.Labels["updated"] = "new-value"
	_, err = client.ApiV1alpha1().Sandboxes("default").Update(context.Background(), updatedSandbox, metav1.UpdateOptions{})
	assert.NoError(t, err)
	time.Sleep(10 * time.Millisecond)

	s := AsSandboxForTest(initialSandbox, client, cache)

	assert.Equal(t, "value", s.Sandbox.Labels["initial"])
	assert.Empty(t, s.Sandbox.Labels["updated"])

	err = s.InplaceRefresh(t.Context(), false)
	assert.NoError(t, err)

	assert.Equal(t, "value", s.Sandbox.Labels["initial"])
	assert.Equal(t, "new-value", s.Sandbox.Labels["updated"])

	err = s.InplaceRefresh(t.Context(), true)
	assert.NoError(t, err)
	assert.Equal(t, "value", s.Sandbox.Labels["initial"])
	assert.Equal(t, "new-value", s.Sandbox.Labels["updated"])
}

//goland:noinspection GoDeprecation
func TestSandbox_Kill(t *testing.T) {
	tests := []struct {
		name              string
		hasDeletionTime   bool
		expectDeleteError bool
	}{
		{
			name:              "normal deletion",
			hasDeletionTime:   false,
			expectDeleteError: false,
		},
		{
			name:              "already marked for deletion",
			hasDeletionTime:   true,
			expectDeleteError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandbox := &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
			}

			if tt.hasDeletionTime {
				now := metav1.Now()
				sandbox.DeletionTimestamp = &now
			}

			client := fake.NewSimpleClientset()
			_, err := client.ApiV1alpha1().Sandboxes("default").Create(context.Background(), sandbox, metav1.CreateOptions{})
			assert.NoError(t, err)

			s := AsSandboxForTest(sandbox, client, nil)

			_, err = client.ApiV1alpha1().Sandboxes("default").Get(context.Background(), "test-sandbox", metav1.GetOptions{})
			assert.NoError(t, err)

			err = s.Kill(context.Background())
			if tt.expectDeleteError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			if !tt.hasDeletionTime {
				_, err = client.ApiV1alpha1().Sandboxes("default").Get(context.Background(), "test-sandbox", metav1.GetOptions{})
				assert.Error(t, err)
				assert.True(t, strings.Contains(err.Error(), "not found"))
			}
		})
	}
}

func TestSandbox_GetTimeout(t *testing.T) {
	now := metav1.Now()
	future := metav1.NewTime(now.Add(time.Hour))

	tests := []struct {
		name     string
		sandbox  *v1alpha1.Sandbox
		expected infra.TimeoutOptions
	}{
		{
			name: "with timeout set",
			sandbox: &v1alpha1.Sandbox{
				Spec: v1alpha1.SandboxSpec{
					ShutdownTime: &future,
					PauseTime:    &now,
				},
			},
			expected: infra.TimeoutOptions{
				ShutdownTime: future.Time,
				PauseTime:    now.Time,
			},
		},
		{
			name: "without shutdown time",
			sandbox: &v1alpha1.Sandbox{
				Spec: v1alpha1.SandboxSpec{
					ShutdownTime: nil,
				},
			},
			expected: infra.TimeoutOptions{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Sandbox{
				Sandbox: tt.sandbox,
			}
			result := s.GetTimeout()
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestSandbox_SaveTimeout(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name        string
		initialTime *metav1.Time
		opts        infra.TimeoutOptions
	}{
		{
			name:        "set timeout on sandbox without existing timeout",
			initialTime: nil,
			opts: infra.TimeoutOptions{
				ShutdownTime: now.Add(30 * time.Minute),
				PauseTime:    now.Add(15 * time.Minute),
			},
		},
		{
			name:        "update timeout on sandbox with existing timeout",
			initialTime: &metav1.Time{Time: now.Add(1 * time.Hour)},
			opts: infra.TimeoutOptions{
				ShutdownTime: now.Add(30 * time.Minute),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandbox := &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSpec{
					ShutdownTime: tt.initialTime,
				},
			}

			cache, client := NewTestCache(t)
			_, err := client.ApiV1alpha1().Sandboxes("default").Create(context.Background(), sandbox, metav1.CreateOptions{})
			assert.NoError(t, err)
			time.Sleep(20 * time.Millisecond)

			s := AsSandboxForTest(sandbox, client, cache)

			err = s.SaveTimeout(t.Context(), tt.opts)
			assert.NoError(t, err)

			updatedSandbox, err := client.ApiV1alpha1().Sandboxes("default").Get(context.Background(), "test-sandbox", metav1.GetOptions{})
			assert.NoError(t, err)
			if !tt.opts.PauseTime.IsZero() {
				assert.NotNil(t, updatedSandbox.Spec.PauseTime)
				assert.WithinDuration(t, tt.opts.PauseTime, updatedSandbox.Spec.PauseTime.Time, time.Second)
			}
			if !tt.opts.ShutdownTime.IsZero() {
				assert.NotNil(t, updatedSandbox.Spec.ShutdownTime)
				assert.WithinDuration(t, tt.opts.ShutdownTime, updatedSandbox.Spec.ShutdownTime.Time, time.Second)
			}
		})
	}
}

func TestSandbox_SetTimeout(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name        string
		initialTime infra.TimeoutOptions
		opts        infra.TimeoutOptions
	}{
		{
			name:        "set timeout on sandbox without existing timeout",
			initialTime: infra.TimeoutOptions{},
			opts: infra.TimeoutOptions{
				ShutdownTime: now,
				PauseTime:    now,
			},
		},
		{
			name:        "update timeout on sandbox with existing timeout",
			initialTime: infra.TimeoutOptions{},
			opts: infra.TimeoutOptions{
				ShutdownTime: now.Add(time.Hour),
				PauseTime:    now.Add(time.Hour),
			},
		},
		{
			name: "clear existing timeout",
			initialTime: infra.TimeoutOptions{
				ShutdownTime: now,
				PauseTime:    now,
			},
			opts: infra.TimeoutOptions{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandbox := &v1alpha1.Sandbox{
				Spec: v1alpha1.SandboxSpec{
					ShutdownTime: ptr.To(metav1.NewTime(tt.initialTime.ShutdownTime)),
					PauseTime:    ptr.To(metav1.NewTime(tt.initialTime.PauseTime)),
				},
			}
			s := &Sandbox{
				Sandbox: sandbox,
			}

			s.SetTimeout(tt.opts)

			assert.WithinDuration(t, tt.opts.ShutdownTime, getTimeFromMetaTime(s.Sandbox.Spec.ShutdownTime), time.Millisecond)
			assert.WithinDuration(t, tt.opts.PauseTime, getTimeFromMetaTime(s.Sandbox.Spec.PauseTime), time.Millisecond)
		})
	}
}

func getTimeFromMetaTime(t *metav1.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.Time
}

func TestSandbox_GetClaimTime(t *testing.T) {
	now := time.Now()
	claimTimeString := now.Format(time.RFC3339)

	tests := []struct {
		name     string
		sandbox  *v1alpha1.Sandbox
		expected time.Time
	}{
		{
			name: "with claim time annotation",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						v1alpha1.AnnotationClaimTime: claimTimeString,
					},
				},
			},
			expected: now,
		},
		{
			name: "without claim time annotation",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
				},
			},
			expected: time.Time{},
		},
		{
			name: "with invalid claim time annotation",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						v1alpha1.AnnotationClaimTime: "invalid-time-format",
					},
				},
			},
			expected: time.Time{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Sandbox{
				Sandbox: tt.sandbox,
			}
			result, _ := s.GetClaimTime()
			if tt.name == "with claim time annotation" {
				assert.WithinDuration(t, tt.expected, result, time.Second)
			} else {
				assert.Equal(t, tt.expected, result)
			}
		})
	}
}

func TestSandbox_GetRoute(t *testing.T) {
	tests := []struct {
		name          string
		sandbox       *v1alpha1.Sandbox
		expectedRoute proxy.Route
	}{
		{
			name: "available sandbox with owner",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "available-sandbox",
					Namespace: "default",
					Annotations: map[string]string{
						v1alpha1.AnnotationOwner: "test-owner",
					},
					OwnerReferences: GetSbsOwnerReference(),
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{
							Type:   string(v1alpha1.SandboxConditionReady),
							Status: metav1.ConditionTrue,
						},
					},
					PodInfo: v1alpha1.PodInfo{
						PodIP: "10.0.0.1",
					},
				},
			},
			expectedRoute: proxy.Route{
				IP:    "10.0.0.1",
				ID:    "default--available-sandbox",
				Owner: "test-owner",
				State: v1alpha1.SandboxStateAvailable,
			},
		},
		{
			name: "running sandbox without owner",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "running-sandbox",
					Namespace: "default",
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{
							Type:   string(v1alpha1.SandboxConditionReady),
							Status: metav1.ConditionTrue,
						},
					},
					PodInfo: v1alpha1.PodInfo{
						PodIP: "10.0.0.2",
					},
				},
			},
			expectedRoute: proxy.Route{
				IP:    "10.0.0.2",
				ID:    "default--running-sandbox",
				Owner: "",
				State: v1alpha1.SandboxStateRunning,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Sandbox{
				Sandbox: tt.sandbox,
			}

			route := s.GetRoute()
			assert.Equal(t, tt.expectedRoute, route)
		})
	}
}

func TestSandbox_CSIMount(t *testing.T) {
	tests := []struct {
		name         string
		result       RunCommandResult
		processError *string
		driver       string
		req          *csi.NodePublishVolumeRequest
		expectError  string
	}{
		{
			name: "successful csi mount",
			result: RunCommandResult{
				ExitCode: 0,
				Exited:   true,
			},
			driver: "csi-driver",
			req: &csi.NodePublishVolumeRequest{
				VolumeId: "volume-id",
			},
		},
		{
			name: "exits non-zero",
			result: RunCommandResult{
				ExitCode: 1,
				Exited:   true,
			},
			driver: "csi-driver",
			req: &csi.NodePublishVolumeRequest{
				VolumeId: "volume-id",
			},
			expectError: "command failed: [1]",
		},
		{
			name: "with process error",
			result: RunCommandResult{
				ExitCode: 0,
				Exited:   true,
			},
			req: &csi.NodePublishVolumeRequest{
				VolumeId: "volume-id",
			},
			processError: ptr.To("some error"),
			expectError:  "some error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := NewTestEnvdServer(tt.result, true, tt.processError)
			defer server.Close()

			cache, client := NewTestCache(t)
			sbx := &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-sandbox",
					Annotations: map[string]string{
						v1alpha1.AnnotationEnvdURL:         server.URL,
						v1alpha1.AnnotationEnvdAccessToken: AccessToken,
					},
				},
			}
			sandbox := AsSandboxForTest(sbx, client, cache)
			request, err := utils2.EncodeBase64Proto(tt.req)
			assert.NoError(t, err)
			err = sandbox.CSIMount(t.Context(), tt.driver, request)
			if tt.expectError != "" {
				assert.Error(t, err)
				assert.ErrorContains(t, err, tt.expectError)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
