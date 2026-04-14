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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

var _ = Describe("SandboxClaim", func() {
	var (
		ctx       = context.Background()
		namespace string
	)

	// Helper function to list sandboxes claimed by a specific SandboxClaim
	// Uses AnnotationOwner (which stores the claim's UID) for filtering
	listClaimedSandboxes := func(ctx context.Context, claim *agentsv1alpha1.SandboxClaim) ([]agentsv1alpha1.Sandbox, error) {
		sandboxList := &agentsv1alpha1.SandboxList{}
		if err := k8sClient.List(ctx, sandboxList, client.InNamespace(claim.Namespace)); err != nil {
			return nil, err
		}

		// AnnotationOwner is set to the claim's UID for uniqueness
		expectedOwner := string(claim.UID)
		claimed := []agentsv1alpha1.Sandbox{}
		for _, sandbox := range sandboxList.Items {
			if sandbox.Annotations != nil &&
				sandbox.Annotations[agentsv1alpha1.AnnotationOwner] == expectedOwner {
				claimed = append(claimed, sandbox)
			}
		}
		return claimed, nil
	}

	BeforeEach(func() {
		namespace = createNamespace(ctx)
	})

	AfterEach(func() {
		// Clean up namespace
		ns := &corev1.Namespace{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: namespace}, ns); err == nil {
			_ = k8sClient.Delete(ctx, ns)
		}
	})

	Context("Basic claim flow", func() {
		var (
			sandboxSet      *agentsv1alpha1.SandboxSet
			sandboxSetEmpty *agentsv1alpha1.SandboxSet
			sandboxClaim    *agentsv1alpha1.SandboxClaim
		)

		BeforeEach(func() {
			// Create a SandboxSet with 5 replicas
			sandboxSet = &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-pool-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxSetSpec{
					Replicas: 5,
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "test-container",
										Image: "nginx:stable-alpine3.23",
									},
								},
							},
						},
					},
				},
			}
			sandboxSetEmpty = sandboxSet.DeepCopy()
			sandboxSetEmpty.Spec.Replicas = 0
			sandboxSetEmpty.Name = sandboxSet.Name + "-empty"
			Expect(k8sClient.Create(ctx, sandboxSet)).To(Succeed())
			Expect(k8sClient.Create(ctx, sandboxSetEmpty)).To(Succeed())

			// Wait for SandboxSet to be ready
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxSet.Name,
					Namespace: sandboxSet.Namespace,
				}, sandboxSet)
				return sandboxSet.Status.AvailableReplicas
			}, time.Minute*2, time.Second).Should(Equal(int32(5)))
		})

		AfterEach(func() {
			if sandboxClaim != nil {
				_ = k8sClient.Delete(ctx, sandboxClaim)
			}
			if sandboxSet != nil {
				_ = k8sClient.Delete(ctx, sandboxSet)
			}
			if sandboxSetEmpty != nil {
				_ = k8sClient.Delete(ctx, sandboxSetEmpty)
			}
		})

		It("should successfully claim a single sandbox", func() {
			By("Creating a SandboxClaim with replicas=1")
			sandboxClaim = &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-claim-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName:    sandboxSet.Name,
					Replicas:        ptr.To(int32(1)),
					SkipInitRuntime: true,
				},
			}
			Expect(k8sClient.Create(ctx, sandboxClaim)).To(Succeed())

			By("Verifying the claim transitions to Completed phase")
			Eventually(func() agentsv1alpha1.SandboxClaimPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxClaim.Name,
					Namespace: sandboxClaim.Namespace,
				}, sandboxClaim)
				return sandboxClaim.Status.Phase
			}, time.Minute, time.Second).Should(Equal(agentsv1alpha1.SandboxClaimPhaseCompleted))

			By("Verifying claimedReplicas equals 1")
			Expect(sandboxClaim.Status.ClaimedReplicas).To(Equal(int32(1)))

			By("Verifying completion time is set")
			Expect(sandboxClaim.Status.CompletionTime).NotTo(BeNil())

			By("Verifying at least one sandbox is claimed by the claim")
			claimedSandboxes, err := listClaimedSandboxes(ctx, sandboxClaim)
			Expect(err).NotTo(HaveOccurred())
			Expect(claimedSandboxes).To(HaveLen(1))
		})

		It("should successfully claim multiple sandboxes", func() {
			By("Creating a SandboxClaim with replicas=3")
			sandboxClaim = &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-claim-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName:    sandboxSet.Name,
					Replicas:        ptr.To(int32(3)),
					SkipInitRuntime: true,
				},
			}
			Expect(k8sClient.Create(ctx, sandboxClaim)).To(Succeed())

			By("Verifying the claim transitions to Completed phase")
			Eventually(func() agentsv1alpha1.SandboxClaimPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxClaim.Name,
					Namespace: sandboxClaim.Namespace,
				}, sandboxClaim)
				return sandboxClaim.Status.Phase
			}, time.Minute, time.Second).Should(Equal(agentsv1alpha1.SandboxClaimPhaseCompleted))

			By("Verifying claimedReplicas equals 3")
			Expect(sandboxClaim.Status.ClaimedReplicas).To(Equal(int32(3)))

			By("Verifying exactly 3 sandboxes are claimed by the claim")
			claimedSandboxes, err := listClaimedSandboxes(ctx, sandboxClaim)
			Expect(err).NotTo(HaveOccurred())
			Expect(claimedSandboxes).To(HaveLen(3))
		})

		It("should show progress during claiming", func() {
			By("Creating a SandboxClaim with replicas=3")
			sandboxClaim = &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-claim-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName:    sandboxSet.Name,
					Replicas:        ptr.To(int32(3)),
					SkipInitRuntime: true,
				},
			}
			Expect(k8sClient.Create(ctx, sandboxClaim)).To(Succeed())

			By("Verifying the claim phase is valid (Claiming or Completed)")
			Eventually(func() bool {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxClaim.Name,
					Namespace: sandboxClaim.Namespace,
				}, sandboxClaim)
				phase := sandboxClaim.Status.Phase
				// Accept any valid phase - if Controller is fast, it might already be Completed
				return phase == agentsv1alpha1.SandboxClaimPhaseClaiming ||
					phase == agentsv1alpha1.SandboxClaimPhaseCompleted
			}, time.Second*10, time.Millisecond*500).Should(BeTrue())

			By("Verifying claimedReplicas is within valid range")
			_ = k8sClient.Get(ctx, types.NamespacedName{
				Name:      sandboxClaim.Name,
				Namespace: sandboxClaim.Namespace,
			}, sandboxClaim)
			initialClaimed := sandboxClaim.Status.ClaimedReplicas
			Expect(initialClaimed).To(BeNumerically(">=", 0))
			Expect(initialClaimed).To(BeNumerically("<=", 3))

			By("Eventually reaching the target replicas")
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxClaim.Name,
					Namespace: sandboxClaim.Namespace,
				}, sandboxClaim)
				return sandboxClaim.Status.ClaimedReplicas
			}, time.Minute, time.Second).Should(Equal(int32(3)))

			By("Verifying the claim eventually reaches Completed phase")
			Expect(sandboxClaim.Status.Phase).To(Equal(agentsv1alpha1.SandboxClaimPhaseCompleted))
		})

		It("create on no stock", func() {
			By("Creating a SandboxClaim with replicas=1")
			sandboxClaim = &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-claim-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName:     sandboxSetEmpty.Name,
					Replicas:         ptr.To(int32(1)),
					WaitReadyTimeout: &metav1.Duration{Duration: time.Minute},
					CreateOnNoStock:  true,
					SkipInitRuntime:  true,
				},
			}
			Expect(k8sClient.Create(ctx, sandboxClaim)).To(Succeed())

			By("Verifying the claim transitions to Completed phase")
			Eventually(func() agentsv1alpha1.SandboxClaimPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxClaim.Name,
					Namespace: sandboxClaim.Namespace,
				}, sandboxClaim)
				return sandboxClaim.Status.Phase
			}, time.Minute, time.Second).Should(Equal(agentsv1alpha1.SandboxClaimPhaseCompleted))

			By("Verifying claimedReplicas equals 1")
			Expect(sandboxClaim.Status.ClaimedReplicas).To(Equal(int32(1)))

			By("Verifying completion time is set")
			Expect(sandboxClaim.Status.CompletionTime).NotTo(BeNil())

			By("Verifying at least one sandbox is claimed by the claim")
			claimedSandboxes, err := listClaimedSandboxes(ctx, sandboxClaim)
			Expect(err).NotTo(HaveOccurred())
			Expect(claimedSandboxes).To(HaveLen(1))
		})
	})

	Context("CPU resize on claim", func() {
		var (
			sandboxSet      *agentsv1alpha1.SandboxSet
			sandboxClaim    *agentsv1alpha1.SandboxClaim
			warmSandboxName string
		)

		BeforeEach(func() {
			sandboxSet = &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-pool-cpu-resize-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxSetSpec{
					Replicas: 1,
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "test-container",
										Image: "nginx:stable-alpine3.23",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												corev1.ResourceCPU:    resource.MustParse("250m"),
												corev1.ResourceMemory: resource.MustParse("128Mi"),
											},
											Limits: corev1.ResourceList{
												corev1.ResourceCPU:    resource.MustParse("500m"),
												corev1.ResourceMemory: resource.MustParse("256Mi"),
											},
										},
									},
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, sandboxSet)).To(Succeed())

			By("Waiting for SandboxSet warm pool to be ready")
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxSet.Name,
					Namespace: sandboxSet.Namespace,
				}, sandboxSet)
				return sandboxSet.Status.AvailableReplicas
			}, time.Minute*2, time.Second).Should(Equal(int32(1)))

			By("Verifying warm sandbox starts with small CPU resources")
			Eventually(func() int {
				sandboxList := &agentsv1alpha1.SandboxList{}
				if err := k8sClient.List(ctx, sandboxList, client.InNamespace(namespace), client.MatchingLabels{
					agentsv1alpha1.LabelSandboxPool: sandboxSet.Name,
				}); err != nil {
					return -1
				}
				if len(sandboxList.Items) == 1 {
					warmSandboxName = sandboxList.Items[0].Name
				}
				return len(sandboxList.Items)
			}, time.Minute, time.Second).Should(Equal(1))

			Eventually(func() int64 {
				pod := &corev1.Pod{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      warmSandboxName,
					Namespace: namespace,
				}, pod); err != nil || len(pod.Spec.Containers) == 0 {
					return -1
				}
				cpuReq, ok := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
				if !ok {
					return -1
				}
				return cpuReq.MilliValue()
			}, time.Minute, time.Second).Should(Equal(int64(250)))

			Eventually(func() corev1.PodQOSClass {
				pod := &corev1.Pod{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      warmSandboxName,
					Namespace: namespace,
				}, pod); err != nil {
					return ""
				}
				return pod.Status.QOSClass
			}, time.Minute, time.Second).Should(Equal(corev1.PodQOSBurstable))
		})

		AfterEach(func() {
			if sandboxClaim != nil {
				_ = k8sClient.Delete(ctx, sandboxClaim)
			}
			if sandboxSet != nil {
				_ = k8sClient.Delete(ctx, sandboxSet)
			}
		})

		It("should reject CPU resize that would change QoS class via sandbox condition", func() {
			// Pool: CPU req=250m/lim=500m, Memory req=128Mi/lim=256Mi → Burstable.
			// Setting CPU req=500m/lim=500m keeps memory req=128Mi/lim=256Mi unchanged.
			// Since memory req!=lim, QoS stays Burstable — this resize is safe.
			// But if we set to match ALL req==lim, QoS changes.
			//
			// Create a separate pool with CPU req=250m/lim=500m, Memory req=128Mi/lim=128Mi → Burstable (CPU req!=lim).
			// Then resize CPU to 500m → req=500m/lim=500m, Memory stays req=128Mi/lim=128Mi.
			// All resources now have req==lim → QoS changes from Burstable to Guaranteed.
			qosBreakSet := &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-pool-qos-break-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxSetSpec{
					Replicas: 1,
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "main",
										Image: "nginx:stable-alpine3.23",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												corev1.ResourceCPU:    resource.MustParse("250m"),
												corev1.ResourceMemory: resource.MustParse("128Mi"),
											},
											Limits: corev1.ResourceList{
												corev1.ResourceCPU:    resource.MustParse("500m"),
												corev1.ResourceMemory: resource.MustParse("128Mi"),
											},
										},
									},
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, qosBreakSet)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, qosBreakSet) }()

			By("Waiting for pool to be ready")
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      qosBreakSet.Name,
					Namespace: qosBreakSet.Namespace,
				}, qosBreakSet)
				return qosBreakSet.Status.AvailableReplicas
			}, time.Minute*2, time.Second).Should(Equal(int32(1)))

			By("Verifying warm sandbox is Burstable")
			var warmName string
			Eventually(func() corev1.PodQOSClass {
				sandboxList := &agentsv1alpha1.SandboxList{}
				if err := k8sClient.List(ctx, sandboxList, client.InNamespace(namespace), client.MatchingLabels{
					agentsv1alpha1.LabelSandboxPool: qosBreakSet.Name,
				}); err != nil || len(sandboxList.Items) == 0 {
					return ""
				}
				warmName = sandboxList.Items[0].Name
				pod := &corev1.Pod{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      warmName,
					Namespace: namespace,
				}, pod); err != nil {
					return ""
				}
				return pod.Status.QOSClass
			}, time.Minute, time.Second).Should(Equal(corev1.PodQOSBurstable))

			By("Creating a claim with CPU requests/limits=500m that would change QoS from Burstable to Guaranteed")
			qosBreakClaim := &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-claim-qos-break-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName:    qosBreakSet.Name,
					Replicas:        ptr.To(int32(1)),
					SkipInitRuntime: true,
					ClaimTimeout:    &metav1.Duration{Duration: 30 * time.Second},
					WaitReadyTimeout: &metav1.Duration{
						Duration: 30 * time.Second,
					},
					InplaceUpdate: &agentsv1alpha1.SandboxClaimInplaceUpdateOptions{
						Resources: &agentsv1alpha1.SandboxClaimInplaceUpdateResourcesOptions{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
							Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, qosBreakClaim)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, qosBreakClaim) }()

			By("Verifying sandbox InplaceUpdate condition reports QoS change failure")
			Eventually(func() string {
				sbx := &agentsv1alpha1.Sandbox{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      warmName,
					Namespace: namespace,
				}, sbx); err != nil {
					return ""
				}
				for _, cond := range sbx.Status.Conditions {
					if cond.Type == string(agentsv1alpha1.SandboxConditionInplaceUpdate) &&
						cond.Reason == agentsv1alpha1.SandboxInplaceUpdateReasonFailed {
						return cond.Message
					}
				}
				return ""
			}, time.Minute*2, time.Second).Should(ContainSubstring("QoS"))

			By("Verifying pod QoS class remains Burstable (resize was not applied)")
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      warmName,
				Namespace: namespace,
			}, pod)).To(Succeed())
			Expect(pod.Status.QOSClass).To(Equal(corev1.PodQOSBurstable))

			By("Verifying spec resources remain at original values (250m req / 500m lim)")
			specRes := pod.Spec.Containers[0].Resources
			Expect(specRes.Requests[corev1.ResourceCPU]).To(Equal(resource.MustParse("250m")))
			Expect(specRes.Limits[corev1.ResourceCPU]).To(Equal(resource.MustParse("500m")))
		})

		It("should allow CPU resize that preserves QoS class (only requests changed)", func() {
			By("Creating a SandboxClaim that only resizes CPU requests (not limits)")
			onlyReqClaim := &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-claim-qos-safe-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName:    sandboxSet.Name,
					Replicas:        ptr.To(int32(1)),
					SkipInitRuntime: true,
					WaitReadyTimeout: &metav1.Duration{
						Duration: 2 * time.Minute,
					},
					InplaceUpdate: &agentsv1alpha1.SandboxClaimInplaceUpdateOptions{
						Resources: &agentsv1alpha1.SandboxClaimInplaceUpdateResourcesOptions{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("300m")},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, onlyReqClaim)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, onlyReqClaim) }()

			By("Verifying the claim transitions to Completed phase")
			Eventually(func() agentsv1alpha1.SandboxClaimPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      onlyReqClaim.Name,
					Namespace: onlyReqClaim.Namespace,
				}, onlyReqClaim)
				return onlyReqClaim.Status.Phase
			}, time.Minute*3, time.Second).Should(Equal(agentsv1alpha1.SandboxClaimPhaseCompleted))

			By("Verifying pod CPU request is updated while QoS stays Burstable")
			claimedSandboxes, err := listClaimedSandboxes(ctx, onlyReqClaim)
			Expect(err).NotTo(HaveOccurred())
			Expect(claimedSandboxes).To(HaveLen(1))

			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      claimedSandboxes[0].Name,
				Namespace: namespace,
			}, pod)).To(Succeed())
			Expect(pod.Status.QOSClass).To(Equal(corev1.PodQOSBurstable))

			By("Verifying spec resources: CPU request=300m, limit=500m (unchanged)")
			specRes := pod.Spec.Containers[0].Resources
			Expect(specRes.Requests[corev1.ResourceCPU]).To(Equal(resource.MustParse("300m")))
			Expect(specRes.Limits[corev1.ResourceCPU]).To(Equal(resource.MustParse("500m")))

			By("Verifying status resources reflect the actual resize (if populated by kubelet)")
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      claimedSandboxes[0].Name,
				Namespace: namespace,
			}, pod)).To(Succeed())
			if len(pod.Status.ContainerStatuses) > 0 && pod.Status.ContainerStatuses[0].Resources != nil {
				cpuReq, ok := pod.Status.ContainerStatuses[0].Resources.Requests[corev1.ResourceCPU]
				if ok {
					Expect(cpuReq.MilliValue()).To(Equal(int64(300)))
				}
			}
		})

		It("should resize CPU and inplace-update image when both are set on claim", func() {
			const targetImage = "nginx:stable-alpine3.20"
			By("Creating a SandboxClaim with cpu requests/limits=1 and inplaceUpdate.image")
			sandboxClaim = &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-claim-cpu-image-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName:    sandboxSet.Name,
					Replicas:        ptr.To(int32(1)),
					SkipInitRuntime: true,
					WaitReadyTimeout: &metav1.Duration{
						Duration: 5 * time.Minute,
					},
					InplaceUpdate: &agentsv1alpha1.SandboxClaimInplaceUpdateOptions{
						Image: targetImage,
						Resources: &agentsv1alpha1.SandboxClaimInplaceUpdateResourcesOptions{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
							Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, sandboxClaim)).To(Succeed())

			By("Verifying the claim transitions to Completed phase")
			Eventually(func() agentsv1alpha1.SandboxClaimPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxClaim.Name,
					Namespace: sandboxClaim.Namespace,
				}, sandboxClaim)
				return sandboxClaim.Status.Phase
			}, time.Minute*5, time.Second).Should(Equal(agentsv1alpha1.SandboxClaimPhaseCompleted))
			Expect(sandboxClaim.Status.ClaimedReplicas).To(Equal(int32(1)))

			By("Verifying the claimed sandbox comes from warm pool")
			claimedSandboxes, err := listClaimedSandboxes(ctx, sandboxClaim)
			Expect(err).NotTo(HaveOccurred())
			Expect(claimedSandboxes).To(HaveLen(1))
			Expect(claimedSandboxes[0].Name).To(Equal(warmSandboxName))

			By("Verifying Sandbox template has target image and target CPU")
			Eventually(func() bool {
				sbx := &agentsv1alpha1.Sandbox{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      warmSandboxName,
					Namespace: namespace,
				}, sbx); err != nil {
					return false
				}
				if sbx.Spec.Template == nil || len(sbx.Spec.Template.Spec.Containers) == 0 {
					return false
				}
				c := sbx.Spec.Template.Spec.Containers[0]
				if c.Image != targetImage {
					return false
				}
				req, ok := c.Resources.Requests[corev1.ResourceCPU]
				if !ok || req.MilliValue() != 1000 {
					return false
				}
				lim, ok := c.Resources.Limits[corev1.ResourceCPU]
				return ok && lim.MilliValue() == 1000
			}, time.Minute*5, time.Second).Should(BeTrue())

			By("Verifying pod reflects target CPU and updated image")
			Eventually(func() bool {
				pod := &corev1.Pod{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      warmSandboxName,
					Namespace: namespace,
				}, pod); err != nil || len(pod.Spec.Containers) == 0 {
					return false
				}
				c := pod.Spec.Containers[0]
				if c.Image != targetImage {
					return false
				}
				req, ok := c.Resources.Requests[corev1.ResourceCPU]
				if !ok || req.MilliValue() != 1000 {
					return false
				}
				lim, ok := c.Resources.Limits[corev1.ResourceCPU]
				return ok && lim.MilliValue() == 1000
			}, time.Minute*5, time.Second).Should(BeTrue())

			By("Verifying QoS class remains Burstable")
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      warmSandboxName,
				Namespace: namespace,
			}, pod)).To(Succeed())
			Expect(pod.Status.QOSClass).To(Equal(corev1.PodQOSBurstable))

			By("Verifying status resources reflect the actual resize (if populated by kubelet)")
			if len(pod.Status.ContainerStatuses) > 0 && pod.Status.ContainerStatuses[0].Resources != nil {
				cpuReq, ok := pod.Status.ContainerStatuses[0].Resources.Requests[corev1.ResourceCPU]
				if ok {
					Expect(cpuReq.MilliValue()).To(Equal(int64(1000)))
				}
			}
		})

		It("should fail fast when CPU resize is infeasible (exceeds node capacity)", func() {
			By("Creating a SandboxClaim with an absurdly large CPU that no node can satisfy")
			infeasibleClaim := &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-claim-infeasible-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName:    sandboxSet.Name,
					Replicas:        ptr.To(int32(1)),
					SkipInitRuntime: true,
					ClaimTimeout:    &metav1.Duration{Duration: 30 * time.Second},
					WaitReadyTimeout: &metav1.Duration{
						Duration: 2 * time.Minute,
					},
					InplaceUpdate: &agentsv1alpha1.SandboxClaimInplaceUpdateOptions{
						Resources: &agentsv1alpha1.SandboxClaimInplaceUpdateResourcesOptions{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("999")},
							Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("999")},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, infeasibleClaim)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, infeasibleClaim) }()

			By("Verifying sandbox InplaceUpdate condition reports infeasible resize failure")
			Eventually(func() string {
				sbx := &agentsv1alpha1.Sandbox{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      warmSandboxName,
					Namespace: namespace,
				}, sbx); err != nil {
					return ""
				}
				for _, cond := range sbx.Status.Conditions {
					if cond.Type == string(agentsv1alpha1.SandboxConditionInplaceUpdate) &&
						cond.Reason == agentsv1alpha1.SandboxInplaceUpdateReasonFailed {
						return cond.Message
					}
				}
				return ""
			}, time.Minute*3, time.Second).Should(ContainSubstring("infeasible"))

			By("Verifying pod CPU remains at original value (resize was not applied)")
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      warmSandboxName,
				Namespace: namespace,
			}, pod)).To(Succeed())
			cpuReq := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
			Expect(cpuReq.MilliValue()).To(Equal(int64(999000)),
				"Pod spec should show the requested (infeasible) CPU since the resize API accepted it")
		})

		It("should only resize the main (first) container, not sidecar containers", func() {
			By("Creating a SandboxSet with main + sidecar containers")
			sidecarSet := &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-pool-sidecar-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxSetSpec{
					Replicas: 1,
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "main",
										Image: "nginx:stable-alpine3.23",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												corev1.ResourceCPU:    resource.MustParse("250m"),
												corev1.ResourceMemory: resource.MustParse("128Mi"),
											},
											Limits: corev1.ResourceList{
												corev1.ResourceCPU:    resource.MustParse("500m"),
												corev1.ResourceMemory: resource.MustParse("256Mi"),
											},
										},
									},
									{
										Name:    "sidecar",
										Image:   "busybox:1.36",
										Command: []string{"sleep", "infinity"},
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												corev1.ResourceCPU:    resource.MustParse("100m"),
												corev1.ResourceMemory: resource.MustParse("64Mi"),
											},
											Limits: corev1.ResourceList{
												corev1.ResourceCPU:    resource.MustParse("200m"),
												corev1.ResourceMemory: resource.MustParse("128Mi"),
											},
										},
									},
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, sidecarSet)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, sidecarSet) }()

			By("Waiting for pool to be ready")
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sidecarSet.Name,
					Namespace: sidecarSet.Namespace,
				}, sidecarSet)
				return sidecarSet.Status.AvailableReplicas
			}, time.Minute*2, time.Second).Should(Equal(int32(1)))

			By("Getting warm sandbox name")
			var sidecarWarmName string
			Eventually(func() int {
				sandboxList := &agentsv1alpha1.SandboxList{}
				if err := k8sClient.List(ctx, sandboxList, client.InNamespace(namespace), client.MatchingLabels{
					agentsv1alpha1.LabelSandboxPool: sidecarSet.Name,
				}); err != nil {
					return 0
				}
				if len(sandboxList.Items) == 1 {
					sidecarWarmName = sandboxList.Items[0].Name
				}
				return len(sandboxList.Items)
			}, time.Minute, time.Second).Should(Equal(1))

			By("Creating a claim that resizes CPU to 1000m")
			sidecarClaim := &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-claim-sidecar-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName:    sidecarSet.Name,
					Replicas:        ptr.To(int32(1)),
					SkipInitRuntime: true,
					WaitReadyTimeout: &metav1.Duration{
						Duration: 2 * time.Minute,
					},
					InplaceUpdate: &agentsv1alpha1.SandboxClaimInplaceUpdateOptions{
						Resources: &agentsv1alpha1.SandboxClaimInplaceUpdateResourcesOptions{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1000m")},
							Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1000m")},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, sidecarClaim)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, sidecarClaim) }()

			By("Verifying claim completes")
			Eventually(func() agentsv1alpha1.SandboxClaimPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sidecarClaim.Name,
					Namespace: sidecarClaim.Namespace,
				}, sidecarClaim)
				return sidecarClaim.Status.Phase
			}, time.Minute*3, time.Second).Should(Equal(agentsv1alpha1.SandboxClaimPhaseCompleted))

			By("Verifying main container CPU spec is resized (req=1000m, lim=1000m)")
			Eventually(func() int64 {
				pod := &corev1.Pod{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      sidecarWarmName,
					Namespace: namespace,
				}, pod); err != nil || len(pod.Spec.Containers) < 2 {
					return -1
				}
				cpuReq := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
				return cpuReq.MilliValue()
			}, time.Minute*2, time.Second).Should(Equal(int64(1000)))

			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      sidecarWarmName,
				Namespace: namespace,
			}, pod)).To(Succeed())
			mainSpec := pod.Spec.Containers[0].Resources
			cpuLim := mainSpec.Limits[corev1.ResourceCPU]
			Expect(cpuLim.MilliValue()).To(Equal(int64(1000)))

			By("Verifying main container status resources reflect the resize (if populated by kubelet)")
			if len(pod.Status.ContainerStatuses) > 0 && pod.Status.ContainerStatuses[0].Resources != nil {
				cpuReq, ok := pod.Status.ContainerStatuses[0].Resources.Requests[corev1.ResourceCPU]
				if ok {
					Expect(cpuReq.MilliValue()).To(Equal(int64(1000)))
				}
			}

			By("Verifying sidecar container CPU remains unchanged")
			Expect(pod.Spec.Containers).To(HaveLen(2))
			sidecar := pod.Spec.Containers[1]
			Expect(sidecar.Name).To(Equal("sidecar"))
			Expect(sidecar.Resources.Requests[corev1.ResourceCPU]).To(Equal(resource.MustParse("100m")))
			Expect(sidecar.Resources.Limits[corev1.ResourceCPU]).To(Equal(resource.MustParse("200m")))
		})

		It("should ignore memory in resize and only apply CPU", func() {
			By("Creating a SandboxClaim with both CPU and memory in requests/limits")
			memIgnoreClaim := &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-claim-mem-ignore-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName:    sandboxSet.Name,
					Replicas:        ptr.To(int32(1)),
					SkipInitRuntime: true,
					WaitReadyTimeout: &metav1.Duration{
						Duration: 2 * time.Minute,
					},
					InplaceUpdate: &agentsv1alpha1.SandboxClaimInplaceUpdateOptions{
						Resources: &agentsv1alpha1.SandboxClaimInplaceUpdateResourcesOptions{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("400m"),
								corev1.ResourceMemory: resource.MustParse("999Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("400m"),
								corev1.ResourceMemory: resource.MustParse("999Mi"),
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, memIgnoreClaim)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, memIgnoreClaim) }()

			By("Verifying claim completes successfully")
			Eventually(func() agentsv1alpha1.SandboxClaimPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      memIgnoreClaim.Name,
					Namespace: memIgnoreClaim.Namespace,
				}, memIgnoreClaim)
				return memIgnoreClaim.Status.Phase
			}, time.Minute*3, time.Second).Should(Equal(agentsv1alpha1.SandboxClaimPhaseCompleted))

			By("Verifying CPU was resized to 400m")
			claimedSandboxes, err := listClaimedSandboxes(ctx, memIgnoreClaim)
			Expect(err).NotTo(HaveOccurred())
			Expect(claimedSandboxes).To(HaveLen(1))

			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      claimedSandboxes[0].Name,
				Namespace: namespace,
			}, pod)).To(Succeed())

			cpuReq := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
			Expect(cpuReq.MilliValue()).To(Equal(int64(400)))
			cpuLim := pod.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]
			Expect(cpuLim.MilliValue()).To(Equal(int64(400)))

			By("Verifying memory remains unchanged (original values, not 999Mi)")
			memReq := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory]
			Expect(memReq.String()).To(Equal("128Mi"))
			memLim := pod.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]
			Expect(memLim.String()).To(Equal("256Mi"))

			By("Verifying status resources reflect the actual CPU resize (if populated by kubelet)")
			if len(pod.Status.ContainerStatuses) > 0 && pod.Status.ContainerStatuses[0].Resources != nil {
				cpuReq, ok := pod.Status.ContainerStatuses[0].Resources.Requests[corev1.ResourceCPU]
				if ok {
					Expect(cpuReq.MilliValue()).To(Equal(int64(400)))
				}
			}
		})

	})

	Context("Replicas immutability (webhook validation)", func() {
		var sandboxClaim *agentsv1alpha1.SandboxClaim

		BeforeEach(func() {
			sandboxClaim = &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-claim-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName:    "test-pool",
					Replicas:        ptr.To(int32(1)),
					SkipInitRuntime: true,
				},
			}
		})

		AfterEach(func() {
			if sandboxClaim != nil {
				_ = k8sClient.Delete(ctx, sandboxClaim)
			}
		})

		It("should reject updates to replicas field", func() {
			By("Creating a SandboxClaim")
			Expect(k8sClient.Create(ctx, sandboxClaim)).To(Succeed())

			By("Waiting for controller to reconcile")
			time.Sleep(time.Second)

			By("Attempting to update replicas field")
			// Get the latest version to avoid ResourceVersion conflict
			claim := &agentsv1alpha1.SandboxClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      sandboxClaim.Name,
				Namespace: sandboxClaim.Namespace,
			}, claim)).To(Succeed())

			claim.Spec.Replicas = ptr.To(int32(5))

			err := k8sClient.Update(ctx, claim)

			By("Verifying the update is rejected by webhook")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("replicas is immutable"))
		})

	})

	Context("Timeout handling", func() {
		var (
			sandboxSet   *agentsv1alpha1.SandboxSet
			sandboxClaim *agentsv1alpha1.SandboxClaim
		)

		BeforeEach(func() {
			// Create a SandboxSet with only 2 replicas
			sandboxSet = &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-pool-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxSetSpec{
					Replicas: 2,
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "test-container",
										Image: "nginx:stable-alpine3.23",
									},
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, sandboxSet)).To(Succeed())

			// Wait for SandboxSet to be ready
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxSet.Name,
					Namespace: sandboxSet.Namespace,
				}, sandboxSet)
				return sandboxSet.Status.AvailableReplicas
			}, time.Minute*2, time.Second).Should(Equal(int32(2)))
		})

		AfterEach(func() {
			if sandboxClaim != nil {
				_ = k8sClient.Delete(ctx, sandboxClaim)
			}
			if sandboxSet != nil {
				_ = k8sClient.Delete(ctx, sandboxSet)
			}
		})

		It("should eventually succeed when SandboxSet auto-replenishes", func() {
			By("Creating a SandboxClaim requesting more than initially available")
			sandboxClaim = &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-claim-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName:    sandboxSet.Name,
					Replicas:        ptr.To(int32(5)),                            // Request more than initially available (2)
					ClaimTimeout:    &metav1.Duration{Duration: 2 * time.Minute}, // Give enough time
					SkipInitRuntime: true,
				},
			}
			Expect(k8sClient.Create(ctx, sandboxClaim)).To(Succeed())

			By("Verifying the claim eventually succeeds as SandboxSet replenishes")
			Eventually(func() agentsv1alpha1.SandboxClaimPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxClaim.Name,
					Namespace: sandboxClaim.Namespace,
				}, sandboxClaim)
				return sandboxClaim.Status.Phase
			}, time.Minute*3, time.Second).Should(Equal(agentsv1alpha1.SandboxClaimPhaseCompleted))

			By("Verifying all requested replicas are claimed")
			Expect(sandboxClaim.Status.ClaimedReplicas).To(Equal(int32(5)))

			By("Verifying the status message indicates success")
			Expect(sandboxClaim.Status.Message).To(ContainSubstring("Successfully claimed"))
		})
	})

	Context("SandboxSet not found", func() {
		var sandboxClaim *agentsv1alpha1.SandboxClaim

		AfterEach(func() {
			if sandboxClaim != nil {
				_ = k8sClient.Delete(ctx, sandboxClaim)
			}
		})

		It("should show appropriate status when SandboxSet does not exist", func() {
			By("Creating a SandboxClaim referencing non-existent SandboxSet")
			sandboxClaim = &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-claim-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName:    "non-existent-pool",
					Replicas:        ptr.To(int32(1)),
					SkipInitRuntime: true,
				},
			}
			Expect(k8sClient.Create(ctx, sandboxClaim)).To(Succeed())

			By("Verifying claimedReplicas is 0 (explicitly shown)")
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxClaim.Name,
					Namespace: sandboxClaim.Namespace,
				}, sandboxClaim)
				return sandboxClaim.Status.ClaimedReplicas
			}, time.Second*10, time.Millisecond*500).Should(Equal(int32(0)))

			By("Verifying the status indicates SandboxSet not found")
			Eventually(func() string {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxClaim.Name,
					Namespace: sandboxClaim.Namespace,
				}, sandboxClaim)
				return sandboxClaim.Status.Message
			}, time.Second*10, time.Millisecond*500).Should(ContainSubstring("not found"))
		})
	})

	Context("No available sandboxes", func() {
		var (
			sandboxSet    *agentsv1alpha1.SandboxSet
			sandboxClaim1 *agentsv1alpha1.SandboxClaim
			sandboxClaim2 *agentsv1alpha1.SandboxClaim
		)

		BeforeEach(func() {
			// Create a SandboxSet with 2 replicas
			sandboxSet = &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-pool-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxSetSpec{
					Replicas: 2,
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "test-container",
										Image: "nginx:stable-alpine3.23",
									},
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, sandboxSet)).To(Succeed())

			// Wait for SandboxSet to be ready
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxSet.Name,
					Namespace: sandboxSet.Namespace,
				}, sandboxSet)
				return sandboxSet.Status.AvailableReplicas
			}, time.Minute*2, time.Second).Should(Equal(int32(2)))

			// Claim all available sandboxes
			sandboxClaim1 = &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-claim-1-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName:    sandboxSet.Name,
					Replicas:        ptr.To(int32(2)),
					SkipInitRuntime: true,
				},
			}
			Expect(k8sClient.Create(ctx, sandboxClaim1)).To(Succeed())

			// Wait for all sandboxes to be claimed
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxClaim1.Name,
					Namespace: sandboxClaim1.Namespace,
				}, sandboxClaim1)
				return sandboxClaim1.Status.ClaimedReplicas
			}, time.Minute, time.Second).Should(Equal(int32(2)))
		})

		AfterEach(func() {
			if sandboxClaim2 != nil {
				_ = k8sClient.Delete(ctx, sandboxClaim2)
			}
			if sandboxClaim1 != nil {
				_ = k8sClient.Delete(ctx, sandboxClaim1)
			}
			if sandboxSet != nil {
				_ = k8sClient.Delete(ctx, sandboxSet)
			}
		})

		It("should handle the case when no sandboxes are available", func() {
			By("Scaling down SandboxSet to 0 to prevent new sandboxes from being created")
			// Retry update to handle concurrent modifications by controllers
			Eventually(func() error {
				latest := &agentsv1alpha1.SandboxSet{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxSet.Name,
					Namespace: sandboxSet.Namespace,
				}, latest); err != nil {
					return err
				}
				// Scale to 0 so no new sandboxes are created
				latest.Spec.Replicas = 0
				return k8sClient.Update(ctx, latest)
			}, time.Second*10, time.Second).Should(Succeed())

			By("Waiting for SandboxSet to stop creating new sandboxes")
			time.Sleep(time.Second * 2)

			By("Creating another claim when all sandboxes are already claimed")
			sandboxClaim2 = &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-claim-2-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName:    sandboxSet.Name,
					Replicas:        ptr.To(int32(1)),
					ClaimTimeout:    &metav1.Duration{Duration: 10 * time.Second},
					SkipInitRuntime: true,
				},
			}
			Expect(k8sClient.Create(ctx, sandboxClaim2)).To(Succeed())

			By("Verifying claimedReplicas remains 0")
			Consistently(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxClaim2.Name,
					Namespace: sandboxClaim2.Namespace,
				}, sandboxClaim2)
				return sandboxClaim2.Status.ClaimedReplicas
			}, time.Second*5, time.Second).Should(Equal(int32(0)))

			By("Verifying the status message indicates no available sandboxes or timeout")
			Eventually(func() string {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxClaim2.Name,
					Namespace: sandboxClaim2.Namespace,
				}, sandboxClaim2)
				return sandboxClaim2.Status.Message
			}, time.Second*15, time.Second).Should(Or(
				ContainSubstring("available"),
				ContainSubstring("timeout"),
				ContainSubstring("claimed 0/1"),
			))
		})
	})

	Context("Default replicas value", func() {
		var (
			sandboxSet   *agentsv1alpha1.SandboxSet
			sandboxClaim *agentsv1alpha1.SandboxClaim
		)

		BeforeEach(func() {
			sandboxSet = &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-pool-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxSetSpec{
					Replicas: 3,
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "test-container",
										Image: "nginx:stable-alpine3.23",
									},
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, sandboxSet)).To(Succeed())

			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxSet.Name,
					Namespace: sandboxSet.Namespace,
				}, sandboxSet)
				return sandboxSet.Status.AvailableReplicas
			}, time.Minute*2, time.Second).Should(Equal(int32(3)))
		})

		AfterEach(func() {
			if sandboxClaim != nil {
				_ = k8sClient.Delete(ctx, sandboxClaim)
			}
			if sandboxSet != nil {
				_ = k8sClient.Delete(ctx, sandboxSet)
			}
		})

		It("should use default replicas=1 when not specified", func() {
			By("Creating a SandboxClaim without specifying replicas")
			sandboxClaim = &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-claim-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName:    sandboxSet.Name,
					SkipInitRuntime: true,
					// Replicas not specified, should default to 1
				},
			}
			Expect(k8sClient.Create(ctx, sandboxClaim)).To(Succeed())

			By("Verifying the claim completes with 1 replica")
			Eventually(func() agentsv1alpha1.SandboxClaimPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxClaim.Name,
					Namespace: sandboxClaim.Namespace,
				}, sandboxClaim)
				return sandboxClaim.Status.Phase
			}, time.Minute, time.Second).Should(Equal(agentsv1alpha1.SandboxClaimPhaseCompleted))

			By("Verifying claimedReplicas equals 1")
			Expect(sandboxClaim.Status.ClaimedReplicas).To(Equal(int32(1)))
		})
	})

	Context("Cleanup and deletion", func() {
		var (
			sandboxSet   *agentsv1alpha1.SandboxSet
			sandboxClaim *agentsv1alpha1.SandboxClaim
		)

		BeforeEach(func() {
			sandboxSet = &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-pool-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxSetSpec{
					Replicas: 3,
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "test-container",
										Image: "nginx:stable-alpine3.23",
									},
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, sandboxSet)).To(Succeed())

			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxSet.Name,
					Namespace: sandboxSet.Namespace,
				}, sandboxSet)
				return sandboxSet.Status.AvailableReplicas
			}, time.Minute*2, time.Second).Should(Equal(int32(3)))
		})

		AfterEach(func() {
			if sandboxSet != nil {
				_ = k8sClient.Delete(ctx, sandboxSet)
			}
		})

		It("should not delete sandboxes when claim is deleted", func() {
			By("Creating and completing a claim")
			sandboxClaim = &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-claim-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName:    sandboxSet.Name,
					Replicas:        ptr.To(int32(2)),
					SkipInitRuntime: true,
				},
			}
			Expect(k8sClient.Create(ctx, sandboxClaim)).To(Succeed())

			Eventually(func() agentsv1alpha1.SandboxClaimPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxClaim.Name,
					Namespace: sandboxClaim.Namespace,
				}, sandboxClaim)
				return sandboxClaim.Status.Phase
			}, time.Minute, time.Second).Should(Equal(agentsv1alpha1.SandboxClaimPhaseCompleted))

			By("Counting sandboxes claimed by this claim")
			claimedSandboxes, err := listClaimedSandboxes(ctx, sandboxClaim)
			Expect(err).NotTo(HaveOccurred())
			Expect(claimedSandboxes).To(HaveLen(2))

			By("Deleting the claim")
			Expect(k8sClient.Delete(ctx, sandboxClaim)).To(Succeed())

			By("Verifying the claim is deleted")
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxClaim.Name,
					Namespace: sandboxClaim.Namespace,
				}, sandboxClaim)
				return apierrors.IsNotFound(err)
			}, time.Second*30, time.Second).Should(BeTrue())

			By("Verifying sandboxes still exist (not cascade deleted)")
			// Sandboxes should NOT be deleted when claim is deleted
			// We use annotations instead of ownerReferences for tracking
			sandboxList := &agentsv1alpha1.SandboxList{}
			Expect(k8sClient.List(ctx, sandboxList, client.InNamespace(namespace))).To(Succeed())

			stillExistCount := 0
			claimUID := string(sandboxClaim.UID)
			for _, sandbox := range sandboxList.Items {
				// Check if sandbox was claimed by this claim (annotation may still exist)
				if sandbox.Annotations != nil && sandbox.Annotations[agentsv1alpha1.AnnotationOwner] == claimUID {
					stillExistCount++
				}
			}
			Expect(stillExistCount).To(Equal(2), "Claimed sandboxes should still exist after claim deletion")
		})
	})

	Context("TTL cleanup", func() {
		var (
			sandboxSet   *agentsv1alpha1.SandboxSet
			sandboxClaim *agentsv1alpha1.SandboxClaim
		)

		BeforeEach(func() {
			By("Creating a SandboxSet for TTL test")
			sandboxSet = &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-sandboxset-ttl-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxSetSpec{
					Replicas: 3,
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{
									"sandboxset": "ttl-test",
								},
							},
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "test-container",
										Image: "nginx:stable-alpine3.23",
									},
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, sandboxSet)).To(Succeed())

			// Wait for sandboxes to be available
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxSet.Name,
					Namespace: sandboxSet.Namespace,
				}, sandboxSet)
				return sandboxSet.Status.AvailableReplicas
			}, time.Minute*2, time.Second).Should(Equal(int32(3)))
		})

		AfterEach(func() {
			// Clean up SandboxSet (claim should be auto-deleted by TTL)
			if sandboxSet != nil {
				_ = k8sClient.Delete(ctx, sandboxSet)
			}
		})

		It("should auto-delete claim after TTL expires", func() {
			By("Creating a SandboxClaim with short TTL")
			sandboxClaim = &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-claim-ttl-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName:      sandboxSet.Name,
					Replicas:          ptr.To(int32(2)),
					TTLAfterCompleted: &metav1.Duration{Duration: 30 * time.Second},
					SkipInitRuntime:   true,
				},
			}
			Expect(k8sClient.Create(ctx, sandboxClaim)).To(Succeed())

			By("Waiting for claim to complete")
			Eventually(func() agentsv1alpha1.SandboxClaimPhase {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxClaim.Name,
					Namespace: sandboxClaim.Namespace,
				}, sandboxClaim)
				if err != nil {
					return ""
				}
				return sandboxClaim.Status.Phase
			}, time.Minute, time.Second).Should(Equal(agentsv1alpha1.SandboxClaimPhaseCompleted))

			By("Recording completion time")
			var completionTime time.Time
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      sandboxClaim.Name,
				Namespace: sandboxClaim.Namespace,
			}, sandboxClaim)).To(Succeed())
			Expect(sandboxClaim.Status.CompletionTime).NotTo(BeNil())
			completionTime = sandboxClaim.Status.CompletionTime.Time

			By("Waiting for TTL to expire and claim to be deleted")
			expectedDeletionTime := completionTime.Add(30 * time.Second)
			waitUntil := time.Until(expectedDeletionTime)
			if waitUntil < 0 {
				waitUntil = 0
			}

			// Wait for deletion with buffer
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxClaim.Name,
					Namespace: sandboxClaim.Namespace,
				}, sandboxClaim)
				return apierrors.IsNotFound(err)
			}, waitUntil+10*time.Second, time.Second*2).Should(BeTrue())

		})

		It("should not delete claim if TTL is not set", func() {
			By("Creating a SandboxClaim without TTL")
			sandboxClaim = &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-claim-no-ttl-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName:    sandboxSet.Name,
					Replicas:        ptr.To(int32(1)),
					SkipInitRuntime: true,
					// No TTLAfterCompleted specified
				},
			}
			Expect(k8sClient.Create(ctx, sandboxClaim)).To(Succeed())

			By("Waiting for claim to complete")
			Eventually(func() agentsv1alpha1.SandboxClaimPhase {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxClaim.Name,
					Namespace: sandboxClaim.Namespace,
				}, sandboxClaim)
				if err != nil {
					return ""
				}
				return sandboxClaim.Status.Phase
			}, time.Minute, time.Second).Should(Equal(agentsv1alpha1.SandboxClaimPhaseCompleted))

			By("Verifying claim is not deleted after a long time")
			Consistently(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxClaim.Name,
					Namespace: sandboxClaim.Namespace,
				}, sandboxClaim)
			}, time.Second*40, time.Second*5).Should(Succeed())

			By("Manually deleting the claim")
			Expect(k8sClient.Delete(ctx, sandboxClaim)).To(Succeed())
		})

		It("should handle very short TTL correctly", func() {
			By("Creating a SandboxClaim with very short TTL (5s)")
			sandboxClaim = &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-claim-short-ttl-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName:      sandboxSet.Name,
					Replicas:          ptr.To(int32(1)),
					TTLAfterCompleted: &metav1.Duration{Duration: 5 * time.Second},
					SkipInitRuntime:   true,
				},
			}
			Expect(k8sClient.Create(ctx, sandboxClaim)).To(Succeed())

			By("Waiting for claim to complete")
			Eventually(func() agentsv1alpha1.SandboxClaimPhase {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxClaim.Name,
					Namespace: sandboxClaim.Namespace,
				}, sandboxClaim)
				if err != nil {
					return ""
				}
				return sandboxClaim.Status.Phase
			}, time.Minute, time.Second).Should(Equal(agentsv1alpha1.SandboxClaimPhaseCompleted))

			By("Verifying claim is deleted quickly after completion")
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxClaim.Name,
					Namespace: sandboxClaim.Namespace,
				}, sandboxClaim)
				return apierrors.IsNotFound(err)
			}, time.Second*15, time.Second).Should(BeTrue())
		})
	})

	Context("Recovery logic", func() {
		var (
			sandboxSet   *agentsv1alpha1.SandboxSet
			sandboxClaim *agentsv1alpha1.SandboxClaim
		)

		BeforeEach(func() {
			// Create a SandboxSet with 10 replicas
			sandboxSet = &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-recovery-pool-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxSetSpec{
					Replicas: 10,
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "test-container",
										Image: "nginx:stable-alpine3.23",
									},
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, sandboxSet)).To(Succeed())

			// Wait for SandboxSet to be ready
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxSet.Name,
					Namespace: sandboxSet.Namespace,
				}, sandboxSet)
				return sandboxSet.Status.AvailableReplicas
			}, time.Minute*2, time.Second).Should(Equal(int32(10)))
		})

		AfterEach(func() {
			if sandboxClaim != nil {
				_ = k8sClient.Delete(ctx, sandboxClaim)
			}
			if sandboxSet != nil {
				_ = k8sClient.Delete(ctx, sandboxSet)
			}
		})

		It("should recover when status count is less than actual claimed count", func() {
			By("Creating a SandboxClaim with replicas=3")
			sandboxClaim = &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-claim-recovery-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName:    sandboxSet.Name,
					Replicas:        ptr.To(int32(3)),
					SkipInitRuntime: true,
				},
			}
			Expect(k8sClient.Create(ctx, sandboxClaim)).To(Succeed())

			By("Waiting for claim to complete normally")
			Eventually(func() agentsv1alpha1.SandboxClaimPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxClaim.Name,
					Namespace: sandboxClaim.Namespace,
				}, sandboxClaim)
				return sandboxClaim.Status.Phase
			}, time.Minute, time.Second).Should(Equal(agentsv1alpha1.SandboxClaimPhaseCompleted))

			By("Verifying 3 sandboxes are claimed")
			Expect(sandboxClaim.Status.ClaimedReplicas).To(Equal(int32(3)))

			By("Simulating status loss - manually setting ClaimedReplicas to 1 (less than actual 3)")
			// This simulates controller crash after claiming but before status update
			Eventually(func() error {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxClaim.Name,
					Namespace: sandboxClaim.Namespace,
				}, sandboxClaim)
				if err != nil {
					return err
				}

				// Manually reduce the count to simulate status loss
				sandboxClaim.Status.ClaimedReplicas = 1
				sandboxClaim.Status.Phase = agentsv1alpha1.SandboxClaimPhaseClaiming
				sandboxClaim.Status.Message = "Simulating status loss"

				return k8sClient.Status().Update(ctx, sandboxClaim)
			}, time.Second*10, time.Second).Should(Succeed())

			By("Verifying controller recovers the correct count without claiming more")
			// The controller should detect actualCount=3 > statusCount=1
			// and recover to 3 without claiming additional sandboxes
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxClaim.Name,
					Namespace: sandboxClaim.Namespace,
				}, sandboxClaim)
				return sandboxClaim.Status.ClaimedReplicas
			}, time.Second*30, time.Second).Should(Equal(int32(3)))

			By("Verifying no duplicate claiming occurred")
			claimedSandboxes, err := listClaimedSandboxes(ctx, sandboxClaim)
			Expect(err).NotTo(HaveOccurred())
			Expect(claimedSandboxes).To(HaveLen(3), "Should still have exactly 3 claimed sandboxes")

			By("Verifying claim transitions back to Completed")
			Eventually(func() agentsv1alpha1.SandboxClaimPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxClaim.Name,
					Namespace: sandboxClaim.Namespace,
				}, sandboxClaim)
				return sandboxClaim.Status.Phase
			}, time.Second*30, time.Second).Should(Equal(agentsv1alpha1.SandboxClaimPhaseCompleted))
		})

		It("should maintain Job semantics when sandbox is manually deleted", func() {
			By("Creating a SandboxClaim with replicas=3")
			sandboxClaim = &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-claim-job-semantics-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName:    sandboxSet.Name,
					Replicas:        ptr.To(int32(3)),
					SkipInitRuntime: true,
				},
			}
			Expect(k8sClient.Create(ctx, sandboxClaim)).To(Succeed())

			By("Waiting for claim to complete")
			Eventually(func() agentsv1alpha1.SandboxClaimPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxClaim.Name,
					Namespace: sandboxClaim.Namespace,
				}, sandboxClaim)
				return sandboxClaim.Status.Phase
			}, time.Minute, time.Second).Should(Equal(agentsv1alpha1.SandboxClaimPhaseCompleted))

			By("Getting the list of claimed sandboxes")
			claimedSandboxes, err := listClaimedSandboxes(ctx, sandboxClaim)
			Expect(err).NotTo(HaveOccurred())
			Expect(claimedSandboxes).To(HaveLen(3))

			By("Manually deleting one claimed sandbox (simulating user action)")
			deletedSandbox := &claimedSandboxes[0]
			Expect(k8sClient.Delete(ctx, deletedSandbox)).To(Succeed())

			By("Waiting for sandbox to be deleted")
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      deletedSandbox.Name,
					Namespace: deletedSandbox.Namespace,
				}, &agentsv1alpha1.Sandbox{})
				return apierrors.IsNotFound(err)
			}, time.Second*30, time.Second).Should(BeTrue())

			By("Triggering reconciliation by updating an annotation")
			Eventually(func() error {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxClaim.Name,
					Namespace: sandboxClaim.Namespace,
				}, sandboxClaim)
				if err != nil {
					return err
				}
				if sandboxClaim.Annotations == nil {
					sandboxClaim.Annotations = make(map[string]string)
				}
				sandboxClaim.Annotations["test-trigger"] = time.Now().String()
				return k8sClient.Update(ctx, sandboxClaim)
			}, time.Second*10, time.Second).Should(Succeed())

			By("Verifying ClaimedReplicas remains 3 (Job semantics - no backfill)")
			// Job semantics: once claimed, always counted, even if deleted
			Consistently(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxClaim.Name,
					Namespace: sandboxClaim.Namespace,
				}, sandboxClaim)
				return sandboxClaim.Status.ClaimedReplicas
			}, time.Second*20, time.Second*2).Should(Equal(int32(3)))

			By("Verifying only 2 sandboxes exist (one was deleted)")
			claimedSandboxes, err = listClaimedSandboxes(ctx, sandboxClaim)
			Expect(err).NotTo(HaveOccurred())
			Expect(claimedSandboxes).To(HaveLen(2), "Should have 2 sandboxes after manual deletion")

			By("Verifying claim remains Completed (no attempt to claim more)")
			Expect(sandboxClaim.Status.Phase).To(Equal(agentsv1alpha1.SandboxClaimPhaseCompleted))
		})

		It("should handle complete status loss scenario", func() {
			By("Creating a SandboxClaim with replicas=5")
			sandboxClaim = &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-claim-complete-loss-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName:    sandboxSet.Name,
					Replicas:        ptr.To(int32(5)),
					SkipInitRuntime: true,
				},
			}
			Expect(k8sClient.Create(ctx, sandboxClaim)).To(Succeed())

			By("Waiting for some sandboxes to be claimed (at least 2)")
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxClaim.Name,
					Namespace: sandboxClaim.Namespace,
				}, sandboxClaim)
				return sandboxClaim.Status.ClaimedReplicas
			}, time.Second*30, time.Second).Should(BeNumerically(">=", 2))

			By("Recording actual claimed count before status reset")
			var actualClaimedBeforeReset int32
			claimedSandboxes, err := listClaimedSandboxes(ctx, sandboxClaim)
			Expect(err).NotTo(HaveOccurred())
			actualClaimedBeforeReset = int32(len(claimedSandboxes))

			By("Simulating complete status loss - resetting ClaimedReplicas to 0")
			Eventually(func() error {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxClaim.Name,
					Namespace: sandboxClaim.Namespace,
				}, sandboxClaim)
				if err != nil {
					return err
				}

				// Reset status to simulate complete loss
				sandboxClaim.Status.ClaimedReplicas = 0
				sandboxClaim.Status.Phase = agentsv1alpha1.SandboxClaimPhaseClaiming
				sandboxClaim.Status.Message = "Simulating complete status loss"

				return k8sClient.Status().Update(ctx, sandboxClaim)
			}, time.Second*10, time.Second).Should(Succeed())

			By("Verifying controller recovers the count from actual sandboxes")
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxClaim.Name,
					Namespace: sandboxClaim.Namespace,
				}, sandboxClaim)
				return sandboxClaim.Status.ClaimedReplicas
			}, time.Second*30, time.Second).Should(Equal(actualClaimedBeforeReset))

			By("Waiting for claim to eventually complete with all 5 replicas")
			Eventually(func() bool {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name:      sandboxClaim.Name,
					Namespace: sandboxClaim.Namespace,
				}, sandboxClaim)
				return sandboxClaim.Status.Phase == agentsv1alpha1.SandboxClaimPhaseCompleted &&
					sandboxClaim.Status.ClaimedReplicas == 5
			}, time.Minute, time.Second).Should(BeTrue())

			By("Verifying exactly 5 sandboxes were claimed (no duplicates)")
			claimedSandboxes, err = listClaimedSandboxes(ctx, sandboxClaim)
			Expect(err).NotTo(HaveOccurred())
			Expect(claimedSandboxes).To(HaveLen(5), "Should have exactly 5 claimed sandboxes")
		})

		Context("Labels and Annotations", func() {
			var sandboxSet *agentsv1alpha1.SandboxSet
			var sandboxClaim *agentsv1alpha1.SandboxClaim

			BeforeEach(func() {
				By("Creating a SandboxSet for labels/annotations tests")
				sandboxSet = &agentsv1alpha1.SandboxSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("test-pool-%d", time.Now().UnixNano()),
						Namespace: namespace,
					},
					Spec: agentsv1alpha1.SandboxSetSpec{
						Replicas: 5,
						EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
							Template: &corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{
											Name:  "test-container",
											Image: "nginx:stable-alpine3.23",
										},
									},
								},
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, sandboxSet)).To(Succeed())

				// Wait for sandboxes to be ready
				Eventually(func() int32 {
					err := k8sClient.Get(ctx, types.NamespacedName{
						Name:      sandboxSet.Name,
						Namespace: sandboxSet.Namespace,
					}, sandboxSet)
					if err != nil {
						return 0
					}
					return sandboxSet.Status.AvailableReplicas
				}, time.Minute, time.Second).Should(BeNumerically(">=", 3))
			})

			AfterEach(func() {
				if sandboxClaim != nil {
					_ = k8sClient.Delete(ctx, sandboxClaim)
				}
				if sandboxSet != nil {
					_ = k8sClient.Delete(ctx, sandboxSet)
				}
			})

			It("should propagate custom labels to claimed sandboxes", func() {
				By("Creating a SandboxClaim with custom labels")
				sandboxClaim = &agentsv1alpha1.SandboxClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("test-claim-labels-%d", time.Now().UnixNano()),
						Namespace: namespace,
					},
					Spec: agentsv1alpha1.SandboxClaimSpec{
						TemplateName: sandboxSet.Name,
						Replicas:     ptr.To(int32(2)),
						Labels: map[string]string{
							"custom-label-1": "value1",
							"custom-label-2": "value2",
							"team":           "platform",
							"env":            "test",
						},
						SkipInitRuntime: true,
					},
				}
				Expect(k8sClient.Create(ctx, sandboxClaim)).To(Succeed())

				By("Waiting for claim to complete")
				Eventually(func() agentsv1alpha1.SandboxClaimPhase {
					err := k8sClient.Get(ctx, types.NamespacedName{
						Name:      sandboxClaim.Name,
						Namespace: sandboxClaim.Namespace,
					}, sandboxClaim)
					if err != nil {
						return ""
					}
					return sandboxClaim.Status.Phase
				}, time.Minute, time.Second).Should(Equal(agentsv1alpha1.SandboxClaimPhaseCompleted))

				By("Verifying custom labels are set on claimed sandboxes")
				claimedSandboxes, err := listClaimedSandboxes(ctx, sandboxClaim)
				Expect(err).NotTo(HaveOccurred())
				Expect(claimedSandboxes).To(HaveLen(2), "Should have 2 claimed sandboxes")

				for _, sandbox := range claimedSandboxes {
					// Verify system annotation (UID for uniqueness)
					Expect(sandbox.Annotations).To(HaveKeyWithValue(
						agentsv1alpha1.AnnotationOwner,
						string(sandboxClaim.UID),
					), "System annotation (UID) should be set")

					// Verify system label (Name for readability/display)
					Expect(sandbox.Labels).To(HaveKeyWithValue(
						agentsv1alpha1.LabelSandboxClaimName,
						sandboxClaim.Name,
					), "System label (Name) should be set")

					// Verify all custom labels
					Expect(sandbox.Labels).To(HaveKeyWithValue("custom-label-1", "value1"))
					Expect(sandbox.Labels).To(HaveKeyWithValue("custom-label-2", "value2"))
					Expect(sandbox.Labels).To(HaveKeyWithValue("team", "platform"))
					Expect(sandbox.Labels).To(HaveKeyWithValue("env", "test"))

					Expect(sandbox.Spec.Template.Labels).To(HaveKeyWithValue("custom-label-1", "value1"))
					Expect(sandbox.Spec.Template.Labels).To(HaveKeyWithValue("custom-label-2", "value2"))
					Expect(sandbox.Spec.Template.Labels).To(HaveKeyWithValue("team", "platform"))
					Expect(sandbox.Spec.Template.Labels).To(HaveKeyWithValue("env", "test"))

					// Verify custom labels are also propagated to the backing Pod
					By(fmt.Sprintf("Verifying pod %s has the same custom labels", sandbox.Name))
					Eventually(func() error {
						pod := &corev1.Pod{}
						err := k8sClient.Get(ctx, types.NamespacedName{
							Name:      sandbox.Name,
							Namespace: sandbox.Namespace,
						}, pod)
						if err != nil {
							return err
						}

						// Verify all custom labels are present on the pod
						if pod.Labels == nil {
							return fmt.Errorf("pod labels are nil")
						}
						if pod.Labels["custom-label-1"] != "value1" {
							return fmt.Errorf("pod missing custom-label-1, got: %v", pod.Labels)
						}
						if pod.Labels["custom-label-2"] != "value2" {
							return fmt.Errorf("pod missing custom-label-2, got: %v", pod.Labels)
						}
						if pod.Labels["team"] != "platform" {
							return fmt.Errorf("pod missing team label, got: %v", pod.Labels)
						}
						if pod.Labels["env"] != "test" {
							return fmt.Errorf("pod missing env label, got: %v", pod.Labels)
						}
						return nil
					}, time.Minute, time.Second).Should(Succeed(),
						"Pod %s should have all custom labels from SandboxClaim within 1 minute", sandbox.Name)
				}
			})

			It("should propagate custom annotations to claimed sandboxes", func() {
				By("Creating a SandboxClaim with custom annotations")
				sandboxClaim = &agentsv1alpha1.SandboxClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("test-claim-annotations-%d", time.Now().UnixNano()),
						Namespace: namespace,
					},
					Spec: agentsv1alpha1.SandboxClaimSpec{
						TemplateName: sandboxSet.Name,
						Replicas:     ptr.To(int32(2)),
						Annotations: map[string]string{
							"custom-annotation-1": "annotation-value1",
							"custom-annotation-2": "annotation-value2",
							"owner":               "platform-team",
							"description":         "Test sandbox for E2E testing",
						},
						SkipInitRuntime: true,
					},
				}
				Expect(k8sClient.Create(ctx, sandboxClaim)).To(Succeed())

				By("Waiting for claim to complete")
				Eventually(func() agentsv1alpha1.SandboxClaimPhase {
					err := k8sClient.Get(ctx, types.NamespacedName{
						Name:      sandboxClaim.Name,
						Namespace: sandboxClaim.Namespace,
					}, sandboxClaim)
					if err != nil {
						return ""
					}
					return sandboxClaim.Status.Phase
				}, time.Minute, time.Second).Should(Equal(agentsv1alpha1.SandboxClaimPhaseCompleted))

				By("Verifying custom annotations are set on claimed sandboxes")
				claimedSandboxes, err := listClaimedSandboxes(ctx, sandboxClaim)
				Expect(err).NotTo(HaveOccurred())
				Expect(claimedSandboxes).To(HaveLen(2), "Should have 2 claimed sandboxes")

				for _, sandbox := range claimedSandboxes {
					// Verify all custom annotations
					Expect(sandbox.Annotations).To(HaveKeyWithValue("custom-annotation-1", "annotation-value1"))
					Expect(sandbox.Annotations).To(HaveKeyWithValue("custom-annotation-2", "annotation-value2"))
					Expect(sandbox.Annotations).To(HaveKeyWithValue("owner", "platform-team"))
					Expect(sandbox.Annotations).To(HaveKeyWithValue("description", "Test sandbox for E2E testing"))
				}
			})

			It("should set both custom labels and annotations together", func() {
				By("Creating a SandboxClaim with both labels and annotations")
				sandboxClaim = &agentsv1alpha1.SandboxClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("test-claim-both-%d", time.Now().UnixNano()),
						Namespace: namespace,
					},
					Spec: agentsv1alpha1.SandboxClaimSpec{
						TemplateName: sandboxSet.Name,
						Replicas:     ptr.To(int32(1)),
						Labels: map[string]string{
							"app":     "my-app",
							"version": "v1.0.0",
						},
						Annotations: map[string]string{
							"build-id": "12345",
							"git-sha":  "abc123",
						},
						SkipInitRuntime: true,
					},
				}
				Expect(k8sClient.Create(ctx, sandboxClaim)).To(Succeed())

				By("Waiting for claim to complete")
				Eventually(func() agentsv1alpha1.SandboxClaimPhase {
					err := k8sClient.Get(ctx, types.NamespacedName{
						Name:      sandboxClaim.Name,
						Namespace: sandboxClaim.Namespace,
					}, sandboxClaim)
					if err != nil {
						return ""
					}
					return sandboxClaim.Status.Phase
				}, time.Minute, time.Second).Should(Equal(agentsv1alpha1.SandboxClaimPhaseCompleted))

				By("Verifying both labels and annotations are set")
				claimedSandboxes, err := listClaimedSandboxes(ctx, sandboxClaim)
				Expect(err).NotTo(HaveOccurred())
				Expect(claimedSandboxes).To(HaveLen(1), "Should have 1 claimed sandbox")

				sandbox := claimedSandboxes[0]

				// Verify system annotation (UID for uniqueness)
				Expect(sandbox.Annotations).To(HaveKeyWithValue(
					agentsv1alpha1.AnnotationOwner,
					string(sandboxClaim.UID),
				))
				// Verify system label (Name for display)
				Expect(sandbox.Labels).To(HaveKeyWithValue(
					agentsv1alpha1.LabelSandboxClaimName,
					sandboxClaim.Name,
				))

				// Verify custom labels
				Expect(sandbox.Labels).To(HaveKeyWithValue("app", "my-app"))
				Expect(sandbox.Labels).To(HaveKeyWithValue("version", "v1.0.0"))

				// Verify custom annotations
				Expect(sandbox.Annotations).To(HaveKeyWithValue("build-id", "12345"))
				Expect(sandbox.Annotations).To(HaveKeyWithValue("git-sha", "abc123"))
			})

			It("should not reuse sandboxes from previous claim with same name", func() {
				By("Creating a SandboxClaim and waiting for completion")
				firstClaimName := fmt.Sprintf("test-claim-recreate-%d", time.Now().UnixNano())
				sandboxClaim = &agentsv1alpha1.SandboxClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      firstClaimName,
						Namespace: namespace,
					},
					Spec: agentsv1alpha1.SandboxClaimSpec{
						TemplateName: sandboxSet.Name,
						Replicas:     ptr.To(int32(2)),
						Labels: map[string]string{
							"generation": "first",
						},
						SkipInitRuntime: true,
					},
				}
				Expect(k8sClient.Create(ctx, sandboxClaim)).To(Succeed())

				// Wait for completion
				Eventually(func() agentsv1alpha1.SandboxClaimPhase {
					err := k8sClient.Get(ctx, types.NamespacedName{
						Name:      sandboxClaim.Name,
						Namespace: sandboxClaim.Namespace,
					}, sandboxClaim)
					if err != nil {
						return ""
					}
					return sandboxClaim.Status.Phase
				}, time.Minute, time.Second).Should(Equal(agentsv1alpha1.SandboxClaimPhaseCompleted))

				By("Recording first claim's UID and sandboxes")
				firstClaimUID := string(sandboxClaim.UID)

				firstClaimSandboxes, err := listClaimedSandboxes(ctx, sandboxClaim)
				Expect(err).NotTo(HaveOccurred())
				Expect(firstClaimSandboxes).To(HaveLen(2), "First claim should have 2 sandboxes")

				firstClaimSandboxNames := make([]string, 0, 2)
				for _, sandbox := range firstClaimSandboxes {
					firstClaimSandboxNames = append(firstClaimSandboxNames, sandbox.Name)
					// Verify first generation label
					Expect(sandbox.Labels).To(HaveKeyWithValue("generation", "first"))
				}

				By("Deleting the first claim")
				Expect(k8sClient.Delete(ctx, sandboxClaim)).To(Succeed())

				// Wait for claim to be deleted
				Eventually(func() bool {
					err := k8sClient.Get(ctx, types.NamespacedName{
						Name:      sandboxClaim.Name,
						Namespace: sandboxClaim.Namespace,
					}, sandboxClaim)
					return apierrors.IsNotFound(err)
				}, time.Second*10, time.Second).Should(BeTrue())

				By("Verifying first claim's sandboxes still exist (not cascade deleted)")
				sandboxList := &agentsv1alpha1.SandboxList{}
				Expect(k8sClient.List(ctx, sandboxList, client.InNamespace(namespace))).To(Succeed())

				stillExistCount := 0
				for _, sandbox := range sandboxList.Items {
					if sandbox.Annotations != nil && sandbox.Annotations[agentsv1alpha1.AnnotationOwner] == firstClaimUID {
						stillExistCount++
					}
				}
				Expect(stillExistCount).To(Equal(2), "Old sandboxes should still exist")

				By("Creating a new claim with the SAME name but different spec")
				sandboxClaim = &agentsv1alpha1.SandboxClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      firstClaimName, // Same name!
						Namespace: namespace,
					},
					Spec: agentsv1alpha1.SandboxClaimSpec{
						TemplateName: sandboxSet.Name,
						Replicas:     ptr.To(int32(2)),
						Labels: map[string]string{
							"generation": "second", // Different label
						},
						SkipInitRuntime: true,
					},
				}
				Expect(k8sClient.Create(ctx, sandboxClaim)).To(Succeed())

				// Wait for completion
				Eventually(func() agentsv1alpha1.SandboxClaimPhase {
					err := k8sClient.Get(ctx, types.NamespacedName{
						Name:      sandboxClaim.Name,
						Namespace: sandboxClaim.Namespace,
					}, sandboxClaim)
					if err != nil {
						return ""
					}
					return sandboxClaim.Status.Phase
				}, time.Minute, time.Second).Should(Equal(agentsv1alpha1.SandboxClaimPhaseCompleted))

				By("Verifying second claim has DIFFERENT UID and claimed NEW sandboxes")
				secondClaimUID := string(sandboxClaim.UID)
				Expect(secondClaimUID).NotTo(Equal(firstClaimUID), "New claim should have different UID")

				secondClaimSandboxes, err := listClaimedSandboxes(ctx, sandboxClaim)
				Expect(err).NotTo(HaveOccurred())
				Expect(secondClaimSandboxes).To(HaveLen(2), "Second claim should have 2 NEW sandboxes")

				secondClaimSandboxNames := make([]string, 0, 2)
				for _, sandbox := range secondClaimSandboxes {
					secondClaimSandboxNames = append(secondClaimSandboxNames, sandbox.Name)
					// Verify second generation label
					Expect(sandbox.Labels).To(HaveKeyWithValue("generation", "second"))
					// Verify annotation (UID)
					Expect(sandbox.Annotations).To(HaveKeyWithValue(
						agentsv1alpha1.AnnotationOwner,
						secondClaimUID,
					))
					// Verify label (Name for display)
					Expect(sandbox.Labels).To(HaveKeyWithValue(
						agentsv1alpha1.LabelSandboxClaimName,
						firstClaimName,
					))
				}

				By("Verifying old and new sandboxes are DIFFERENT")
				// No overlap between first and second claim sandboxes
				for _, newName := range secondClaimSandboxNames {
					Expect(firstClaimSandboxNames).NotTo(ContainElement(newName),
						"Second claim should NOT reuse sandboxes from first claim")
				}

				By("Verifying both sets of sandboxes exist simultaneously")
				// Query by name label - should find sandboxes from both claims
				allSandboxList := &agentsv1alpha1.SandboxList{}
				Expect(k8sClient.List(ctx, allSandboxList,
					client.InNamespace(namespace),
					client.MatchingLabels{
						agentsv1alpha1.LabelSandboxClaimName: firstClaimName,
					},
				)).To(Succeed())
				Expect(allSandboxList.Items).To(HaveLen(4),
					"Should have 4 total sandboxes (2 from first claim + 2 from second claim)")

				By("Verifying each sandbox has correct UID annotation")
				firstGenCount := 0
				secondGenCount := 0
				for _, sandbox := range allSandboxList.Items {
					uid := ""
					if sandbox.Annotations != nil {
						uid = sandbox.Annotations[agentsv1alpha1.AnnotationOwner]
					}
					if uid == firstClaimUID {
						firstGenCount++
						Expect(sandbox.Labels).To(HaveKeyWithValue("generation", "first"))
					} else if uid == secondClaimUID {
						secondGenCount++
						Expect(sandbox.Labels).To(HaveKeyWithValue("generation", "second"))
					} else {
						Fail(fmt.Sprintf("Unexpected UID: %s", uid))
					}
				}
				Expect(firstGenCount).To(Equal(2), "Should have 2 sandboxes from first claim")
				Expect(secondGenCount).To(Equal(2), "Should have 2 sandboxes from second claim")
			})
		})
	})
})
