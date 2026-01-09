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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
				SandboxTemplate: agentsv1alpha1.SandboxTemplate{
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

	Context("with volume claim templates", func() {
		It("should create SandboxSet with PVC templates", func() {
			By("Creating a SandboxSet with VolumeClaimTemplates")

			sandboxSetWithPVC := &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-sandboxset-pvc-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxSetSpec{
					Replicas: 1,
					SandboxTemplate: agentsv1alpha1.SandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "test-container",
										Image: "nginx:stable-alpine3.23",
										VolumeMounts: []corev1.VolumeMount{
											{
												Name:      "www",
												MountPath: "/usr/share/nginx/html",
											},
										},
									},
								},
								Volumes: []corev1.Volume{
									{
										Name: "www",
										VolumeSource: corev1.VolumeSource{
											PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
												ClaimName: "www",
											},
										},
									},
								},
							},
						},
						VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
							{
								ObjectMeta: metav1.ObjectMeta{
									Name: "www",
								},
								Spec: corev1.PersistentVolumeClaimSpec{
									AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
									Resources: corev1.VolumeResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceStorage: resource.MustParse("1Gi"),
										},
									},
								},
							},
						},
					},
				},
			}

			Expect(k8sClient.Create(ctx, sandboxSetWithPVC)).To(Succeed())

			By("Waiting for SandboxSet to have available replicas")
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxSetWithPVC.Name,
					Namespace: sandboxSetWithPVC.Namespace,
				}, sandboxSetWithPVC)
				return sandboxSetWithPVC.Status.AvailableReplicas
			}, time.Second*90, time.Millisecond*500).Should(Equal(int32(1)))

			By("Verifying PVCs are created for SandboxSet pods")
			// Find the sandbox created by the SandboxSet
			sandboxList := &agentsv1alpha1.SandboxList{}
			Eventually(func() int {
				err := k8sClient.List(ctx, sandboxList,
					&client.ListOptions{
						Namespace: namespace,
						LabelSelector: labels.SelectorFromSet(map[string]string{
							agentsv1alpha1.LabelSandboxPool: sandboxSetWithPVC.Name, // 使用正确的标签选择器
						}),
					})
				if err != nil {
					return 0
				}
				return len(sandboxList.Items)
			}, time.Second*30, time.Millisecond*500).Should(Equal(1))

			if len(sandboxList.Items) > 0 {
				sandboxInstance := &sandboxList.Items[0]
				pvcName := "www-" + sandboxInstance.Name

				By("Verifying PVC is bound")
				Eventually(func() corev1.PersistentVolumeClaimPhase {
					pvc := &corev1.PersistentVolumeClaim{}
					err := k8sClient.Get(ctx, types.NamespacedName{
						Name:      pvcName,
						Namespace: namespace,
					}, pvc)
					if err != nil {
						return ""
					}
					return pvc.Status.Phase
				}, time.Second*60, time.Millisecond*500).Should(Equal(corev1.ClaimBound))

				By("Verifying the pod has correct volume configuration")
				pod := &corev1.Pod{}
				Eventually(func() error {
					return k8sClient.Get(ctx, types.NamespacedName{
						Name:      sandboxInstance.Name,
						Namespace: namespace,
					}, pod)
				}, time.Second*30, time.Millisecond*500).Should(Succeed())

				var foundVolume bool
				for _, volume := range pod.Spec.Volumes {
					if volume.Name == "www" && volume.PersistentVolumeClaim != nil {
						Expect(volume.PersistentVolumeClaim.ClaimName).To(Equal(pvcName))
						foundVolume = true
						break
					}
				}
				Expect(foundVolume).To(BeTrue(), "Expected to find volume 'www' in pod")

				var foundMount bool
				for _, container := range pod.Spec.Containers {
					if container.Name == "test-container" {
						for _, mount := range container.VolumeMounts {
							if mount.Name == "www" && mount.MountPath == "/usr/share/nginx/html" {
								foundMount = true
								break
							}
						}
					}
				}
				Expect(foundMount).To(BeTrue(), "Expected to find volume mount 'www' in container")
			}
		})
	})
})
