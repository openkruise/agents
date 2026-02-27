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

})
