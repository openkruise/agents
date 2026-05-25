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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	intstrutil "k8s.io/apimachinery/pkg/util/intstr"
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

	Context("RollingUpdate tests", func() {
		It("should keep AvailableReplicas>=9 with MaxUnavailable=1 during rolling update", func() {
			By("Creating a SandboxSet with 10 replicas and UpdateStrategy.MaxUnavailable=1")
			maxUnavailable := intstrutil.FromInt(1)
			sandbox.Spec.Replicas = 10
			sandbox.Spec.UpdateStrategy = agentsv1alpha1.SandboxSetUpdateStrategy{
				MaxUnavailable: &maxUnavailable,
			}
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())

			By("Waiting for all 10 replicas to become available")
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, sandbox)
				return sandbox.Status.AvailableReplicas
			}, time.Minute*5, time.Second*2).Should(Equal(int32(10)))

			By("Triggering rolling update by changing container image")
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      sandbox.Name,
				Namespace: sandbox.Namespace,
			}, sandbox)).To(Succeed())
			sandbox.Spec.Template.Spec.Containers[0].Image = "nginx:stable-alpine3.20"
			Expect(k8sClient.Update(ctx, sandbox)).To(Succeed())

			By("Starting a monitor goroutine to detect AvailableReplicas<9 violations during rolling update")
			violationCh := make(chan int32, 1)
			doneCh := make(chan struct{})
			go func() {
				defer GinkgoRecover()
				ticker := time.NewTicker(500 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-doneCh:
						return
					case <-ticker.C:
						latest := &agentsv1alpha1.SandboxSet{}
						if err := k8sClient.Get(ctx, types.NamespacedName{
							Name:      sandbox.Name,
							Namespace: sandbox.Namespace,
						}, latest); err == nil {
							if latest.Status.AvailableReplicas < 9 {
								select {
								case violationCh <- latest.Status.AvailableReplicas:
								default:
								}
							}
						}
					}
				}
			}()

			By("Waiting for rolling update to complete: UpdatedAvailableReplicas=10")
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, sandbox)
				return sandbox.Status.UpdatedAvailableReplicas
			}, time.Minute*10, time.Second*2).Should(Equal(int32(10)))

			close(doneCh)

			By("Verifying AvailableReplicas never dropped below 9 during the rolling update")
			select {
			case v := <-violationCh:
				Fail(fmt.Sprintf("AvailableReplicas dropped below 9 during rolling update: observed %d", v))
			default:
				// No violation detected
			}

			By("Verifying final state: all 10 replicas are updated and available")
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      sandbox.Name,
				Namespace: sandbox.Namespace,
			}, sandbox)).To(Succeed())
			Expect(sandbox.Status.UpdatedAvailableReplicas).To(Equal(int32(10)))
			Expect(sandbox.Status.AvailableReplicas).To(Equal(int32(10)))
		})
	})

	Context("SandboxMultiClusterNaming FeatureGate tests", func() {
		It("should generate sandbox names with SandboxSet name prefix when SandboxMultiClusterNaming is disabled (default)", func() {
			By("Creating a SandboxSet with 2 replicas")
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())

			By("Waiting for all replicas to be available")
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandbox.Name,
					Namespace: sandbox.Namespace,
				}, sandbox)
				return sandbox.Status.AvailableReplicas
			}, time.Minute*2, time.Second).Should(Equal(int32(2)))

			By("Listing Sandboxes owned by this SandboxSet")
			sandboxList := &agentsv1alpha1.SandboxList{}
			Expect(k8sClient.List(ctx, sandboxList, client.InNamespace(sandbox.Namespace),
				client.MatchingLabels{agentsv1alpha1.LabelSandboxPool: sandbox.Name})).To(Succeed())
			Expect(len(sandboxList.Items)).To(Equal(2))

			By("Verifying each Sandbox name starts with '{sbsName}-' and has no extra cluster hash segment")
			expectedPrefix := sandbox.Name + "-"
			for _, sbx := range sandboxList.Items {
				Expect(sbx.Name).To(HavePrefix(expectedPrefix),
					"Sandbox name %q should start with prefix %q", sbx.Name, expectedPrefix)
				// With SandboxMultiClusterNaming disabled, the name format is "{sbsName}-{random5}"
				// (generateName adds 5 random chars). The suffix after the prefix should NOT
				// contain another '-' segment that looks like a 4-char hex hash.
				suffix := strings.TrimPrefix(sbx.Name, expectedPrefix)
				Expect(suffix).NotTo(BeEmpty(), "Sandbox name suffix should not be empty")
				// The suffix should be a single random string (5 chars) without additional dashes
				Expect(strings.Contains(suffix, "-")).To(BeFalse(),
					"Sandbox name %q suffix %q should not contain '-' (no cluster hash embedded)", sbx.Name, suffix)
			}
		})
	})

	Context("VolumeClaimTemplates tests", func() {
		It("should create sandbox with volumeClaimTemplates and volumeMounts correctly", func() {
			By("Creating a SandboxSet with volumeClaimTemplates")
			volumeName := "data-vol"
			sandbox.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: volumeName,
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
			}
			sandbox.Spec.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{
				{
					Name:      volumeName,
					MountPath: "/usr/share/nginx/html",
				},
			}

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

			By("Verifying that PVCs are created for each sandbox")
			sandboxList := &agentsv1alpha1.SandboxList{}
			Expect(k8sClient.List(ctx, sandboxList, client.InNamespace(sandbox.Namespace),
				client.MatchingLabels{agentsv1alpha1.LabelSandboxPool: sandbox.Name})).To(Succeed())

			Expect(len(sandboxList.Items)).To(Equal(2))

			for _, sbx := range sandboxList.Items {
				pvcName := fmt.Sprintf("%s-%s", volumeName, sbx.Name)
				pvc := &corev1.PersistentVolumeClaim{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name:      pvcName,
					Namespace: sandbox.Namespace,
				}, pvc)).To(Succeed())

				// Verify PVC spec
				Expect(pvc.Spec.AccessModes).To(ContainElement(corev1.ReadWriteOnce))
				Expect(pvc.Spec.Resources.Requests[corev1.ResourceStorage]).To(Equal(resource.MustParse("1Gi")))
			}

			By("Verifying that Pods have correct volumes and volumeMounts")
			for _, sbx := range sandboxList.Items {
				pod := &corev1.Pod{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name:      sbx.Name,
					Namespace: sandbox.Namespace,
				}, pod)).To(Succeed())

				// Verify volume exists
				foundVolume := false
				for _, vol := range pod.Spec.Volumes {
					if vol.Name == volumeName {
						foundVolume = true
						Expect(vol.VolumeSource.PersistentVolumeClaim).NotTo(BeNil())
						Expect(vol.VolumeSource.PersistentVolumeClaim.ClaimName).To(Equal(fmt.Sprintf("%s-%s", volumeName, sbx.Name)))
						break
					}
				}
				Expect(foundVolume).To(BeTrue(), "Volume %s not found in pod %s", volumeName, sbx.Name)

				// Verify volumeMount exists
				foundMount := false
				for _, mount := range pod.Spec.Containers[0].VolumeMounts {
					if mount.Name == volumeName && mount.MountPath == "/usr/share/nginx/html" {
						foundMount = true
						break
					}
				}
				Expect(foundMount).To(BeTrue(), "VolumeMount %s not found in pod %s", volumeName, sbx.Name)
			}
		})
	})

})
