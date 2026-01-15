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

package e2e

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
)

var _ = Describe("Sandbox", func() {
	var (
		sandbox      *agentsv1alpha1.Sandbox
		ctx          = context.Background()
		namespace    string
		updateImage  string
		initialImage string
	)

	BeforeEach(func() {
		namespace = createNamespace(ctx)
		updateImage = "nginx:stable-alpine3.23"
		initialImage = "nginx:stable-alpine3.20"
		// Create a basic Sandbox resource
		sandbox = &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("test-sandbox-%d", time.Now().UnixNano()),
				Namespace: namespace,
			},
			Spec: agentsv1alpha1.SandboxSpec{
				SandboxTemplate: agentsv1alpha1.SandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test-container",
									Image: initialImage,
									Ports: []corev1.ContainerPort{
										{
											Name:          "http",
											ContainerPort: 80,
										},
									},
								},
							},
							RestartPolicy: corev1.RestartPolicyNever,
						},
					},
				},
			},
		}
	})

	AfterEach(func() {
		// Clean up test resources
		_ = k8sClient.Delete(ctx, sandbox)
	})

	Context("creation and pending phase", func() {
		It("should create sandbox and transition to pending phase", func() {
			By("Creating a new Sandbox")
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())

			By("Verifying the sandbox is created")
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, sandbox)
			}, time.Minute*10, time.Millisecond*500).Should(Succeed())

			By("Verifying the sandbox phase transitions to Pending")
			Eventually(func() agentsv1alpha1.SandboxPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, sandbox)
				return sandbox.Status.Phase
			}, time.Second*30, time.Millisecond*500).Should(Equal(agentsv1alpha1.SandboxPending))

			By("Verifying the sandbox has latest revision")
			Expect(sandbox.Status.UpdateRevision).NotTo(BeEmpty())
		})
	})

	Context("running phase", func() {
		It("should transition from pending to running when pod is ready", func() {
			By("Creating a new Sandbox")
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())

			By("Waiting for sandbox to reach Running phase")
			Eventually(func() agentsv1alpha1.SandboxPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, sandbox)
				return sandbox.Status.Phase
			}, time.Second*60, time.Millisecond*500).Should(Equal(agentsv1alpha1.SandboxRunning))

			By("Verifying sandbox has pod information when running")
			klog.Infof("sandbox status(%s)", utils.DumpJson(sandbox.Status))
			Expect(sandbox.Status.PodInfo.PodIP).NotTo(BeEmpty())
			Expect(sandbox.Status.PodInfo.NodeName).NotTo(BeEmpty())
			Expect(sandbox.Status.SandboxIp).NotTo(BeEmpty())
			Expect(sandbox.Status.NodeName).NotTo(BeEmpty())

			By("Verifying sandbox ready condition is set")
			readyCondition := utils.GetSandboxCondition(&sandbox.Status, string(agentsv1alpha1.SandboxConditionReady))
			Expect(readyCondition).NotTo(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))
		})
	})

	Context("pause and resume lifecycle", func() {
		It("should pause and resume sandbox successfully", func() {
			By("Creating a new Sandbox")
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())

			By("Waiting for sandbox to reach Running phase")
			Eventually(func() agentsv1alpha1.SandboxPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, sandbox)
				return sandbox.Status.Phase
			}, time.Second*60, time.Millisecond*500).Should(Equal(agentsv1alpha1.SandboxRunning))

			By("Pausing the sandbox")
			originalSandbox := &agentsv1alpha1.Sandbox{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      sandbox.Name,
				Namespace: sandbox.Namespace,
			}, originalSandbox)).To(Succeed())

			originalSandbox.Spec.Paused = true
			Expect(updateSandboxSpec(ctx, originalSandbox)).To(Succeed())

			By("Verifying sandbox transitions to Paused phase")
			Eventually(func() agentsv1alpha1.SandboxPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, sandbox)
				return sandbox.Status.Phase
			}, time.Second*30, time.Millisecond*500).Should(Equal(agentsv1alpha1.SandboxPaused))

			By("Verifying the associated pod is deleted when paused")
			Eventually(func() bool {
				pod := &corev1.Pod{}
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, pod)
				return err != nil
			}, time.Second*30, time.Millisecond*500).Should(BeTrue())

			By("Resuming the sandbox")
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      sandbox.Name,
				Namespace: sandbox.Namespace,
			}, originalSandbox)).To(Succeed())

			originalSandbox.Spec.Paused = false
			Expect(updateSandboxSpec(ctx, originalSandbox)).To(Succeed())

			By("Verifying sandbox transitions to Resuming phase")
			Eventually(func() agentsv1alpha1.SandboxPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, sandbox)
				return sandbox.Status.Phase
			}, time.Second*30, time.Millisecond*500).Should(Equal(agentsv1alpha1.SandboxResuming))

			By("Verifying sandbox transitions to Running after resuming")
			Eventually(func() agentsv1alpha1.SandboxPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, sandbox)
				return sandbox.Status.Phase
			}, time.Second*60, time.Millisecond*500).Should(Equal(agentsv1alpha1.SandboxRunning))

			By("Verifying resumed sandbox has pod information")
			Expect(sandbox.Status.PodInfo.PodIP).NotTo(BeEmpty())
			Expect(sandbox.Status.SandboxIp).NotTo(BeEmpty())
		})
	})

	Context("termination", func() {
		It("should terminate sandbox properly", func() {
			By("Creating a new Sandbox")
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())

			By("Waiting for sandbox to reach Running phase")
			Eventually(func() agentsv1alpha1.SandboxPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, sandbox)
				return sandbox.Status.Phase
			}, time.Second*60, time.Millisecond*500).Should(Equal(agentsv1alpha1.SandboxRunning))

			By("Deleting the sandbox")
			Expect(k8sClient.Delete(ctx, sandbox)).To(Succeed())

			By("Verifying sandbox transitions to Terminating phase")
			Eventually(func() agentsv1alpha1.SandboxPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, sandbox)
				return sandbox.Status.Phase
			}, time.Second*30, time.Millisecond*500).Should(Equal(agentsv1alpha1.SandboxTerminating))

			By("Verifying the associated pod is deleted during termination")
			Eventually(func() bool {
				pod := &corev1.Pod{}
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, pod)
				return err != nil
			}, time.Second*30, time.Millisecond*500).Should(BeTrue())

			By("Verifying sandbox is completely deleted")
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, &agentsv1alpha1.Sandbox{})
				return err != nil
			}, time.Second*60, time.Millisecond*500).Should(BeTrue())
		})
	})

	Context("with shutdown time", func() {
		It("should delete sandbox when shutdown time is reached", func() {
			// Set a shutdown time that will expire soon
			shutdownTime := metav1.NewTime(time.Now().Add(5 * time.Second))
			sandbox.Spec.ShutdownTime = &shutdownTime

			By("Creating a new Sandbox with shutdown time")
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())

			By("Waiting for sandbox to be deleted due to shutdown time")
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, &agentsv1alpha1.Sandbox{})
				return err != nil
			}, time.Second*30, time.Millisecond*500).Should(BeTrue())
		})
	})

	Context("failed state", func() {
		It("should transition to failed state when pod fails", func() {
			// Create a pod configuration that will cause failure
			failingSandbox := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("failing-sandbox-%d", time.Now().UnixNano()),
					Namespace: Namespace,
				},
				Spec: agentsv1alpha1.SandboxSpec{
					SandboxTemplate: agentsv1alpha1.SandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:    "failing-container",
										Image:   "busybox:latest",
										Command: []string{"/bin/sh", "-c", "exit 1"},
									},
								},
								RestartPolicy: corev1.RestartPolicyNever,
							},
						},
					},
				},
			}

			By("Creating a failing Sandbox")
			Expect(k8sClient.Create(ctx, failingSandbox)).To(Succeed())

			By("Waiting for sandbox to transition to Failed phase")
			Eventually(func() agentsv1alpha1.SandboxPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      failingSandbox.Name,
					Namespace: failingSandbox.Namespace,
				}, failingSandbox)
				return failingSandbox.Status.Phase
			}, time.Second*90, time.Millisecond*500).Should(Equal(agentsv1alpha1.SandboxFailed))

			// Clean up the failing sandbox
			_ = k8sClient.Delete(ctx, failingSandbox)
		})
	})

	Context("inplace upgrade image", func() {
		It("should upgrade image inplace successfully", func() {
			By(fmt.Sprintf("Creating a new Sandbox with initial image namespace %s", sandbox.Namespace))
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())

			By("Waiting for sandbox to reach Running phase")
			Eventually(func() agentsv1alpha1.SandboxPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, sandbox)
				return sandbox.Status.Phase
			}, time.Second*30, time.Millisecond*500).Should(Equal(agentsv1alpha1.SandboxRunning))

			By("Verifying initial pod is running with initial image")
			var initialPodName string
			Eventually(func() string {
				pod := &corev1.Pod{}
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, pod)
				if err != nil {
					return ""
				}
				initialPodName = pod.Name
				for _, container := range pod.Spec.Containers {
					if container.Name == "test-container" && container.Image == initialImage {
						return container.Image
					}
				}
				return ""
			}, time.Second*10, time.Millisecond*500).Should(Equal(initialImage))

			By("Recording initial pod information")
			initialPod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      initialPodName,
				Namespace: sandbox.Namespace,
			}, initialPod)).To(Succeed())
			klog.InfoS("fetch initial pod status", "status", utils.DumpJson(initialPod.Status))

			By("Updating sandbox image for inplace upgrade")
			originalSandbox := &agentsv1alpha1.Sandbox{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      sandbox.Name,
				Namespace: sandbox.Namespace,
			}, originalSandbox)).To(Succeed())

			originalSandbox.Spec.Template.Spec.Containers[0].Image = updateImage
			Expect(updateSandboxSpec(ctx, originalSandbox)).To(Succeed())

			By("Verifying sandbox transitions to Updating phase")
			Eventually(func() int64 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, sandbox)
				return sandbox.Status.ObservedGeneration
			}, time.Minute*3, time.Millisecond*500).Should(Equal(int64(2)))
			Eventually(func() metav1.ConditionStatus {
				pod := &corev1.Pod{}
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, pod)
				if err != nil {
					return ""
				}
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, sandbox)
				condition := utils.GetSandboxCondition(&sandbox.Status, string(agentsv1alpha1.SandboxConditionReady))
				if condition.Status != metav1.ConditionTrue {
					klog.InfoS("fetch sandbox pod status", "status", utils.DumpJson(pod.Status))
				}
				return condition.Status
			}, time.Second*60, time.Millisecond*500).Should(Equal(metav1.ConditionTrue))

			By("Verifying sandbox eventually reaches Running phase with updated image")
			Eventually(func() string {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, sandbox)

				pod := &corev1.Pod{}
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, pod)
				if err != nil {
					return ""
				}

				for _, container := range pod.Spec.Containers {
					if container.Name == "test-container" {
						return container.Image
					}
				}
				return ""
			}, time.Second, time.Millisecond*500).Should(Equal(updateImage))

			By("Verifying sandbox has latest revision after update")
			Expect(sandbox.Status.UpdateRevision).NotTo(Equal(initialPod.Labels[agentsv1alpha1.PodLabelTemplateHash]))
			Expect(sandbox.Status.UpdateRevision).NotTo(BeEmpty())
		})
	})

})

func createNamespace(ctx context.Context) string {
	rand.Seed(time.Now().UnixNano())
	chars := "abcdefghijklmnopqrstuvwxyz0123456789"
	suffix := make([]byte, 5)
	for i := range suffix {
		suffix[i] = chars[rand.Intn(len(chars))]
	}
	name := "checkpoint-e2e-" + string(suffix)
	obj := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
	Expect(k8sClient.Create(ctx, obj)).To(Succeed())
	return name
}

func updateSandboxSpec(ctx context.Context, sandbox *agentsv1alpha1.Sandbox) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latestSandbox := &agentsv1alpha1.Sandbox{}
		err := k8sClient.Get(ctx, types.NamespacedName{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
		}, latestSandbox)
		if err != nil {
			return err
		}
		latestSandbox.Spec = sandbox.Spec
		return k8sClient.Update(ctx, latestSandbox)
	})
	return err
}
