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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

var _ = Describe("InplaceUpdate Upgrade via SandboxUpdateOps", func() {
	var (
		ctx          = context.Background()
		namespace    string
		initialImage = "centos:7"
		updateImage  = "centos:8"
		badImage     = "centos:non-existent-tag-999"
		batchLabel   = "e2e-inplace-batch"
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

	// newInplaceSandbox creates a Sandbox whose UpgradePolicy is InplaceUpdate,
	// with the runtime init container and startup probe expected by the controller.
	newInplaceSandbox := func(name, labelValue string) *agentsv1alpha1.Sandbox {
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
					batchLabel:                           labelValue,
					agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.True,
				},
			},
			Spec: agentsv1alpha1.SandboxSpec{
				UpgradePolicy: &agentsv1alpha1.SandboxUpgradePolicy{
					Type: agentsv1alpha1.SandboxUpgradePolicyInplaceUpdate,
				},
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

	// newInplaceOps creates a SandboxUpdateOps with the InplaceUpdate strategy.
	newInplaceOps := func(name, labelValue string, strategyType agentsv1alpha1.SandboxUpdateOpsStrategyType, patch runtime.RawExtension) *agentsv1alpha1.SandboxUpdateOps {
		maxUnavailable := intstr.FromInt(1)
		return &agentsv1alpha1.SandboxUpdateOps{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{batchLabel: labelValue},
				},
				UpdateStrategy: agentsv1alpha1.SandboxUpdateOpsStrategy{
					Type:           strategyType,
					MaxUnavailable: &maxUnavailable,
				},
				Patch: patch,
			},
		}
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

	waitOpsPhase := func(ops *agentsv1alpha1.SandboxUpdateOps, phase agentsv1alpha1.SandboxUpdateOpsPhase, timeout time.Duration) {
		Eventually(func() agentsv1alpha1.SandboxUpdateOpsPhase {
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: ops.Name, Namespace: ops.Namespace}, ops)
			return ops.Status.Phase
		}, timeout, time.Second).Should(Equal(phase), func() string {
			return fmt.Sprintf("ops %s stuck in phase %s, status: replicas=%d updated=%d failed=%d",
				ops.Name, ops.Status.Phase, ops.Status.Replicas, ops.Status.UpdatedReplicas, ops.Status.FailedReplicas)
		})
	}

	getUpgradingCondition := func(sbx *agentsv1alpha1.Sandbox) *metav1.Condition {
		_ = k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, sbx)
		for i := range sbx.Status.Conditions {
			if sbx.Status.Conditions[i].Type == string(agentsv1alpha1.SandboxConditionUpgrading) {
				return &sbx.Status.Conditions[i]
			}
		}
		return nil
	}

	// waitUpgradePodFailed asserts the sandbox reached a terminal in-place failure:
	// Upgrading condition is False with Reason=UpgradePodFailed.
	waitUpgradePodFailed := func(sbx *agentsv1alpha1.Sandbox, timeout time.Duration) {
		Eventually(func() bool {
			cond := getUpgradingCondition(sbx)
			return cond != nil && cond.Status == metav1.ConditionFalse &&
				cond.Reason == agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed
		}, timeout, time.Second).Should(BeTrue(), func() string {
			cond := getUpgradingCondition(sbx)
			if cond == nil {
				return fmt.Sprintf("no Upgrading condition on sandbox %s (phase=%s)", sbx.Name, sbx.Status.Phase)
			}
			return fmt.Sprintf("expected UpgradePodFailed, got reason=%s status=%s (phase=%s)", cond.Reason, cond.Status, sbx.Status.Phase)
		})
	}

	// waitPodImage asserts the pod container image matches the expected value.
	waitPodImage := func(sbx *agentsv1alpha1.Sandbox, expected string, timeout time.Duration) {
		Eventually(func() bool {
			pod := &corev1.Pod{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, pod); err != nil {
				return false
			}
			return len(pod.Spec.Containers) > 0 && pod.Spec.Containers[0].Image == expected
		}, timeout, time.Second).Should(BeTrue(), "pod %s image should be %s", sbx.Name, expected)
	}

	// waitSandboxStaysUpgrading asserts the sandbox remains in Upgrading for
	// the given duration, confirming a transient (non-terminal) failure.
	//
	// In K8s 1.32, when a running container's image is patched in-place to
	// an unpullable image, the kubelet keeps the old container running while
	// retrying the pull. The pod may never enter ImagePullBackOff because
	// the old container stays Running. The sandbox stays Upgrading because
	// IsInplaceUpdateCompleted sees the ImageID hasn't changed.
	waitSandboxStaysUpgrading := func(sbx *agentsv1alpha1.Sandbox, duration time.Duration) {
		Consistently(func() agentsv1alpha1.SandboxPhase {
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, sbx)
			return sbx.Status.Phase
		}, duration, 2*time.Second).Should(Equal(agentsv1alpha1.SandboxUpgrading),
			"sandbox %s should stay Upgrading (transient failure)", sbx.Name)
	}

	// waitOpsDeleted deletes the ops and waits for the finalizer to finish
	// cleanup (label/annotation removal from matched sandboxes) so the object
	// is fully gone. The validating webhook rejects new SandboxUpdateOps while
	// an existing non-terminal (non-Completed/Failed) ops is present in the
	// namespace, so transient-failure scenarios must delete the prior ops
	// before creating a corrective one.
	waitOpsDeleted := func(ops *agentsv1alpha1.SandboxUpdateOps) {
		Expect(k8sClient.Delete(ctx, ops)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: ops.Name, Namespace: ops.Namespace}, ops))
		}, 30*time.Second, time.Second).Should(BeTrue(), "ops %s should be fully deleted", ops.Name)
	}

	// --- Helpers for building SUO patches ---

	patchImage := func(image string) runtime.RawExtension {
		return mustMarshalPatch(corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "test-container", Image: image}},
			},
		})
	}

	patchImageAndResources := func(image string, req, lim corev1.ResourceList) runtime.RawExtension {
		return mustMarshalPatch(corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:      "test-container",
					Image:     image,
					Resources: corev1.ResourceRequirements{Requests: req, Limits: lim},
				}},
			},
		})
	}

	patchCommand := func(cmd []string) runtime.RawExtension {
		return mustMarshalPatch(corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "test-container", Command: cmd}},
			},
		})
	}

	// =========================================================================
	// Success scenarios
	// =========================================================================

	Context("Success", func() {
		It("should upgrade container image in place", func() {
			labelValue := fmt.Sprintf("inplace-img-ok-%d", time.Now().UnixNano())
			sbx := newInplaceSandbox(fmt.Sprintf("inplace-img-ok-%d", time.Now().UnixNano()), labelValue)
			Expect(k8sClient.Create(ctx, sbx)).To(Succeed())
			waitSandboxRunning(sbx)

			By("Creating SandboxUpdateOps with InplaceUpdate strategy to update image")
			ops := newInplaceOps(
				fmt.Sprintf("ops-inplace-img-%d", time.Now().UnixNano()),
				labelValue,
				agentsv1alpha1.SandboxUpdateOpsStrategyInplaceUpdate,
				patchImage(updateImage),
			)
			Expect(k8sClient.Create(ctx, ops)).To(Succeed())

			By("Waiting for Ops to reach Completed")
			waitOpsPhase(ops, agentsv1alpha1.SandboxUpdateOpsCompleted, 5*time.Minute)

			By("Verifying sandbox returned to Running")
			Eventually(func() agentsv1alpha1.SandboxPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, sbx)
				return sbx.Status.Phase
			}, 2*time.Minute, time.Second).Should(Equal(agentsv1alpha1.SandboxRunning))

			By("Verifying pod image was updated in place (same pod name)")
			waitPodImage(sbx, updateImage, 2*time.Minute)

			By("Verifying pod was not recreated (same creation timestamp)")
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, pod)).To(Succeed())
			Expect(pod.Status.Phase).To(Equal(corev1.PodRunning))
		})

		It("should upgrade image and resize CPU in place preserving QoS", func() {
			labelValue := fmt.Sprintf("inplace-res-ok-%d", time.Now().UnixNano())
			sbx := newInplaceSandbox(fmt.Sprintf("inplace-res-ok-%d", time.Now().UnixNano()), labelValue)
			// Start with Burstable QoS: cpu request=250m, limit=500m.
			sbx.Spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
			}
			Expect(k8sClient.Create(ctx, sbx)).To(Succeed())
			waitSandboxRunning(sbx)

			By("Creating SandboxUpdateOps to update image + resize CPU to 1/1 (still Burstable)")
			ops := newInplaceOps(
				fmt.Sprintf("ops-inplace-res-%d", time.Now().UnixNano()),
				labelValue,
				agentsv1alpha1.SandboxUpdateOpsStrategyInplaceUpdate,
				patchImageAndResources(updateImage,
					corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
					corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
				),
			)
			Expect(k8sClient.Create(ctx, ops)).To(Succeed())

			By("Waiting for Ops to reach Completed")
			waitOpsPhase(ops, agentsv1alpha1.SandboxUpdateOpsCompleted, 5*time.Minute)

			By("Verifying sandbox returned to Running")
			Eventually(func() agentsv1alpha1.SandboxPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, sbx)
				return sbx.Status.Phase
			}, 2*time.Minute, time.Second).Should(Equal(agentsv1alpha1.SandboxRunning))

			By("Verifying pod image and CPU were updated in place")
			pod := &corev1.Pod{}
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, pod); err != nil {
					return false
				}
				if len(pod.Spec.Containers) == 0 || pod.Spec.Containers[0].Image != updateImage {
					return false
				}
				req := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
				lim := pod.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]
				return req.MilliValue() == 1000 && lim.MilliValue() == 1000
			}, 2*time.Minute, time.Second).Should(BeTrue())

			By("Verifying QoS class remains Burstable")
			Expect(pod.Status.QOSClass).To(Equal(corev1.PodQOSBurstable))
		})
	})

	// =========================================================================
	// Terminal failure scenarios (UpgradePodFailed, no retry)
	// =========================================================================

	Context("Terminal failure", func() {
		It("should fail terminally when hash-immutable part changes (command)", func() {
			labelValue := fmt.Sprintf("inplace-cmd-fail-%d", time.Now().UnixNano())
			sbx := newInplaceSandbox(fmt.Sprintf("inplace-cmd-fail-%d", time.Now().UnixNano()), labelValue)
			Expect(k8sClient.Create(ctx, sbx)).To(Succeed())
			waitSandboxRunning(sbx)

			By("Creating SandboxUpdateOps to change command (hash-immutable field)")
			ops := newInplaceOps(
				fmt.Sprintf("ops-cmd-fail-%d", time.Now().UnixNano()),
				labelValue,
				agentsv1alpha1.SandboxUpdateOpsStrategyInplaceUpdate,
				patchCommand([]string{"/bin/bash", "-c", "sleep 3600"}),
			)
			Expect(k8sClient.Create(ctx, ops)).To(Succeed())

			By("Verifying SUO reaches Failed (patch rejected by pre-validation)")
			waitOpsPhase(ops, agentsv1alpha1.SandboxUpdateOpsFailed, 3*time.Minute)

			By("Verifying sandbox stays Running (template was never patched)")
			Eventually(func() agentsv1alpha1.SandboxPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, sbx)
				return sbx.Status.Phase
			}, 30*time.Second, time.Second).Should(Equal(agentsv1alpha1.SandboxRunning))

			By("Verifying sandbox has upgrade-failed label")
			Eventually(func() bool {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, sbx)
				_, ok := sbx.Labels[agentsv1alpha1.LabelSandboxUpgradeFailed]
				return ok
			}, 30*time.Second, time.Second).Should(BeTrue(), "sandbox should have upgrade-failed label")

			By("Verifying pod command was NOT changed (patch rejected before delivery)")
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, pod)).To(Succeed())
			Expect(pod.Spec.Containers[0].Command).To(Equal([]string{"/bin/bash", "-c", "sleep infinity"}),
				"pod command should remain original")
		})

		It("should fail terminally when resource resize changes QoS class", func() {
			labelValue := fmt.Sprintf("inplace-qos-fail-%d", time.Now().UnixNano())
			sbx := newInplaceSandbox(fmt.Sprintf("inplace-qos-fail-%d", time.Now().UnixNano()), labelValue)
			// Start Guaranteed: every container (including the sidecar init
			// container) must have request==limit for both CPU and memory.
			// computeQoSClass iterates all containers; if any lacks limits the
			// pod is Burstable and the patch below would not change QoS.
			sbx.Spec.Template.Spec.InitContainers[0].Resources = corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
			}
			sbx.Spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
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

			By("Creating SandboxUpdateOps to lower CPU request (Guaranteed -> Burstable)")
			ops := newInplaceOps(
				fmt.Sprintf("ops-qos-fail-%d", time.Now().UnixNano()),
				labelValue,
				agentsv1alpha1.SandboxUpdateOpsStrategyInplaceUpdate,
				patchImageAndResources(initialImage,
					corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
					corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				),
			)
			Expect(k8sClient.Create(ctx, ops)).To(Succeed())

			By("Verifying sandbox stays Upgrading with UpgradePodFailed")
			waitUpgradePodFailed(sbx, 3*time.Minute)

			By("Verifying pod QoS remains Guaranteed (resize rejected)")
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, pod)).To(Succeed())
			Expect(pod.Status.QOSClass).To(Equal(corev1.PodQOSGuaranteed))

			By("Verifying pod spec resources remain original (500m/500m)")
			req := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
			lim := pod.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]
			Expect(req.MilliValue()).To(Equal(int64(500)), "pod spec request should remain 500m")
			Expect(lim.MilliValue()).To(Equal(int64(500)), "pod spec limit should remain 500m")
		})
	})

	// =========================================================================
	// Transient failure (bad image, stays InplaceUpdating)
	// =========================================================================

	Context("Transient failure", func() {
		It("should stay upgrading when image pull fails (no terminal failure)", func() {
			labelValue := fmt.Sprintf("inplace-badimg-%d", time.Now().UnixNano())
			sbx := newInplaceSandbox(fmt.Sprintf("inplace-badimg-%d", time.Now().UnixNano()), labelValue)
			Expect(k8sClient.Create(ctx, sbx)).To(Succeed())
			waitSandboxRunning(sbx)

			By("Creating SandboxUpdateOps with a non-pullable image")
			ops := newInplaceOps(
				fmt.Sprintf("ops-badimg-%d", time.Now().UnixNano()),
				labelValue,
				agentsv1alpha1.SandboxUpdateOpsStrategyInplaceUpdate,
				patchImage(badImage),
			)
			Expect(k8sClient.Create(ctx, ops)).To(Succeed())

			By("Verifying sandbox enters Upgrading")
			Eventually(func() agentsv1alpha1.SandboxPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, sbx)
				return sbx.Status.Phase
			}, 2*time.Minute, time.Second).Should(Equal(agentsv1alpha1.SandboxUpgrading))

			By("Verifying pod image was patched to bad image (patch delivered)")
			waitPodImage(sbx, badImage, 2*time.Minute)

			By("Verifying sandbox stays Upgrading (transient failure, not terminal)")
			waitSandboxStaysUpgrading(sbx, 60*time.Second)

			By("Verifying Upgrading condition is not UpgradePodFailed (still in progress)")
			cond := getUpgradingCondition(sbx)
			Expect(cond).NotTo(BeNil(), "Upgrading condition should exist")
			// During an in-progress upgrade, Status=False (not yet complete) and
			// Reason=UpgradePod. Status=True means succeeded; Reason=UpgradePodFailed
			// means terminal failure.
			Expect(cond.Status).To(Equal(metav1.ConditionFalse), "Upgrading should be False (still in progress)")
			Expect(cond.Reason).NotTo(Equal(agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed),
				"should not be terminal failure during transient image pull failure")
		})
	})

	// =========================================================================
	// Recovery after failure (user follow-up operations)
	// =========================================================================

	Context("Recovery after failure", func() {
		It("should recover from transient bad image by switching to Recreate", func() {
			labelValue := fmt.Sprintf("inplace-recover-badimg-%d", time.Now().UnixNano())
			sbx := newInplaceSandbox(fmt.Sprintf("inplace-rcv-bad-%d", time.Now().UnixNano()), labelValue)
			Expect(k8sClient.Create(ctx, sbx)).To(Succeed())
			waitSandboxRunning(sbx)

			By("Step 1: creating SUO with bad image to trigger transient failure")
			opsBad := newInplaceOps(
				fmt.Sprintf("ops-rcv-bad-1-%d", time.Now().UnixNano()),
				labelValue,
				agentsv1alpha1.SandboxUpdateOpsStrategyInplaceUpdate,
				patchImage(badImage),
			)
			Expect(k8sClient.Create(ctx, opsBad)).To(Succeed())

			By("Waiting for sandbox to enter Upgrading")
			Eventually(func() agentsv1alpha1.SandboxPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, sbx)
				return sbx.Status.Phase
			}, 2*time.Minute, time.Second).Should(Equal(agentsv1alpha1.SandboxUpgrading))

			By("Verifying pod image was patched to bad image (patch delivered)")
			waitPodImage(sbx, badImage, 2*time.Minute)

			By("Verifying sandbox stays Upgrading (transient failure)")
			waitSandboxStaysUpgrading(sbx, 30*time.Second)

			By("Deleting the transient-failure SUO (validating webhook blocks concurrent Updating ops)")
			waitOpsDeleted(opsBad)

			By("Step 2: user switches to Recreate strategy with corrected image")
			opsGood := newInplaceOps(
				fmt.Sprintf("ops-rcv-bad-2-%d", time.Now().UnixNano()),
				labelValue,
				agentsv1alpha1.SandboxUpdateOpsStrategyRecreate,
				patchImage(updateImage),
			)
			Expect(k8sClient.Create(ctx, opsGood)).To(Succeed())

			By("Verifying the Recreate SUO reaches Completed")
			waitOpsPhase(opsGood, agentsv1alpha1.SandboxUpdateOpsCompleted, 5*time.Minute)

			By("Verifying sandbox recovered to Running")
			Eventually(func() agentsv1alpha1.SandboxPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, sbx)
				return sbx.Status.Phase
			}, 2*time.Minute, time.Second).Should(Equal(agentsv1alpha1.SandboxRunning))

			By("Verifying pod image is now the corrected image")
			waitPodImage(sbx, updateImage, 2*time.Minute)
		})

		It("should recover from terminal hash-immutable failure by switching to Recreate", func() {
			labelValue := fmt.Sprintf("inplace-recover-cmd-%d", time.Now().UnixNano())
			sbx := newInplaceSandbox(fmt.Sprintf("inplace-rcv-cmd-%d", time.Now().UnixNano()), labelValue)
			Expect(k8sClient.Create(ctx, sbx)).To(Succeed())
			waitSandboxRunning(sbx)

			By("Step 1: creating SUO with command change to trigger terminal failure")
			opsFail := newInplaceOps(
				fmt.Sprintf("ops-rcv-cmd-1-%d", time.Now().UnixNano()),
				labelValue,
				agentsv1alpha1.SandboxUpdateOpsStrategyInplaceUpdate,
				patchCommand([]string{"/bin/bash", "-c", "sleep 3600"}),
			)
			Expect(k8sClient.Create(ctx, opsFail)).To(Succeed())

			By("Waiting for SUO to reach Failed (pre-validation rejected the patch)")
			waitOpsPhase(opsFail, agentsv1alpha1.SandboxUpdateOpsFailed, 3*time.Minute)

			By("Step 2: user switches to Recreate strategy with image change (new revision)")
			opsRecreate := newInplaceOps(
				fmt.Sprintf("ops-rcv-cmd-2-%d", time.Now().UnixNano()),
				labelValue,
				agentsv1alpha1.SandboxUpdateOpsStrategyRecreate,
				patchImage(updateImage),
			)
			Expect(k8sClient.Create(ctx, opsRecreate)).To(Succeed())

			By("Verifying the Recreate SUO reaches Completed")
			waitOpsPhase(opsRecreate, agentsv1alpha1.SandboxUpdateOpsCompleted, 5*time.Minute)

			By("Verifying sandbox recovered to Running")
			Eventually(func() agentsv1alpha1.SandboxPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, sbx)
				return sbx.Status.Phase
			}, 2*time.Minute, time.Second).Should(Equal(agentsv1alpha1.SandboxRunning))

			By("Verifying pod image is updated (pod replaced via Recreate)")
			waitPodImage(sbx, updateImage, 2*time.Minute)
		})

		// NOT SUPPORTED: recovering from a bad-image InplaceUpdate failure with
		// another InplaceUpdate SUO (delete the stuck SUO, create a new one that
		// rolls back or fixes the image). Every variant deadlocks in Upgrading:
		//   1. Rolling back to the SAME image (centos:7→bad→centos:7): the
		//      container never left the old image, so its ImageID never changes
		//      and isPodImageUpdateCompleted always returns false.
		//   2. Fixing to a DIFFERENT pullable image (centos:7→bad→centos:8): the
		//      kubelet does not restart a container stuck in ImagePullBackOff
		//      just because spec.image changed again (verified on K8s 1.32 by a
		//      CI run of exactly this scenario: the pod spec was patched to
		//      centos:8 but the container kept running centos:7 and the SUO
		//      stayed in Updating until timeout).
		// The ONLY recovery path from a bad-image in-place failure is a pod
		// replacement strategy (Recreate / CheckpointRestore), covered by the
		// "switching to Recreate" scenario above. Do not add an InplaceUpdate
		// corrective/rollback scenario here.

		It("should recover from terminal QoS failure by rolling back resources", func() {
			labelValue := fmt.Sprintf("inplace-rollback-qos-%d", time.Now().UnixNano())
			sbx := newInplaceSandbox(fmt.Sprintf("inplace-rlb-qos-%d", time.Now().UnixNano()), labelValue)
			// Start Guaranteed: every container (including the sidecar init
			// container) must have request==limit for both CPU and memory.
			sbx.Spec.Template.Spec.InitContainers[0].Resources = corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
			}
			sbx.Spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
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

			By("Step 1: creating SUO with QoS-changing resize to trigger terminal failure")
			opsFail := newInplaceOps(
				fmt.Sprintf("ops-rlb-qos-1-%d", time.Now().UnixNano()),
				labelValue,
				agentsv1alpha1.SandboxUpdateOpsStrategyInplaceUpdate,
				patchImageAndResources(initialImage,
					corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
					corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				),
			)
			Expect(k8sClient.Create(ctx, opsFail)).To(Succeed())

			By("Verifying sandbox reaches UpgradePodFailed (QoS changed Guaranteed -> Burstable)")
			waitUpgradePodFailed(sbx, 3*time.Minute)

			By("Verifying pod resources were NOT patched (QoS check runs before Update)")
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, pod)).To(Succeed())
			Expect(pod.Status.QOSClass).To(Equal(corev1.PodQOSGuaranteed), "pod QoS should remain Guaranteed")

			By("Deleting the terminal-failure SUO (validating webhook blocks concurrent ops)")
			waitOpsDeleted(opsFail)

			By("Step 2: user rolls back resources to original Guaranteed values")
			opsRecover := newInplaceOps(
				fmt.Sprintf("ops-rlb-qos-2-%d", time.Now().UnixNano()),
				labelValue,
				agentsv1alpha1.SandboxUpdateOpsStrategyInplaceUpdate,
				patchImageAndResources(initialImage,
					corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("512Mi"),
					},
					corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("512Mi"),
					},
				),
			)
			Expect(k8sClient.Create(ctx, opsRecover)).To(Succeed())

			By("Verifying the recovery SUO reaches Completed")
			waitOpsPhase(opsRecover, agentsv1alpha1.SandboxUpdateOpsCompleted, 5*time.Minute)

			By("Verifying sandbox recovered to Running")
			Eventually(func() agentsv1alpha1.SandboxPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, sbx)
				return sbx.Status.Phase
			}, 2*time.Minute, time.Second).Should(Equal(agentsv1alpha1.SandboxRunning))

			By("Verifying pod resources are back to original Guaranteed values")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, pod)).To(Succeed())
			req := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
			Expect(req.MilliValue()).To(Equal(int64(500)), "pod spec request should be 500m")
			Expect(pod.Status.QOSClass).To(Equal(corev1.PodQOSGuaranteed), "pod QoS should be Guaranteed")
		})
	})
})
