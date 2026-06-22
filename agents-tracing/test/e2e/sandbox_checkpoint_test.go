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

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

// detectCheckpointGateEnabled probes the cluster to determine whether the
// SandboxPauseCheckpoint feature gate is enabled on the controller. It creates
// a temporary sandbox, pauses it, and observes whether a Checkpoint CR appears.
func detectCheckpointGateEnabled(ctx context.Context) bool {
	ns := createNamespace(ctx)
	sandbox := newCheckpointSandbox(ns)
	nn := types.NamespacedName{Name: sandbox.Name, Namespace: ns}

	Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
	defer func() { _ = k8sClient.Delete(ctx, sandbox) }()

	waitForSandboxRunning(ctx, nn)

	sandbox = getSandbox(ctx, nn)
	pauseSandbox(ctx, sandbox)

	var gateEnabled bool
	Eventually(func() bool {
		cpList := listCheckpoints(ctx, ns, sandbox.Name)
		if len(cpList) > 0 {
			gateEnabled = true
			return true
		}
		s := &agentsv1alpha1.Sandbox{}
		_ = k8sClient.Get(ctx, nn, s)
		if s.Status.Phase == agentsv1alpha1.SandboxPaused {
			gateEnabled = false
			return true
		}
		return false
	}, 60*time.Second, 500*time.Millisecond).Should(BeTrue(), "timed out detecting checkpoint gate state")

	return gateEnabled
}

func newCheckpointSandbox(namespace string) *agentsv1alpha1.Sandbox {
	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("cp-sandbox-%d", time.Now().UnixNano()),
			Namespace: namespace,
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "main",
								Image: "nginx:stable-alpine3.20",
								Ports: []corev1.ContainerPort{
									{Name: "http", ContainerPort: 80},
								},
							},
						},
						RestartPolicy: corev1.RestartPolicyNever,
					},
				},
			},
		},
	}
}

func waitForSandboxPhase(ctx context.Context, nn types.NamespacedName, phase agentsv1alpha1.SandboxPhase, timeout time.Duration) *agentsv1alpha1.Sandbox {
	sandbox := &agentsv1alpha1.Sandbox{}
	Eventually(func() agentsv1alpha1.SandboxPhase {
		_ = k8sClient.Get(ctx, nn, sandbox)
		return sandbox.Status.Phase
	}, timeout, 500*time.Millisecond).Should(Equal(phase))
	return sandbox
}

func waitForSandboxRunning(ctx context.Context, nn types.NamespacedName) *agentsv1alpha1.Sandbox {
	return waitForSandboxPhase(ctx, nn, agentsv1alpha1.SandboxRunning, 90*time.Second)
}

func pauseSandbox(ctx context.Context, sandbox *agentsv1alpha1.Sandbox) {
	sandbox.Spec.Paused = true
	ExpectWithOffset(1, updateSandboxSpec(ctx, sandbox)).To(Succeed())
}

func resumeSandbox(ctx context.Context, sandbox *agentsv1alpha1.Sandbox) {
	sandbox.Spec.Paused = false
	ExpectWithOffset(1, updateSandboxSpec(ctx, sandbox)).To(Succeed())
}

func listCheckpoints(ctx context.Context, namespace, sandboxName string) []agentsv1alpha1.Checkpoint {
	cpList := &agentsv1alpha1.CheckpointList{}
	err := k8sClient.List(ctx, cpList,
		client.InNamespace(namespace),
		client.MatchingLabels{
			agentsv1alpha1.CheckpointLabelSandboxName: sandboxName,
			agentsv1alpha1.CheckpointLabelType:        agentsv1alpha1.CheckpointTypePodInfo,
		},
	)
	if err != nil {
		return nil
	}
	return cpList.Items
}

func simulateCheckpointSucceeded(ctx context.Context, cp *agentsv1alpha1.Checkpoint, delta []byte) {
	cp.Status.Phase = agentsv1alpha1.CheckpointSucceeded
	if len(delta) > 0 {
		cp.Status.PodTemplateDelta = runtime.RawExtension{Raw: delta}
	}
	ExpectWithOffset(1, k8sClient.Status().Update(ctx, cp)).To(Succeed())
}

