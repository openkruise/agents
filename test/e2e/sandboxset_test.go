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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	intstrutil "k8s.io/apimachinery/pkg/util/intstr"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

var _ = Describe("SandboxSet", func() {
	var (
		sandbox      *agentsv1alpha1.SandboxSet
		ctx          = context.Background()
		namespace    string
		initialImage string
	)

	BeforeEach(func() {
		namespace = createNamespace(ctx)
		initialImage = "nginx:stable-alpine3.23"
		// Create a basic SandboxSet resource
		sandbox = &agentsv1alpha1.SandboxSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("test-sandboxset-%d", time.Now().UnixNano()),
				Namespace: namespace,
			},
			Spec: agentsv1alpha1.SandboxSetSpec{
				Replicas: 2,
				EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"sandboxset": "true",
							},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test-container",
									Image: initialImage,
								},
							},
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

	Context("normal case", func() {
		It("creation and scale up", func() {
			By("Creating a new SandboxSet")
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())

			By("Verifying the sandboxset is created")
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, sandbox)
			}, time.Minute*10, time.Millisecond*500).Should(Succeed())

			By("Verifying the sandboxset AvailableReplicas")
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, sandbox)
				return sandbox.Status.AvailableReplicas
			}, time.Second*30, time.Millisecond*500).Should(Equal(int32(2)))

			By("scale up sandboxset to 3")
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      sandbox.Name,
				Namespace: sandbox.Namespace,
			}, sandbox)).To(Succeed())

			sandbox.Spec.Replicas = 3
			Expect(k8sClient.Update(ctx, sandbox)).To(Succeed())

			By("Verifying the sandboxset AvailableReplicas")
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, sandbox)
				return sandbox.Status.AvailableReplicas
			}, time.Second*30, time.Millisecond*500).Should(Equal(int32(3)))
		})
	})

	Context("MaxUnavailable tests", func() {
		It("should respect MaxUnavailable when scaling up with integer value", func() {
			By("Creating a SandboxSet with MaxUnavailable=2")
			maxUnavailable := intstrutil.FromInt(2)
			sandbox.Spec.Replicas = 10
			sandbox.Spec.ScaleStrategy = agentsv1alpha1.SandboxSetScaleStrategy{
				MaxUnavailable: &maxUnavailable,
			}
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())

			By("Verifying initial sandboxes are created with MaxUnavailable limit")
			// Should create 2 sandboxes first (limited by MaxUnavailable)
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, sandbox)
				return sandbox.Status.Replicas
			}, time.Second*10, time.Millisecond*500).Should(BeNumerically(">=", int32(2)))

			By("Eventually reaching target replicas (10) in multiple batches")
			// Eventually all 10 replicas should be created
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, sandbox)
				return sandbox.Status.AvailableReplicas
			}, time.Minute*3, time.Second*2).Should(Equal(int32(10)))

			By("Verifying all replicas are available")
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      sandbox.Name,
				Namespace: sandbox.Namespace,
			}, sandbox)).To(Succeed())
			Expect(sandbox.Status.Replicas).To(Equal(int32(10)))
			Expect(sandbox.Status.AvailableReplicas).To(Equal(int32(10)))
		})

		It("should respect MaxUnavailable with percentage value", func() {
			By("Creating a SandboxSet with MaxUnavailable=50%")
			maxUnavailable := intstrutil.FromString("50%")
			sandbox.Spec.Replicas = 10
			sandbox.Spec.ScaleStrategy = agentsv1alpha1.SandboxSetScaleStrategy{
				MaxUnavailable: &maxUnavailable,
			}
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())

			By("Verifying initial sandboxes are created with 50% limit")
			// Should create 5 sandboxes first (50% of 10)
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, sandbox)
				return sandbox.Status.Replicas
			}, time.Second*10, time.Millisecond*500).Should(BeNumerically(">=", int32(5)))

			By("Eventually reaching target replicas (10)")
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, sandbox)
				return sandbox.Status.AvailableReplicas
			}, time.Minute*3, time.Second*2).Should(Equal(int32(10)))

			By("Verifying all replicas are available")
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      sandbox.Name,
				Namespace: sandbox.Namespace,
			}, sandbox)).To(Succeed())
			Expect(sandbox.Status.Replicas).To(Equal(int32(10)))
			Expect(sandbox.Status.AvailableReplicas).To(Equal(int32(10)))
		})

		It("should block scale up when MaxUnavailable=0", func() {
			By("Creating a SandboxSet with MaxUnavailable=0")
			maxUnavailable := intstrutil.FromInt(0)
			sandbox.Spec.Replicas = 5
			sandbox.Spec.ScaleStrategy = agentsv1alpha1.SandboxSetScaleStrategy{
				MaxUnavailable: &maxUnavailable,
			}
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())

			By("Verifying no sandboxes are created due to MaxUnavailable=0")
			Consistently(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, sandbox)
				return sandbox.Status.Replicas
			}, time.Second*10, time.Millisecond*500).Should(Equal(int32(0)))
		})

		It("should scale up gradually with MaxUnavailable=1", func() {
			By("Creating a SandboxSet with MaxUnavailable=1")
			maxUnavailable := intstrutil.FromInt(1)
			sandbox.Spec.Replicas = 5
			sandbox.Spec.ScaleStrategy = agentsv1alpha1.SandboxSetScaleStrategy{
				MaxUnavailable: &maxUnavailable,
			}
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())

			By("Verifying sandboxes are created one by one")
			// First check we have at least 1
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, sandbox)
				return sandbox.Status.Replicas
			}, time.Second*10, time.Millisecond*500).Should(BeNumerically(">=", int32(1)))

			By("Eventually reaching target replicas (5)")
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, sandbox)
				return sandbox.Status.AvailableReplicas
			}, time.Minute*5, time.Second*2).Should(Equal(int32(5)))
		})

		It("should not limit scale down with MaxUnavailable", func() {
			By("Creating a SandboxSet with 5 replicas and no MaxUnavailable")
			sandbox.Spec.Replicas = 5
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())

			By("Waiting for all 5 sandboxes to be available")
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, sandbox)
				return sandbox.Status.AvailableReplicas
			}, time.Minute*2, time.Second).Should(Equal(int32(5)))

			By("Scaling down to 2 with MaxUnavailable=1")
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      sandbox.Name,
				Namespace: sandbox.Namespace,
			}, sandbox)).To(Succeed())

			maxUnavailable := intstrutil.FromInt(1)
			sandbox.Spec.Replicas = 2
			sandbox.Spec.ScaleStrategy = agentsv1alpha1.SandboxSetScaleStrategy{
				MaxUnavailable: &maxUnavailable,
			}
			Expect(k8sClient.Update(ctx, sandbox)).To(Succeed())

			By("Verifying scale down is not limited by MaxUnavailable")
			// Scale down should delete 3 sandboxes immediately
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, sandbox)
				return sandbox.Status.Replicas
			}, time.Second*30, time.Second).Should(Equal(int32(2)))
		})

		It("should handle scale up from existing replicas with MaxUnavailable", func() {
			By("Creating a SandboxSet with 3 replicas initially")
			sandbox.Spec.Replicas = 3
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())

			By("Waiting for initial 3 sandboxes to be available")
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, sandbox)
				return sandbox.Status.AvailableReplicas
			}, time.Minute*2, time.Second).Should(Equal(int32(3)))

			By("Scaling up to 8 with MaxUnavailable=2")
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      sandbox.Name,
				Namespace: sandbox.Namespace,
			}, sandbox)).To(Succeed())

			maxUnavailable := intstrutil.FromInt(2)
			sandbox.Spec.Replicas = 8
			sandbox.Spec.ScaleStrategy = agentsv1alpha1.SandboxSetScaleStrategy{
				MaxUnavailable: &maxUnavailable,
			}
			Expect(k8sClient.Update(ctx, sandbox)).To(Succeed())

			By("Verifying scale up is limited by MaxUnavailable")
			// Should add 2 sandboxes at a time (limited by MaxUnavailable=2)
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, sandbox)
				return sandbox.Status.Replicas
			}, time.Second*15, time.Millisecond*500).Should(BeNumerically(">=", int32(5)))

			By("Eventually reaching target replicas (8)")
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, sandbox)
				return sandbox.Status.AvailableReplicas
			}, time.Minute*3, time.Second*2).Should(Equal(int32(8)))
		})
	})

})
