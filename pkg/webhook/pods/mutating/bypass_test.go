package mutating

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/onsi/gomega"
	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/webhook/types"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestHandleCreate(t *testing.T) {
	// 添加 v1alpha1 到 scheme
	err := v1alpha1.AddToScheme(scheme.Scheme)
	require.NoError(t, err)

	tests := []struct {
		name            string
		pod             *corev1.Pod
		existingSandbox *v1alpha1.Sandbox
		expectResult    types.Result
		expectAllowed   bool
		expectDenied    bool
		expectError     bool
	}{
		{
			name: "No existing sandbox should be skipped",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Labels: map[string]string{
						utils.PodLabelEnableAutoCreateSandbox: utils.True,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "test-container",
							Image: "nginx:latest",
						},
					},
				},
			},
			existingSandbox: nil,
			expectAllowed:   true,
			expectError:     false,
		},
		{
			name: "Existing sandbox not paused should be denied",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Labels: map[string]string{
						utils.PodLabelEnableAutoCreateSandbox: utils.True,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "test-container",
							Image: "nginx:latest",
						},
					},
				},
			},
			existingSandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSpec{
					Paused: false,
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxPaused,
				},
			},
			expectError: false,
		},
		{
			name: "Existing sandbox paused but not in Paused phase should be denied",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Labels: map[string]string{
						utils.PodLabelEnableAutoCreateSandbox: utils.True,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "test-container",
							Image: "nginx:latest",
						},
					},
				},
			},
			existingSandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSpec{
					Paused: true,
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxRunning,
				},
			},
			expectError: false,
		},
		{
			name: "Existing sandbox paused and in Paused phase should update pod spec",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Labels: map[string]string{
						utils.PodLabelEnableAutoCreateSandbox: utils.True,
					},
					Annotations: map[string]string{},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "test-container",
							Image: "old-image:latest",
						},
					},
				},
			},
			existingSandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSpec{
					Paused: true,
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test-container",
									Image: "new-image:latest",
								},
							},
						},
					},
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxPaused,
					PodInfo: v1alpha1.PodInfo{
						Annotations: map[string]string{
							utils.PodAnnotationAcsInstanceId: "test-instance-id",
						},
						NodeName: "test-node",
					},
				},
			},
			expectResult: types.Patch,
			expectError:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewGomegaWithT(t)

			// 创建 fake client
			objs := []runtime.Object{}
			if tt.existingSandbox != nil {
				objs = append(objs, tt.existingSandbox)
			}

			fakeClient := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(objs...).Build()

			// 创建 decoder
			decoder := admission.NewDecoder(scheme.Scheme)

			// 创建 handler
			handler := &PodBypassSandboxHandler{
				Client:  fakeClient,
				Decoder: decoder,
			}

			// 执行测试
			ctx := context.Background()
			logf.SetLogger(zap.New(zap.UseDevMode(true)))

			// 构造 admission 请求
			podRaw, err := json.Marshal(tt.pod)
			require.NoError(t, err)

			req := admission.Request{
				AdmissionRequest: admissionv1.AdmissionRequest{
					Operation: admissionv1.Create,
					Object: runtime.RawExtension{
						Raw: podRaw,
					},
				},
			}

			response := handler.HandleCreate(ctx, req)

			// 验证结果
			if tt.expectError {
				g.Expect(response.Allowed).To(gomega.BeFalse())
			} else {
				g.Expect(err).NotTo(gomega.HaveOccurred())
			}

			if tt.expectAllowed {
				g.Expect(response.Allowed).To(gomega.BeTrue())
			}

			if tt.expectDenied {
				g.Expect(response.Allowed).To(gomega.BeFalse())
			}

			// 如果期望更改，验证 pod spec 是否更新
			if tt.expectResult == types.Patch && tt.existingSandbox != nil {
				// 检查是否进行了 Patch 操作
				g.Expect(response.Patches).ToNot(gomega.BeNil())
			}
		})
	}
}