func simulateCheckpointFailed(ctx context.Context, cp *agentsv1alpha1.Checkpoint, msg string) {
	cp.Status.Phase = agentsv1alpha1.CheckpointFailed
	cp.Status.Message = msg
	ExpectWithOffset(1, k8sClient.Status().Update(ctx, cp)).To(Succeed())
}

func getPausedCondition(sandbox *agentsv1alpha1.Sandbox) *metav1.Condition {
	return utils.GetSandboxCondition(&sandbox.Status, string(agentsv1alpha1.SandboxConditionPaused))
}

func getSandbox(ctx context.Context, nn types.NamespacedName) *agentsv1alpha1.Sandbox {
	sandbox := &agentsv1alpha1.Sandbox{}
	ExpectWithOffset(1, k8sClient.Get(ctx, nn, sandbox)).To(Succeed())
	return sandbox
}

var _ = Describe("Sandbox Checkpoint", Ordered, func() {
	var (
		ctx         = context.Background()
		namespace   string
		sandbox     *agentsv1alpha1.Sandbox
		nn          types.NamespacedName
		gateEnabled bool
	)

	BeforeAll(func() {
		gateEnabled = detectCheckpointGateEnabled(ctx)
	})

	BeforeEach(func() {
		namespace = createNamespace(ctx)
	})

	AfterEach(func() {
		if sandbox != nil {
			_ = k8sClient.Delete(ctx, sandbox)
			sandbox = nil
		}
	})

	Context("without SandboxPauseCheckpoint gate", func() {
		BeforeEach(func() {
			if gateEnabled {
				Skip("SandboxPauseCheckpoint gate is enabled on the cluster")
			}
		})

		It("should pause and resume without creating checkpoint CR", func() {
			sandbox = newCheckpointSandbox(namespace)
			nn = types.NamespacedName{Name: sandbox.Name, Namespace: namespace}

			By("Creating sandbox and waiting for Running")
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			waitForSandboxRunning(ctx, nn)

			By("Pausing the sandbox")
			sandbox = getSandbox(ctx, nn)
			pauseSandbox(ctx, sandbox)

			By("Waiting for sandbox to reach Paused phase")
			waitForSandboxPhase(ctx, nn, agentsv1alpha1.SandboxPaused, 30*time.Second)

			By("Verifying no Checkpoint CR was created")
			cpList := listCheckpoints(ctx, namespace, sandbox.Name)
			Expect(cpList).To(BeEmpty())

			By("Verifying pod is deleted")
			Eventually(func() bool {
				pod := &corev1.Pod{}
				err := k8sClient.Get(ctx, nn, pod)
				return err != nil
			}, 30*time.Second, 500*time.Millisecond).Should(BeTrue())

			By("Resuming the sandbox")
			sandbox = getSandbox(ctx, nn)
			resumeSandbox(ctx, sandbox)

			By("Waiting for sandbox to reach Running phase")
			waitForSandboxRunning(ctx, nn)

			By("Verifying pod exists after resume")
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, nn, pod)).To(Succeed())
			Expect(pod.Spec.Containers[0].Image).To(Equal("nginx:stable-alpine3.20"))
		})
	})

	Context("with SandboxPauseCheckpoint gate", func() {
		BeforeEach(func() {
			if !gateEnabled {
				Skip("SandboxPauseCheckpoint gate is not enabled on the cluster")
			}
		})

		It("should create checkpoint CR on pause and wait for completion", func() {
			sandbox = newCheckpointSandbox(namespace)
			nn = types.NamespacedName{Name: sandbox.Name, Namespace: namespace}

			By("Creating sandbox and waiting for Running")
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			waitForSandboxRunning(ctx, nn)

			By("Pausing the sandbox")
			sandbox = getSandbox(ctx, nn)
			pauseSandbox(ctx, sandbox)

			By("Verifying checkpoint CR is created")
			var cpList []agentsv1alpha1.Checkpoint
			Eventually(func() int {
				cpList = listCheckpoints(ctx, namespace, sandbox.Name)
				return len(cpList)
			}, 30*time.Second, 500*time.Millisecond).Should(Equal(1))

			By("Verifying checkpoint CR labels and ownerRef")
			cp := cpList[0]
			Expect(cp.Labels[agentsv1alpha1.CheckpointLabelSandboxName]).To(Equal(sandbox.Name))
			Expect(cp.Labels[agentsv1alpha1.CheckpointLabelType]).To(Equal(agentsv1alpha1.CheckpointTypePodInfo))
			Expect(cp.OwnerReferences).To(HaveLen(1))
			Expect(cp.OwnerReferences[0].Name).To(Equal(sandbox.Name))
			Expect(cp.OwnerReferences[0].Kind).To(Equal("Sandbox"))

			By("Verifying sandbox condition reason is CheckpointCreating")
			sandbox = getSandbox(ctx, nn)
			cond := getPausedCondition(sandbox)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Reason).To(Equal(agentsv1alpha1.SandboxPausedReasonCheckpointCreating))

			By("Verifying pod is NOT deleted yet (waiting for checkpoint)")
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, nn, pod)).To(Succeed())
			Expect(pod.DeletionTimestamp.IsZero()).To(BeTrue())
		})

		It("should complete pause when checkpoint succeeds", func() {
			sandbox = newCheckpointSandbox(namespace)
			nn = types.NamespacedName{Name: sandbox.Name, Namespace: namespace}

			By("Creating sandbox and waiting for Running")
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			waitForSandboxRunning(ctx, nn)

			By("Pausing the sandbox")
			sandbox = getSandbox(ctx, nn)
			pauseSandbox(ctx, sandbox)

			By("Waiting for checkpoint CR")
			var cpList []agentsv1alpha1.Checkpoint
			Eventually(func() int {
				cpList = listCheckpoints(ctx, namespace, sandbox.Name)
				return len(cpList)
			}, 30*time.Second, 500*time.Millisecond).Should(Equal(1))

			By("Simulating checkpoint succeeded")
			simulateCheckpointSucceeded(ctx, &cpList[0], nil)

			By("Verifying sandbox transitions to Paused phase")
			waitForSandboxPhase(ctx, nn, agentsv1alpha1.SandboxPaused, 30*time.Second)

			By("Verifying pod is deleted")
			Eventually(func() bool {
				pod := &corev1.Pod{}
				err := k8sClient.Get(ctx, nn, pod)
				return err != nil
			}, 30*time.Second, 500*time.Millisecond).Should(BeTrue())
		})

		It("should apply pod template delta on resume and cleanup checkpoint", func() {
			sandbox = newCheckpointSandbox(namespace)
			nn = types.NamespacedName{Name: sandbox.Name, Namespace: namespace}

			By("Creating sandbox and waiting for Running")
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			waitForSandboxRunning(ctx, nn)

			By("Pausing the sandbox")
			sandbox = getSandbox(ctx, nn)
			pauseSandbox(ctx, sandbox)

			By("Waiting for checkpoint CR")
			var cpList []agentsv1alpha1.Checkpoint
			Eventually(func() int {
				cpList = listCheckpoints(ctx, namespace, sandbox.Name)
				return len(cpList)
			}, 30*time.Second, 500*time.Millisecond).Should(Equal(1))

			By("Simulating checkpoint succeeded with pod template delta")
			delta, err := json.Marshal(map[string]any{
				"metadata": map[string]any{
					"labels": map[string]string{
						"checkpoint-restored": "true",
					},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			simulateCheckpointSucceeded(ctx, &cpList[0], delta)

			By("Waiting for sandbox to reach Paused phase")
			waitForSandboxPhase(ctx, nn, agentsv1alpha1.SandboxPaused, 30*time.Second)

			By("Resuming the sandbox")
			sandbox = getSandbox(ctx, nn)
			resumeSandbox(ctx, sandbox)

			By("Waiting for sandbox to reach Running phase")
			sandbox = waitForSandboxRunning(ctx, nn)

			By("Verifying the recreated pod has the checkpoint delta applied")
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, nn, pod)).To(Succeed())
			Expect(pod.Labels["checkpoint-restored"]).To(Equal("true"))

			By("Verifying checkpoint CR is cleaned up after resume")
			Eventually(func() int {
				return len(listCheckpoints(ctx, namespace, sandbox.Name))
			}, 30*time.Second, 500*time.Millisecond).Should(Equal(0))
		})

		It("should reject pause when container image changed", func() {
			sandbox = newCheckpointSandbox(namespace)
			nn = types.NamespacedName{Name: sandbox.Name, Namespace: namespace}

			By("Creating sandbox and waiting for Running")
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			waitForSandboxRunning(ctx, nn)

			By("Patching pod container image to simulate drift")
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, nn, pod)).To(Succeed())
			patch := client.MergeFrom(pod.DeepCopy())
			pod.Spec.Containers[0].Image = "nginx:stable-alpine3.23"
			Expect(k8sClient.Patch(ctx, pod, patch)).To(Succeed())

			By("Waiting for pod to be ready with new image")
			Eventually(func() bool {
				p := &corev1.Pod{}
				if err := k8sClient.Get(ctx, nn, p); err != nil {
					return false
				}
				for _, cs := range p.Status.ContainerStatuses {
					if cs.Name == "main" && cs.Ready {
						return true
					}
				}
				return false
			}, 90*time.Second, time.Second).Should(BeTrue())

			By("Pausing the sandbox")
			sandbox = getSandbox(ctx, nn)
			pauseSandbox(ctx, sandbox)

			By("Verifying sandbox condition reason is ImageChanged")
			Eventually(func() string {
				sandbox = getSandbox(ctx, nn)
				cond := getPausedCondition(sandbox)
				if cond == nil {
					return ""
				}
				return cond.Reason
			}, 30*time.Second, 500*time.Millisecond).Should(Equal(agentsv1alpha1.SandboxPausedReasonImageChanged))

			By("Verifying pod is NOT deleted")
			pod = &corev1.Pod{}
			Expect(k8sClient.Get(ctx, nn, pod)).To(Succeed())
			Expect(pod.DeletionTimestamp.IsZero()).To(BeTrue())
		})

		It("should report checkpoint failure in condition", func() {
			sandbox = newCheckpointSandbox(namespace)
			nn = types.NamespacedName{Name: sandbox.Name, Namespace: namespace}

			By("Creating sandbox and waiting for Running")
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			waitForSandboxRunning(ctx, nn)

			By("Pausing the sandbox")
			sandbox = getSandbox(ctx, nn)
			pauseSandbox(ctx, sandbox)

			By("Waiting for checkpoint CR")
			var cpList []agentsv1alpha1.Checkpoint
			Eventually(func() int {
				cpList = listCheckpoints(ctx, namespace, sandbox.Name)
				return len(cpList)
			}, 30*time.Second, 500*time.Millisecond).Should(Equal(1))

			By("Simulating checkpoint failed")
			simulateCheckpointFailed(ctx, &cpList[0], "disk quota exceeded")

			By("Verifying sandbox condition reason is CheckpointFailed")
			Eventually(func() string {
				sandbox = getSandbox(ctx, nn)
				cond := getPausedCondition(sandbox)
				if cond == nil {
					return ""
				}
				return cond.Reason
			}, 30*time.Second, 500*time.Millisecond).Should(Equal(agentsv1alpha1.SandboxPausedReasonCheckpointFailed))

			By("Verifying pod is NOT deleted")
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, nn, pod)).To(Succeed())
			Expect(pod.DeletionTimestamp.IsZero()).To(BeTrue())
		})

		It("should handle multiple pause/resume cycles with checkpoint cleanup", func() {
			sandbox = newCheckpointSandbox(namespace)
			nn = types.NamespacedName{Name: sandbox.Name, Namespace: namespace}

			By("Creating sandbox and waiting for Running")
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			waitForSandboxRunning(ctx, nn)

			By("First cycle: pausing")
			sandbox = getSandbox(ctx, nn)
			pauseSandbox(ctx, sandbox)

			var cpList []agentsv1alpha1.Checkpoint
			Eventually(func() int {
				cpList = listCheckpoints(ctx, namespace, sandbox.Name)
				return len(cpList)
			}, 30*time.Second, 500*time.Millisecond).Should(Equal(1))
			firstCPName := cpList[0].Name

			simulateCheckpointSucceeded(ctx, &cpList[0], nil)
			waitForSandboxPhase(ctx, nn, agentsv1alpha1.SandboxPaused, 30*time.Second)

			By("First cycle: resuming")
			sandbox = getSandbox(ctx, nn)
			resumeSandbox(ctx, sandbox)
			waitForSandboxRunning(ctx, nn)

			By("Verifying first checkpoint is cleaned up")
			Eventually(func() int {
				return len(listCheckpoints(ctx, namespace, sandbox.Name))
			}, 30*time.Second, 500*time.Millisecond).Should(Equal(0))

			By("Second cycle: pausing")
			sandbox = getSandbox(ctx, nn)
			pauseSandbox(ctx, sandbox)

			Eventually(func() int {
				cpList = listCheckpoints(ctx, namespace, sandbox.Name)
				return len(cpList)
			}, 30*time.Second, 500*time.Millisecond).Should(Equal(1))

			By("Verifying second checkpoint is different from first")
			Expect(cpList[0].Name).NotTo(Equal(firstCPName))

			By("Completing second pause")
			simulateCheckpointSucceeded(ctx, &cpList[0], nil)
			waitForSandboxPhase(ctx, nn, agentsv1alpha1.SandboxPaused, 30*time.Second)

			By("Second cycle: resuming")
			sandbox = getSandbox(ctx, nn)
			resumeSandbox(ctx, sandbox)
			waitForSandboxRunning(ctx, nn)
		})

		It("should reject pause when sidecar init container image changed", func() {
			alwaysRestart := corev1.ContainerRestartPolicyAlways
			sandbox = &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("cp-sidecar-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								InitContainers: []corev1.Container{
									{
										Name:          "sidecar",
										Image:         "nginx:stable-alpine3.20",
										RestartPolicy: &alwaysRestart,
										Command:       []string{"nginx", "-g", "daemon off;"},
									},
								},
								Containers: []corev1.Container{
									{
										Name:  "main",
										Image: "nginx:stable-alpine3.20",
										Ports: []corev1.ContainerPort{
											{Name: "http", ContainerPort: 80},
										},
									},
								},
								RestartPolicy: corev1.RestartPolicyNever,
							},
						},
					},
				},
			}
			nn = types.NamespacedName{Name: sandbox.Name, Namespace: namespace}

			By("Creating sandbox with sidecar init container and waiting for Running")
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			waitForSandboxRunning(ctx, nn)

			By("Patching sidecar init container image to simulate drift")
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, nn, pod)).To(Succeed())
			patch := client.MergeFrom(pod.DeepCopy())
			for i := range pod.Spec.InitContainers {
				if pod.Spec.InitContainers[i].Name == "sidecar" {
					pod.Spec.InitContainers[i].Image = "nginx:stable-alpine3.23"
				}
			}
			Expect(k8sClient.Patch(ctx, pod, patch)).To(Succeed())

			By("Waiting for sidecar init container to be ready with new image")
			Eventually(func() bool {
				p := &corev1.Pod{}
				if err := k8sClient.Get(ctx, nn, p); err != nil {
					return false
				}
				for _, cs := range p.Status.InitContainerStatuses {
					if cs.Name == "sidecar" && cs.Ready {
						return true
					}
				}
				return false
			}, 90*time.Second, time.Second).Should(BeTrue())

			By("Pausing the sandbox")
			sandbox = getSandbox(ctx, nn)
			pauseSandbox(ctx, sandbox)

			By("Verifying sandbox condition reason is ImageChanged")
			Eventually(func() string {
				sandbox = getSandbox(ctx, nn)
				cond := getPausedCondition(sandbox)
				if cond == nil {
					return ""
				}
				return cond.Reason
			}, 30*time.Second, 500*time.Millisecond).Should(Equal(agentsv1alpha1.SandboxPausedReasonImageChanged))

			By("Verifying pod is NOT deleted")
			pod = &corev1.Pod{}
			Expect(k8sClient.Get(ctx, nn, pod)).To(Succeed())
			Expect(pod.DeletionTimestamp.IsZero()).To(BeTrue())
		})

		It("should preserve externally-injected labels and annotations after pause/resume", func() {
			sandbox = newCheckpointSandbox(namespace)
			nn = types.NamespacedName{Name: sandbox.Name, Namespace: namespace}

			By("Creating sandbox and waiting for Running")
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			waitForSandboxRunning(ctx, nn)

			By("Patching pod with external labels and annotations")
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, nn, pod)).To(Succeed())
			patch := client.MergeFrom(pod.DeepCopy())
			if pod.Labels == nil {
				pod.Labels = map[string]string{}
			}
			pod.Labels["topology.kubernetes.io/zone"] = "cn-hangzhou-b"
			if pod.Annotations == nil {
				pod.Annotations = map[string]string{}
			}
			pod.Annotations["scheduling.k8s.io/group-name"] = "pool-a"
			Expect(k8sClient.Patch(ctx, pod, patch)).To(Succeed())

			By("Pausing the sandbox")
			sandbox = getSandbox(ctx, nn)
			pauseSandbox(ctx, sandbox)

			By("Waiting for checkpoint CR")
			var cpList []agentsv1alpha1.Checkpoint
			Eventually(func() int {
				cpList = listCheckpoints(ctx, namespace, sandbox.Name)
				return len(cpList)
			}, 30*time.Second, 500*time.Millisecond).Should(Equal(1))

			By("Simulating checkpoint succeeded with metadata delta")
			delta, err := json.Marshal(map[string]any{
				"metadata": map[string]any{
					"labels": map[string]string{
						"topology.kubernetes.io/zone": "cn-hangzhou-b",
					},
					"annotations": map[string]string{
						"scheduling.k8s.io/group-name": "pool-a",
					},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			simulateCheckpointSucceeded(ctx, &cpList[0], delta)

			By("Waiting for sandbox to reach Paused phase")
			waitForSandboxPhase(ctx, nn, agentsv1alpha1.SandboxPaused, 30*time.Second)

			By("Resuming the sandbox")
			sandbox = getSandbox(ctx, nn)
			resumeSandbox(ctx, sandbox)

			By("Waiting for sandbox to reach Running phase")
			waitForSandboxRunning(ctx, nn)

			By("Verifying the recreated pod has the externally-injected labels and annotations")
			pod = &corev1.Pod{}
			Expect(k8sClient.Get(ctx, nn, pod)).To(Succeed())
			Expect(pod.Labels["topology.kubernetes.io/zone"]).To(Equal("cn-hangzhou-b"))
			Expect(pod.Annotations["scheduling.k8s.io/group-name"]).To(Equal("pool-a"))
		})

		It("should preserve VPA-modified container resources after pause/resume", func() {
			sandbox = &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("cp-vpa-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "main",
										Image: "nginx:stable-alpine3.20",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												corev1.ResourceCPU:    resource.MustParse("100m"),
												corev1.ResourceMemory: resource.MustParse("128Mi"),
											},
											Limits: corev1.ResourceList{
												corev1.ResourceCPU:    resource.MustParse("100m"),
												corev1.ResourceMemory: resource.MustParse("128Mi"),
											},
										},
										Ports: []corev1.ContainerPort{
											{Name: "http", ContainerPort: 80},
										},
									},
								},
								RestartPolicy: corev1.RestartPolicyNever,
							},
						},
					},
				},
			}
			nn = types.NamespacedName{Name: sandbox.Name, Namespace: namespace}

			By("Creating sandbox with resources and waiting for Running")
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			waitForSandboxRunning(ctx, nn)

			By("Pausing the sandbox")
			sandbox = getSandbox(ctx, nn)
			pauseSandbox(ctx, sandbox)

			By("Waiting for checkpoint CR")
			var cpList []agentsv1alpha1.Checkpoint
			Eventually(func() int {
				cpList = listCheckpoints(ctx, namespace, sandbox.Name)
				return len(cpList)
			}, 30*time.Second, 500*time.Millisecond).Should(Equal(1))

			By("Simulating checkpoint succeeded with VPA-modified resources delta")
			delta, err := json.Marshal(map[string]any{
				"spec": map[string]any{
					"containers": []map[string]any{
						{
							"name": "main",
							"resources": map[string]any{
								"requests": map[string]string{
									"cpu":    "200m",
									"memory": "256Mi",
								},
								"limits": map[string]string{
									"cpu":    "200m",
									"memory": "256Mi",
								},
							},
						},
					},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			simulateCheckpointSucceeded(ctx, &cpList[0], delta)

			By("Waiting for sandbox to reach Paused phase")
			waitForSandboxPhase(ctx, nn, agentsv1alpha1.SandboxPaused, 30*time.Second)

			By("Resuming the sandbox")
			sandbox = getSandbox(ctx, nn)
			resumeSandbox(ctx, sandbox)

			By("Waiting for sandbox to reach Running phase")
			waitForSandboxRunning(ctx, nn)

			By("Verifying the recreated pod has VPA-modified resources")
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, nn, pod)).To(Succeed())
			mainContainer := pod.Spec.Containers[0]
			Expect(mainContainer.Name).To(Equal("main"))
			Expect(mainContainer.Resources.Requests.Cpu().String()).To(Equal("200m"))
			Expect(mainContainer.Resources.Requests.Memory().String()).To(Equal("256Mi"))
			Expect(mainContainer.Resources.Limits.Cpu().String()).To(Equal("200m"))
			Expect(mainContainer.Resources.Limits.Memory().String()).To(Equal("256Mi"))
		})

		It("should preserve runtime-injected container config after pause/resume despite ConfigMap update", func() {
			const (
				runtimeName     = "e2e-sidecar"
				configMapNS     = "sandbox-system"
				configMapName   = "sandbox-injection-config"
				sidecarImageOld = "busybox:1.35"
				sidecarImageNew = "busybox:1.36"
			)

			By("Creating injection ConfigMap with runtime sidecar config")
			injectConfig, err := json.Marshal(map[string]any{
				"containers": []map[string]any{
					{
						"name":    runtimeName,
						"image":   sidecarImageOld,
						"command": []string{"sleep", "infinity"},
					},
				},
			})
			Expect(err).NotTo(HaveOccurred())

			configMap := &corev1.ConfigMap{}
			cmNN := types.NamespacedName{Name: configMapName, Namespace: configMapNS}
			cmErr := k8sClient.Get(ctx, cmNN, configMap)

			var originalCMData map[string]string
			if cmErr == nil {
				originalCMData = make(map[string]string, len(configMap.Data))
				for k, v := range configMap.Data {
					originalCMData[k] = v
				}
				configMap.Data[runtimeName] = string(injectConfig)
				Expect(k8sClient.Update(ctx, configMap)).To(Succeed())
			} else {
				configMap = &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      configMapName,
						Namespace: configMapNS,
					},
					Data: map[string]string{
						runtimeName: string(injectConfig),
					},
				}
				Expect(k8sClient.Create(ctx, configMap)).To(Succeed())
			}
			defer func() {
				if originalCMData != nil {
					cm := &corev1.ConfigMap{}
					if getErr := k8sClient.Get(ctx, cmNN, cm); getErr == nil {
						cm.Data = originalCMData
						_ = k8sClient.Update(ctx, cm)
					}
				} else {
					_ = k8sClient.Delete(ctx, configMap)
				}
			}()

			By("Creating sandbox with runtimes and waiting for Running")
			sandbox = &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("cp-runtime-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Runtimes: []agentsv1alpha1.RuntimeConfig{{Name: runtimeName}},
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "main",
										Image: "nginx:stable-alpine3.20",
										Ports: []corev1.ContainerPort{
											{Name: "http", ContainerPort: 80},
										},
									},
								},
								RestartPolicy: corev1.RestartPolicyNever,
							},
						},
					},
				},
			}
			nn = types.NamespacedName{Name: sandbox.Name, Namespace: namespace}
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			waitForSandboxRunning(ctx, nn)

			By("Verifying the running pod has the runtime-injected sidecar")
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, nn, pod)).To(Succeed())
			sidecarFound := false
			for _, c := range pod.Spec.Containers {
				if c.Name == runtimeName {
					sidecarFound = true
					Expect(c.Image).To(Equal(sidecarImageOld))
				}
			}
			Expect(sidecarFound).To(BeTrue(), "runtime sidecar should be injected into the pod")

			By("Pausing the sandbox")
			sandbox = getSandbox(ctx, nn)
			pauseSandbox(ctx, sandbox)

			By("Waiting for checkpoint CR")
			var cpList []agentsv1alpha1.Checkpoint
			Eventually(func() int {
				cpList = listCheckpoints(ctx, namespace, sandbox.Name)
				return len(cpList)
			}, 30*time.Second, 500*time.Millisecond).Should(Equal(1))

			By("Simulating checkpoint succeeded with pause-time sidecar config in delta")
			delta, err := json.Marshal(map[string]any{
				"spec": map[string]any{
					"containers": []map[string]any{
						{
							"name":  "main",
							"image": "nginx:stable-alpine3.20",
						},
						{
							"name":    runtimeName,
							"image":   sidecarImageOld,
							"command": []string{"sleep", "infinity"},
						},
					},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			simulateCheckpointSucceeded(ctx, &cpList[0], delta)

			By("Waiting for sandbox to reach Paused phase")
			waitForSandboxPhase(ctx, nn, agentsv1alpha1.SandboxPaused, 30*time.Second)

			By("Updating ConfigMap to simulate runtime image change during pause")
			updatedConfig, err := json.Marshal(map[string]any{
				"containers": []map[string]any{
					{
						"name":    runtimeName,
						"image":   sidecarImageNew,
						"command": []string{"sleep", "infinity"},
					},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, cmNN, configMap)).To(Succeed())
			configMap.Data[runtimeName] = string(updatedConfig)
			Expect(k8sClient.Update(ctx, configMap)).To(Succeed())

			By("Resuming the sandbox")
			sandbox = getSandbox(ctx, nn)
			resumeSandbox(ctx, sandbox)

			By("Waiting for sandbox to reach Running phase")
			waitForSandboxRunning(ctx, nn)

			By("Verifying the recreated pod has the OLD sidecar image from checkpoint, not the updated ConfigMap")
			pod = &corev1.Pod{}
			Expect(k8sClient.Get(ctx, nn, pod)).To(Succeed())
			sidecarFound = false
			for _, c := range pod.Spec.Containers {
				if c.Name == runtimeName {
					sidecarFound = true
					Expect(c.Image).To(Equal(sidecarImageOld),
						"sidecar image should be preserved from checkpoint, not updated from ConfigMap")
				}
			}
			Expect(sidecarFound).To(BeTrue(), "runtime sidecar should be present after resume")

			By("Verifying checkpoint CR is cleaned up after resume")
			Eventually(func() int {
				return len(listCheckpoints(ctx, namespace, sandbox.Name))
			}, 30*time.Second, 500*time.Millisecond).Should(Equal(0))
		})
	})
})
