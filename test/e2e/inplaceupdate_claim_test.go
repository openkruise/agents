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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// This test file covers the SandboxClaim delivery path (a.k.a. "claim path"):
// sandboxes without an UpgradePolicy whose template is changed directly via
// k8sClient.Update.  The controller applies these changes in place from the
// Running phase (see common_control.go EnsureSandboxUpdated).  Unlike the
// upgrade path (covered by inplaceupdate_upgrade_test.go), the claim path
// never enters the Upgrading phase.
//
// Failure semantics differ from the upgrade path:
//   - Terminal failure (hash-immutable, QoS change): the sandbox stays Running
//     and Ready=True (the change is "delivered" with an InplaceUpdate=Failed
//     condition so the claim can still be served).
//   - Transient failure (bad image): the sandbox stays Running but Ready=False
//     (not delivered) with InplaceUpdate=InplaceUpdating.
var _ = Describe("InplaceUpdate Claim Path (SandboxClaim delivery)", func() {
	var (
		ctx          = context.Background()
		namespace    string
		initialImage = "centos:7"
		updateImage  = "centos:8"
		badImage     = "centos:non-existent-tag-999"
	)

	BeforeEach(func() {
		namespace = createNamespace(ctx)
	})

	AfterEach(func() {
		ns := &corev1.Namespace{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: namespace}, ns); err == nil {
			_ = k8sClient.Delete(ctx, ns)
		}
	})

	// newClaimSandbox creates a Sandbox without an UpgradePolicy.  Template
	// changes to such a sandbox are applied in place from the Running phase
	// via handleInplaceUpdateSandbox, never entering the Upgrading phase.
	newClaimSandbox := func(name string) *agentsv1alpha1.Sandbox {
		alwaysRestart := corev1.ContainerRestartPolicyAlways
		mounts := []corev1.VolumeMount{{Name: "envd-volume", MountPath: "/mnt/envd"}}
		volumes := []corev1.Volume{{
			Name:         "envd-volume",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		}}
		return &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels: map[string]string{
					agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.True,
				},
			},
			Spec: agentsv1alpha1.SandboxSpec{
				// No UpgradePolicy — this is the claim path.
				EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							InitContainers: []corev1.Container{{
								Name:    "runtime",
								Image:   "openkruise/agent-runtime:v0.2.0",
								Command: []string{"sh", "/workspace/entrypoint.sh"},
								VolumeMounts: []corev1.VolumeMount{
									{Name: "envd-volume", MountPath: "/mnt/envd"},
								},
								Env:           []corev1.EnvVar{{Name: "ENVD_DIR", Value: "/mnt/envd"}},
								RestartPolicy: &alwaysRestart,
							}},
							Containers: []corev1.Container{{
								Name:    "test-container",
								Image:   initialImage,
								Command: []string{"/bin/bash", "-c", "sleep infinity"},
								Env:     []corev1.EnvVar{{Name: "ENVD_DIR", Value: "/mnt/envd"}},
								StartupProbe: &corev1.Probe{
									ProbeHandler: corev1.ProbeHandler{
										TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(49983)},
									},
									FailureThreshold: 20,
									PeriodSeconds:    1,
								},
								VolumeMounts: mounts,
								Lifecycle: &corev1.Lifecycle{
									PostStart: &corev1.LifecycleHandler{
										Exec: &corev1.ExecAction{
											Command: []string{"sh", "/mnt/envd/envd-run.sh"},
										},
									},
								},
							}},
							Volumes:       volumes,
							RestartPolicy: corev1.RestartPolicyNever,
						},
					},
				},
			},
		}
	}

	// updateSandboxTemplate fetches the latest sandbox, applies the modifier
	// to its template, and retries on conflict.  The controller updates sandbox
	// status concurrently, so a plain Update will often 409.
	updateSandboxTemplate := func(sbx *agentsv1alpha1.Sandbox, modify func(spec *corev1.PodTemplateSpec)) {
		Eventually(func() error {
			latest := &agentsv1alpha1.Sandbox{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, latest); err != nil {
				return err
			}
			if latest.Spec.EmbeddedSandboxTemplate.Template == nil {
				return fmt.Errorf("template is nil")
			}
			modify(latest.Spec.EmbeddedSandboxTemplate.Template)
			return k8sClient.Update(ctx, latest)
		}, 30*time.Second, time.Second).Should(Succeed())
	}

	waitSandboxRunning := func(sbx *agentsv1alpha1.Sandbox) {
		Eventually(func() agentsv1alpha1.SandboxPhase {
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, sbx)
			return sbx.Status.Phase
		}, 3*time.Minute, 500*time.Millisecond).Should(Equal(agentsv1alpha1.SandboxRunning), func() string {
			msg := fmt.Sprintf("sandbox %s stuck in phase %s", sbx.Name, sbx.Status.Phase)
			pod := &corev1.Pod{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, pod); err == nil {
				msg += fmt.Sprintf("\npodPhase: %s, containerStatuses: %v", pod.Status.Phase, pod.Status.ContainerStatuses)
			}
			return msg
		})
	}

	waitPodImage := func(sbx *agentsv1alpha1.Sandbox, expected string, timeout time.Duration) {
		Eventually(func() bool {
			pod := &corev1.Pod{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, pod); err != nil {
				return false
			}
			return len(pod.Spec.Containers) > 0 && pod.Spec.Containers[0].Image == expected
		}, timeout, time.Second).Should(BeTrue(), "pod %s image should be %s", sbx.Name, expected)
	}

	getInplaceUpdateCondition := func(sbx *agentsv1alpha1.Sandbox) *metav1.Condition {
		_ = k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, sbx)
		for i := range sbx.Status.Conditions {
			if sbx.Status.Conditions[i].Type == string(agentsv1alpha1.SandboxConditionInplaceUpdate) {
				return &sbx.Status.Conditions[i]
			}
		}
		return nil
	}

	getReadyCondition := func(sbx *agentsv1alpha1.Sandbox) *metav1.Condition {
		_ = k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, sbx)
		for i := range sbx.Status.Conditions {
			if sbx.Status.Conditions[i].Type == string(agentsv1alpha1.SandboxConditionReady) {
				return &sbx.Status.Conditions[i]
			}
		}
		return nil
	}

	// waitInplaceUpdateSucceeded asserts the InplaceUpdate condition reaches a
	// terminal success: Status=True, Reason=Succeeded. The condition is set to
	// False/InplaceUpdating as soon as the pod patch is applied, so the Eventually
	// must poll for Status==True, not merely for the condition to be non-nil.
	waitInplaceUpdateSucceeded := func(sbx *agentsv1alpha1.Sandbox, timeout time.Duration) {
		Eventually(func() metav1.ConditionStatus {
			cond := getInplaceUpdateCondition(sbx)
			if cond == nil {
				return ""
			}
			return cond.Status
		}, timeout, time.Second).Should(Equal(metav1.ConditionTrue), "InplaceUpdate should become True")
		cond := getInplaceUpdateCondition(sbx)
		Expect(cond.Reason).To(Equal(agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded), "InplaceUpdate reason should be Succeeded")
	}

	// waitInplaceUpdateFailed asserts the InplaceUpdate condition reaches a
	// terminal failure: Status=False, Reason=Failed, Message non-empty.
	// The condition starts as False/InplaceUpdating, so the Eventually must poll
	// for Reason==Failed, not merely for the condition to be non-nil.
	waitInplaceUpdateFailed := func(sbx *agentsv1alpha1.Sandbox, timeout time.Duration) {
		Eventually(func() string {
			cond := getInplaceUpdateCondition(sbx)
			if cond == nil {
				return ""
			}
			return cond.Reason
		}, timeout, time.Second).Should(Equal(agentsv1alpha1.SandboxInplaceUpdateReasonFailed), "InplaceUpdate reason should become Failed")
		cond := getInplaceUpdateCondition(sbx)
		Expect(cond.Status).To(Equal(metav1.ConditionFalse), "InplaceUpdate should be False")
		Expect(cond.Message).NotTo(BeEmpty(), "failure must have non-empty message (no silent failure)")
	}

	// =========================================================================
	// Success scenarios
	// =========================================================================

	Context("Success", func() {
		It("should update container image in place (claim path)", func() {
			sbx := newClaimSandbox(fmt.Sprintf("claim-img-ok-%d", time.Now().UnixNano()))
			Expect(k8sClient.Create(ctx, sbx)).To(Succeed())
			waitSandboxRunning(sbx)

			By("Updating sandbox template image to centos:8")
			updateSandboxTemplate(sbx, func(spec *corev1.PodTemplateSpec) {
				spec.Spec.Containers[0].Image = updateImage
			})

			By("Verifying pod image was updated in place")
			waitPodImage(sbx, updateImage, 3*time.Minute)

			By("Verifying sandbox stays Running (claim path does not enter Upgrading)")
			Consistently(func() agentsv1alpha1.SandboxPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, sbx)
				return sbx.Status.Phase
			}, 10*time.Second, 2*time.Second).Should(Equal(agentsv1alpha1.SandboxRunning))

			By("Verifying InplaceUpdate condition is Succeeded")
			waitInplaceUpdateSucceeded(sbx, 3*time.Minute)
		})

		It("should resize resources in place preserving QoS (claim path)", func() {
			sbx := newClaimSandbox(fmt.Sprintf("claim-res-ok-%d", time.Now().UnixNano()))
			// Start Burstable: cpu request=250m, limit=500m.
			sbx.Spec.EmbeddedSandboxTemplate.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
			}
			Expect(k8sClient.Create(ctx, sbx)).To(Succeed())
			waitSandboxRunning(sbx)

			By("Updating sandbox template to resize CPU to 1/1 (still Burstable)")
			updateSandboxTemplate(sbx, func(spec *corev1.PodTemplateSpec) {
				spec.Spec.Containers[0].Resources = corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
				}
			})

			By("Verifying pod CPU was resized in place")
			Eventually(func() bool {
				pod := &corev1.Pod{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, pod); err != nil {
					return false
				}
				req := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
				lim := pod.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]
				return req.MilliValue() == 1000 && lim.MilliValue() == 1000
			}, 3*time.Minute, time.Second).Should(BeTrue(), "pod CPU should be resized to 1/1")

			By("Verifying QoS class remains Burstable")
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, pod)).To(Succeed())
			Expect(pod.Status.QOSClass).To(Equal(corev1.PodQOSBurstable))

			By("Verifying sandbox stays Running")
			Consistently(func() agentsv1alpha1.SandboxPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, sbx)
				return sbx.Status.Phase
			}, 10*time.Second, 2*time.Second).Should(Equal(agentsv1alpha1.SandboxRunning))
		})

		It("should apply metadata-only change directly (claim path)", func() {
			sbx := newClaimSandbox(fmt.Sprintf("claim-meta-ok-%d", time.Now().UnixNano()))
			Expect(k8sClient.Create(ctx, sbx)).To(Succeed())
			waitSandboxRunning(sbx)

			By("Updating sandbox template labels only (metadata-only change)")
			updateSandboxTemplate(sbx, func(spec *corev1.PodTemplateSpec) {
				spec.Labels = map[string]string{"updated-by": "claim-e2e"}
			})

			By("Verifying pod labels were updated")
			Eventually(func() bool {
				pod := &corev1.Pod{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, pod); err != nil {
					return false
				}
				return pod.Labels["updated-by"] == "claim-e2e"
			}, 2*time.Minute, time.Second).Should(BeTrue(), "pod should have updated label")

			By("Verifying sandbox stays Running")
			Consistently(func() agentsv1alpha1.SandboxPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, sbx)
				return sbx.Status.Phase
			}, 10*time.Second, 2*time.Second).Should(Equal(agentsv1alpha1.SandboxRunning))

			By("Verifying no InplaceUpdate condition set (metadata-only bypasses inplace flow)")
			// Metadata-only changes are patched directly without setting the
			// InplaceUpdate condition.  The condition may be absent entirely.
			// If present from a prior reconcile, it must not be InplaceUpdating.
			cond := getInplaceUpdateCondition(sbx)
			if cond != nil {
				Expect(cond.Reason).NotTo(Equal(agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating),
					"metadata-only change should not set InplaceUpdating")
			}
		})
	})

	// =========================================================================
	// Terminal failure scenarios (deliver Running + InplaceUpdate=Failed)
	// =========================================================================

	Context("Terminal failure (deliver Running)", func() {
		It("should fail and deliver Running when resource resize changes QoS", func() {
			sbx := newClaimSandbox(fmt.Sprintf("claim-qos-fail-%d", time.Now().UnixNano()))
			// Start Guaranteed: every container (including the sidecar init
			// container) must have request==limit for both CPU and memory.
			sbx.Spec.EmbeddedSandboxTemplate.Template.Spec.InitContainers[0].Resources = corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
			}
			sbx.Spec.EmbeddedSandboxTemplate.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("512Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("512Mi"),
				},
			}
			Expect(k8sClient.Create(ctx, sbx)).To(Succeed())
			waitSandboxRunning(sbx)

			By("Updating sandbox template to lower CPU request (Guaranteed -> Burstable)")
			updateSandboxTemplate(sbx, func(spec *corev1.PodTemplateSpec) {
				spec.Spec.Containers[0].Resources = corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				}
			})

			By("Verifying InplaceUpdate condition is Failed with non-empty message")
			waitInplaceUpdateFailed(sbx, 3*time.Minute)

			By("Verifying sandbox stays Running (delivered despite failure)")
			Eventually(func() agentsv1alpha1.SandboxPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, sbx)
				return sbx.Status.Phase
			}, 30*time.Second, time.Second).Should(Equal(agentsv1alpha1.SandboxRunning))

			By("Verifying pod QoS remains Guaranteed (resize rejected)")
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, pod)).To(Succeed())
			Expect(pod.Status.QOSClass).To(Equal(corev1.PodQOSGuaranteed))
		})

		It("should fail and deliver Running when hash-immutable part changes", func() {
			sbx := newClaimSandbox(fmt.Sprintf("claim-cmd-fail-%d", time.Now().UnixNano()))
			Expect(k8sClient.Create(ctx, sbx)).To(Succeed())
			waitSandboxRunning(sbx)

			By("Updating sandbox template command (hash-immutable field)")
			updateSandboxTemplate(sbx, func(spec *corev1.PodTemplateSpec) {
				spec.Spec.Containers[0].Command = []string{"/bin/bash", "-c", "sleep 3600"}
			})

			By("Verifying InplaceUpdate condition is Failed with non-empty message")
			waitInplaceUpdateFailed(sbx, 3*time.Minute)

			By("Verifying sandbox stays Running (delivered despite failure)")
			Eventually(func() agentsv1alpha1.SandboxPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, sbx)
				return sbx.Status.Phase
			}, 30*time.Second, time.Second).Should(Equal(agentsv1alpha1.SandboxRunning))

			By("Verifying pod command was NOT changed (patch rejected before delivery)")
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, pod)).To(Succeed())
			Expect(pod.Spec.Containers[0].Command).To(Equal([]string{"/bin/bash", "-c", "sleep infinity"}),
				"pod command should remain original")
		})
	})

	// =========================================================================
	// Transient failure (no delivery, stays InplaceUpdating)
	// =========================================================================

	Context("Transient failure (no delivery)", func() {
		It("should not deliver when image pull fails (stays InplaceUpdating)", func() {
			sbx := newClaimSandbox(fmt.Sprintf("claim-badimg-%d", time.Now().UnixNano()))
			Expect(k8sClient.Create(ctx, sbx)).To(Succeed())
			waitSandboxRunning(sbx)

			By("Updating sandbox template image to a non-pullable image")
			updateSandboxTemplate(sbx, func(spec *corev1.PodTemplateSpec) {
				spec.Spec.Containers[0].Image = badImage
			})

			By("Verifying pod image was patched to bad image (patch delivered to pod spec)")
			waitPodImage(sbx, badImage, 2*time.Minute)

			By("Verifying sandbox stays Running (claim path never enters Upgrading)")
			Consistently(func() agentsv1alpha1.SandboxPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, sbx)
				return sbx.Status.Phase
			}, 30*time.Second, 2*time.Second).Should(Equal(agentsv1alpha1.SandboxRunning))

			By("Verifying InplaceUpdate condition is InplaceUpdating (not terminal)")
			// The condition may briefly be absent before the controller processes the
			// template change; poll until it appears with the expected transient state.
			Eventually(func() string {
				cond := getInplaceUpdateCondition(sbx)
				if cond == nil {
					return ""
				}
				return cond.Reason
			}, 2*time.Minute, time.Second).Should(Equal(agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating), "InplaceUpdate should be InplaceUpdating")
			cond := getInplaceUpdateCondition(sbx)
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))

			By("Verifying Ready condition is False (not delivered)")
			readyCond := getReadyCondition(sbx)
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
		})
	})

	// =========================================================================
	// Combination scenarios (verify no partial delivery)
	// =========================================================================

	Context("Combination (no partial delivery)", func() {
		It("should reject both image and resource when QoS would change", func() {
			sbx := newClaimSandbox(fmt.Sprintf("claim-combo-qos-%d", time.Now().UnixNano()))
			// Start Guaranteed.
			sbx.Spec.EmbeddedSandboxTemplate.Template.Spec.InitContainers[0].Resources = corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
			}
			sbx.Spec.EmbeddedSandboxTemplate.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("512Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("512Mi"),
				},
			}
			Expect(k8sClient.Create(ctx, sbx)).To(Succeed())
			waitSandboxRunning(sbx)

			By("Updating sandbox template: new image + QoS-changing resource in the same revision")
			updateSandboxTemplate(sbx, func(spec *corev1.PodTemplateSpec) {
				spec.Spec.Containers[0].Image = updateImage
				spec.Spec.Containers[0].Resources = corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				}
			})

			By("Verifying InplaceUpdate condition is Failed (QoS check rejected the whole change)")
			waitInplaceUpdateFailed(sbx, 3*time.Minute)

			By("Verifying pod image was NOT patched (no partial delivery)")
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, pod)).To(Succeed())
			Expect(pod.Spec.Containers[0].Image).To(Equal(initialImage), "image should NOT be patched")

			By("Verifying pod resources were NOT patched (no partial delivery)")
			req := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
			Expect(req.MilliValue()).To(Equal(int64(500)), "resource should NOT be patched")
			Expect(pod.Status.QOSClass).To(Equal(corev1.PodQOSGuaranteed), "QoS should remain Guaranteed")
		})

		It("should reject both image and hash-immutable change together", func() {
			sbx := newClaimSandbox(fmt.Sprintf("claim-combo-cmd-%d", time.Now().UnixNano()))
			Expect(k8sClient.Create(ctx, sbx)).To(Succeed())
			waitSandboxRunning(sbx)

			By("Updating sandbox template: new image + new command in the same revision")
			updateSandboxTemplate(sbx, func(spec *corev1.PodTemplateSpec) {
				spec.Spec.Containers[0].Image = updateImage
				spec.Spec.Containers[0].Command = []string{"/bin/bash", "-c", "sleep 3600"}
			})

			By("Verifying InplaceUpdate condition is Failed (hash check rejected the whole change)")
			waitInplaceUpdateFailed(sbx, 3*time.Minute)

			By("Verifying pod image was NOT patched (no partial delivery)")
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, pod)).To(Succeed())
			Expect(pod.Spec.Containers[0].Image).To(Equal(initialImage), "image should NOT be patched")

			By("Verifying pod command was NOT changed (no partial delivery)")
			Expect(pod.Spec.Containers[0].Command).To(Equal([]string{"/bin/bash", "-c", "sleep infinity"}),
				"command should remain original")
		})
	})
})
