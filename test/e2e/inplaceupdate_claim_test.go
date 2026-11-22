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

	"github.com/distribution/reference"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils/inplaceupdate"
)

// waitInplacePodObserved checks the delivered spec and actual kubelet state, comparing resource quantities and normalized image references.
// The UID verifies that the Pod was not recreated; the assertions do not depend on the controller's completion check.
func waitInplacePodObserved(ctx context.Context, sbx *agentsv1alpha1.Sandbox, uid types.UID, images map[string]string, resources map[string]corev1.ResourceRequirements) *corev1.Pod {
	pod := &corev1.Pod{}
	Eventually(func(g Gomega) {
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, pod)).To(Succeed())
		g.Expect(pod.UID).To(Equal(uid), "in-place upgrades must not recreate the Pod")
		g.Expect(pod.DeletionTimestamp).To(BeNil())
		containers := make(map[string]corev1.Container, len(pod.Spec.Containers))
		statuses := make(map[string]corev1.ContainerStatus, len(pod.Status.ContainerStatuses))
		for _, container := range pod.Spec.Containers {
			containers[container.Name] = container
		}
		for _, status := range pod.Status.ContainerStatuses {
			statuses[status.Name] = status
		}
		for name, image := range images {
			container, found := containers[name]
			g.Expect(found).To(BeTrue(), "Pod spec is missing container %s", name)
			g.Expect(container.Image).To(Equal(image))
			status, found := statuses[name]
			g.Expect(found).To(BeTrue(), "Pod status is missing container %s", name)
			g.Expect(status.State.Running).NotTo(BeNil(), "container %s must actually be Running", name)
			g.Expect(status.ImageID).NotTo(BeEmpty())
			actual, err := reference.ParseNormalizedNamed(status.Image)
			g.Expect(err).NotTo(HaveOccurred())
			expected, err := reference.ParseNormalizedNamed(image)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(reference.TagNameOnly(actual).String()).To(Equal(reference.TagNameOnly(expected).String()), "container %s is not running the target image yet", name)
		}
		for name, desired := range resources {
			container, found := containers[name]
			g.Expect(found).To(BeTrue())
			status, found := statuses[name]
			g.Expect(found).To(BeTrue())
			g.Expect(status.Resources).NotTo(BeNil(), "kubelet must report actual resources for container %s", name)
			for resourceName, quantity := range desired.Requests {
				spec := container.Resources.Requests[resourceName]
				actual := status.Resources.Requests[resourceName]
				g.Expect(spec.Cmp(quantity)).To(BeZero(), "container %s has not received the %s request", name, resourceName)
				g.Expect(actual.Cmp(quantity)).To(BeZero(), "container %s has not applied the %s request", name, resourceName)
			}
			for resourceName, quantity := range desired.Limits {
				spec := container.Resources.Limits[resourceName]
				actual := status.Resources.Limits[resourceName]
				g.Expect(spec.Cmp(quantity)).To(BeZero(), "container %s has not received the %s limit", name, resourceName)
				g.Expect(actual.Cmp(quantity)).To(BeZero(), "container %s has not applied the %s limit", name, resourceName)
			}
		}
	}, 3*time.Minute, time.Second).Should(Succeed())
	return pod
}

// This test file covers the SandboxClaim delivery path (a.k.a. "claim path"):
// sandboxes without an UpgradePolicy whose template is changed directly via
// k8sClient.Update.  The controller applies these changes in place from the
// Running phase (see common_control.go EnsureSandboxUpdated).  Unlike the
// upgrade path (covered by inplaceupdate_upgrade_test.go), the claim path
// never enters the Upgrading phase.
//
// Claim configuration completion and readiness are checked separately: metadata-only success preserves existing Ready,
// unapplied image or resource changes keep InplaceUpdating/Ready=False; unsupported template changes
// must set Failed/Ready=False even when the old Pod remains Ready.
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
		if CurrentSpecReport().Failed() && namespace != "" {
			diagnosticCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			_, _ = fmt.Fprintf(GinkgoWriter, "[claim-failure] collecting diagnostics before deleting namespace %s\n", namespace)
			sandboxes := &agentsv1alpha1.SandboxList{}
			if err := k8sClient.List(diagnosticCtx, sandboxes, &client.ListOptions{Namespace: namespace, Limit: 10}); err != nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "[claim-failure] list sandboxes: %v\n", err)
			} else {
				for _, sbx := range sandboxes.Items {
					status, err := json.Marshal(sbx.Status)
					if err != nil {
						_, _ = fmt.Fprintf(GinkgoWriter, "[claim-failure] encode sandbox %s status: %v\n", sbx.Name, err)
						continue
					}
					_, _ = fmt.Fprintf(GinkgoWriter, "[claim-failure] sandbox %s generation=%d status=%s\n", sbx.Name, sbx.Generation, status)
				}
			}
			pods := &corev1.PodList{}
			if err := k8sClient.List(diagnosticCtx, pods, &client.ListOptions{Namespace: namespace, Limit: 10}); err != nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "[claim-failure] list pods: %v\n", err)
			} else {
				for _, pod := range pods.Items {
					status, err := json.Marshal(pod.Status)
					if err != nil {
						_, _ = fmt.Fprintf(GinkgoWriter, "[claim-failure] encode pod %s status: %v\n", pod.Name, err)
						continue
					}
					_, _ = fmt.Fprintf(GinkgoWriter, "[claim-failure] pod %s uid=%s restartPolicy=%s templateHash=%s inplaceState=%s status=%s\n",
						pod.Name, pod.UID, pod.Spec.RestartPolicy, pod.Labels[agentsv1alpha1.PodLabelTemplateHash],
						pod.Annotations[inplaceupdate.PodAnnotationInPlaceUpdateStateKey], status)
					for _, container := range pod.Spec.Containers {
						_, _ = fmt.Fprintf(GinkgoWriter, "[claim-failure] pod %s container %s targetImage=%s\n", pod.Name, container.Name, container.Image)
					}
				}
			}
			events := &corev1.EventList{}
			if err := k8sClient.List(diagnosticCtx, events, &client.ListOptions{Namespace: namespace, Limit: 100}); err != nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "[claim-failure] list events: %v\n", err)
			} else {
				for _, event := range events.Items {
					_, _ = fmt.Fprintf(GinkgoWriter, "[claim-failure] event %s/%s type=%s reason=%s count=%d first=%s last=%s eventTime=%s message=%q\n",
						event.InvolvedObject.Kind, event.InvolvedObject.Name, event.Type, event.Reason, event.Count,
						event.FirstTimestamp.Time.Format(time.RFC3339), event.LastTimestamp.Time.Format(time.RFC3339),
						event.EventTime.Time.Format(time.RFC3339), event.Message)
				}
			}
			for _, pod := range pods.Items {
				statuses := append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
				for _, container := range statuses {
					for _, previous := range []bool{false, true} {
						if previous && container.RestartCount == 0 {
							continue
						}
						tailLines, limitBytes := int64(100), int64(8192)
						logCtx, logCancel := context.WithTimeout(diagnosticCtx, 3*time.Second)
						logs, err := clientset.CoreV1().Pods(namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
							Container: container.Name, Previous: previous, Timestamps: true,
							TailLines: &tailLines, LimitBytes: &limitBytes,
						}).DoRaw(logCtx)
						logCancel()
						if err != nil {
							_, _ = fmt.Fprintf(GinkgoWriter, "[claim-failure] pod %s container %s previous=%t logs: %v\n", pod.Name, container.Name, previous, err)
							continue
						}
						_, _ = fmt.Fprintf(GinkgoWriter, "[claim-failure] pod %s container %s previous=%t logs:\n%s\n", pod.Name, container.Name, previous, logs)
					}
				}
			}
		}
		ns := &corev1.Namespace{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: namespace}, ns); err == nil {
			if err := k8sClient.Delete(ctx, ns); err != nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "delete namespace %s: %v\n", namespace, err)
			}
		} else if client.IgnoreNotFound(err) != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "get namespace %s for cleanup: %v\n", namespace, err)
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
			if err := k8sClient.Update(ctx, latest); err != nil {
				return err
			}
			latest.DeepCopyInto(sbx)
			return nil
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

	// Wait for the current target to be accepted and delivered so an old Succeeded condition cannot falsely pass successive upgrades.
	waitInplaceUpdateSucceeded := func(sbx *agentsv1alpha1.Sandbox, timeout time.Duration) {
		generation := sbx.Generation
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, sbx)).To(Succeed())
			g.Expect(sbx.Status.ObservedGeneration).To(Equal(generation))
			cond := meta.FindStatusCondition(sbx.Status.Conditions, string(agentsv1alpha1.SandboxConditionInplaceUpdate))
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(cond.Reason).To(Equal(agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded))
			pod := &corev1.Pod{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, pod)).To(Succeed())
			g.Expect(sbx.Status.UpdateRevision).NotTo(BeEmpty())
			g.Expect(pod.Labels[agentsv1alpha1.PodLabelTemplateHash]).To(Equal(sbx.Status.UpdateRevision))
		}, timeout, time.Second).Should(Succeed())
	}

	waitReady := func(sbx *agentsv1alpha1.Sandbox) *metav1.Condition {
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, sbx)).To(Succeed())
			cond := meta.FindStatusCondition(sbx.Status.Conditions, string(agentsv1alpha1.SandboxConditionReady))
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(cond.Reason).To(Equal(agentsv1alpha1.SandboxReadyReasonPodReady))
		}, 3*time.Minute, time.Second).Should(Succeed())
		return meta.FindStatusCondition(sbx.Status.Conditions, string(agentsv1alpha1.SandboxConditionReady)).DeepCopy()
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
		Consistently(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, sbx)).To(Succeed())
			failed := meta.FindStatusCondition(sbx.Status.Conditions, string(agentsv1alpha1.SandboxConditionInplaceUpdate))
			ready := meta.FindStatusCondition(sbx.Status.Conditions, string(agentsv1alpha1.SandboxConditionReady))
			g.Expect(failed).NotTo(BeNil())
			g.Expect(failed.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(failed.Reason).To(Equal(agentsv1alpha1.SandboxInplaceUpdateReasonFailed))
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionFalse), "old PodReady must not reopen readiness after a failure")
			g.Expect(ready.Reason).To(Equal(agentsv1alpha1.SandboxInplaceUpdateReasonFailed))
			g.Expect(ready.Message).To(Equal(failed.Message))
		}, 5*time.Second, time.Second).Should(Succeed())
	}

	// =========================================================================
	// Success scenarios
	// =========================================================================

	Context("Success", func() {
		It("should update container image in place (claim path)", func() {
			sbx := newClaimSandbox(fmt.Sprintf("claim-img-ok-%d", time.Now().UnixNano()))
			Expect(k8sClient.Create(ctx, sbx)).To(Succeed())
			waitSandboxRunning(sbx)
			originalPod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, originalPod)).To(Succeed())

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
			waitInplacePodObserved(ctx, sbx, originalPod.UID, map[string]string{"test-container": updateImage}, nil)
			waitReady(sbx)
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

			originalPod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, originalPod)).To(Succeed())

			By("Updating sandbox template to resize CPU to 1/1 (still Burstable)")
			updateSandboxTemplate(sbx, func(spec *corev1.PodTemplateSpec) {
				spec.Spec.Containers[0].Resources = corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
				}
			})

			By("Verifying that kubelet applied the resources, not just the Pod spec")
			pod := waitInplacePodObserved(ctx, sbx, originalPod.UID,
				map[string]string{"test-container": initialImage},
				map[string]corev1.ResourceRequirements{"test-container": sbx.Spec.Template.Spec.Containers[0].Resources})
			waitInplaceUpdateSucceeded(sbx, 3*time.Minute)
			waitReady(sbx)

			By("Verifying QoS class remains Burstable")
			Expect(pod.Status.QOSClass).To(Equal(corev1.PodQOSBurstable))

			By("Verifying sandbox stays Running")
			Consistently(func() agentsv1alpha1.SandboxPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, sbx)
				return sbx.Status.Phase
			}, 10*time.Second, 2*time.Second).Should(Equal(agentsv1alpha1.SandboxRunning))
		})

		It("should restore Ready only after combined image and resource updates are applied", func() {
			sbx := newClaimSandbox(fmt.Sprintf("claim-combo-ok-%d", time.Now().UnixNano()))
			sbx.Spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
			}
			Expect(k8sClient.Create(ctx, sbx)).To(Succeed())
			waitSandboxRunning(sbx)
			originalPod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, originalPod)).To(Succeed())
			updateSandboxTemplate(sbx, func(spec *corev1.PodTemplateSpec) {
				spec.Spec.Containers[0].Image = updateImage
				spec.Spec.Containers[0].Resources = corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
				}
			})
			waitInplaceUpdateSucceeded(sbx, 3*time.Minute)
			pod := waitInplacePodObserved(ctx, sbx, originalPod.UID,
				map[string]string{"test-container": updateImage},
				map[string]corev1.ResourceRequirements{"test-container": sbx.Spec.Template.Spec.Containers[0].Resources})
			Expect(pod.Status.QOSClass).To(Equal(originalPod.Status.QOSClass))
			waitReady(sbx)
		})

		It("should preserve Ready without restarting containers after metadata-only success", func() {
			sbx := newClaimSandbox(fmt.Sprintf("claim-meta-ok-%d", time.Now().UnixNano()))
			Expect(k8sClient.Create(ctx, sbx)).To(Succeed())
			waitSandboxRunning(sbx)
			waitInplaceUpdateSucceeded(sbx, 3*time.Minute)
			originalReady := waitReady(sbx)
			originalPod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, originalPod)).To(Succeed())

			for _, round := range []string{"first", "second"} {
				By("Updating template labels and annotations in successive rounds: " + round)
				updateSandboxTemplate(sbx, func(spec *corev1.PodTemplateSpec) {
					spec.Labels = map[string]string{"updated-by": "claim-e2e-" + round}
					spec.Annotations = map[string]string{"e2e-metadata-round": round}
				})
				Eventually(func(g Gomega) {
					pod := &corev1.Pod{}
					g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, pod)).To(Succeed())
					g.Expect(pod.Labels["updated-by"]).To(Equal("claim-e2e-" + round))
					g.Expect(pod.Annotations["e2e-metadata-round"]).To(Equal(round))
				}, 2*time.Minute, time.Second).Should(Succeed())
				waitInplaceUpdateSucceeded(sbx, 3*time.Minute)
				Consistently(func(g Gomega) {
					g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, sbx)).To(Succeed())
					ready := meta.FindStatusCondition(sbx.Status.Conditions, string(agentsv1alpha1.SandboxConditionReady))
					g.Expect(ready).NotTo(BeNil())
					g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
					g.Expect(ready.LastTransitionTime).To(Equal(originalReady.LastTransitionTime), "Ready must not fluctuate during metadata updates")
					g.Expect(sbx.Status.Phase).To(Equal(agentsv1alpha1.SandboxRunning))
					pod := &corev1.Pod{}
					g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, pod)).To(Succeed())
					g.Expect(pod.UID).To(Equal(originalPod.UID))
					g.Expect(pod.Spec).To(Equal(originalPod.Spec))
					for i := range originalPod.Status.ContainerStatuses {
						before := originalPod.Status.ContainerStatuses[i]
						g.Expect(pod.Status.ContainerStatuses).To(ContainElement(And(
							HaveField("Name", before.Name), HaveField("ContainerID", before.ContainerID),
							HaveField("RestartCount", before.RestartCount))))
					}
				}, 5*time.Second, time.Second).Should(Succeed())
			}
		})
	})

	Context("Configuration completion does not imply Pod Ready", func() {
		It("should not publish Ready for a Succeeded metadata-only update while the Pod is unready", func() {
			gate := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "readiness-gate", Namespace: namespace},
				Data:       map[string]string{"ready": "true"},
			}
			Expect(k8sClient.Create(ctx, gate)).To(Succeed())
			sbx := newClaimSandbox(fmt.Sprintf("claim-ready-gate-%d", time.Now().UnixNano()))
			template := sbx.Spec.Template
			template.Spec.Volumes = append(template.Spec.Volumes, corev1.Volume{
				Name: "readiness-gate", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: gate.Name},
				}},
			})
			template.Spec.Containers[0].VolumeMounts = append(template.Spec.Containers[0].VolumeMounts,
				corev1.VolumeMount{Name: "readiness-gate", MountPath: "/mnt/readiness-gate", ReadOnly: true})
			template.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{
					"sh", "-c", `test "$(cat /mnt/readiness-gate/ready)" = true`,
				}}},
				PeriodSeconds: 1, FailureThreshold: 1,
			}
			Expect(k8sClient.Create(ctx, sbx)).To(Succeed())
			waitSandboxRunning(sbx)
			waitReady(sbx)
			originalPod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, originalPod)).To(Succeed())

			By("Disabling the real readiness probe and waiting for kubelet to report PodReady=False")
			gate.Data["ready"] = "false"
			Expect(k8sClient.Update(ctx, gate)).To(Succeed())
			Eventually(func(g Gomega) {
				pod := &corev1.Pod{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, pod)).To(Succeed())
				g.Expect(pod.Status.Conditions).To(ContainElement(And(
					HaveField("Type", corev1.PodReady), HaveField("Status", corev1.ConditionFalse))))
			}, 3*time.Minute, time.Second).Should(Succeed())

			By("Updating only metadata without publishing Ready=True before the Pod becomes ready")
			updateSandboxTemplate(sbx, func(spec *corev1.PodTemplateSpec) {
				spec.Annotations = map[string]string{"e2e-readiness": "blocked"}
			})
			waitInplaceUpdateSucceeded(sbx, 3*time.Minute)
			Consistently(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, sbx)).To(Succeed())
				ready := meta.FindStatusCondition(sbx.Status.Conditions, string(agentsv1alpha1.SandboxConditionReady))
				g.Expect(ready).NotTo(BeNil())
				g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
				cond := meta.FindStatusCondition(sbx.Status.Conditions, string(agentsv1alpha1.SandboxConditionInplaceUpdate))
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Reason).To(Equal(agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded))
			}, 5*time.Second, time.Second).Should(Succeed())
			waitInplacePodObserved(ctx, sbx, originalPod.UID, map[string]string{"test-container": initialImage}, nil)

			By("Restoring the probe so Ready recovers from the actual Pod status")
			gate.Data["ready"] = "true"
			Expect(k8sClient.Update(ctx, gate)).To(Succeed())
			waitReady(sbx)
		})
	})

	// =========================================================================
	// Resource overrides and unsupported template changes
	// =========================================================================

	Context("Resource overrides and terminal failures", func() {
		It("should preserve InplaceUpdate success for a covered resource downscale", func() {
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

			By("Verifying that the resource override target succeeds in the current round")
			waitInplaceUpdateSucceeded(sbx, 3*time.Minute)
			waitReady(sbx)

			By("Verifying sandbox stays Running")
			Eventually(func() agentsv1alpha1.SandboxPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, sbx)
				return sbx.Status.Phase
			}, 30*time.Second, time.Second).Should(Equal(agentsv1alpha1.SandboxRunning))

			By("Verifying pod QoS remains Guaranteed")
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, pod)).To(Succeed())
			Expect(pod.Status.QOSClass).To(Equal(corev1.PodQOSGuaranteed))
		})

		It("should keep Ready=False for unsupported command changes and recover after reverting them", func() {
			sbx := newClaimSandbox(fmt.Sprintf("claim-cmd-fail-%d", time.Now().UnixNano()))
			Expect(k8sClient.Create(ctx, sbx)).To(Succeed())
			waitSandboxRunning(sbx)

			By("Updating sandbox template command (hash-immutable field)")
			updateSandboxTemplate(sbx, func(spec *corev1.PodTemplateSpec) {
				spec.Spec.Containers[0].Command = []string{"/bin/bash", "-c", "sleep 3600"}
			})

			By("Verifying that the unsupported target fails and old PodReady cannot restore Sandbox readiness")
			waitInplaceUpdateFailed(sbx, 3*time.Minute)

			By("Verifying sandbox stays Running (delivered despite rejection)")
			Eventually(func() agentsv1alpha1.SandboxPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, sbx)
				return sbx.Status.Phase
			}, 30*time.Second, time.Second).Should(Equal(agentsv1alpha1.SandboxRunning))

			By("Verifying pod command was NOT changed (patch rejected before delivery)")
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, pod)).To(Succeed())
			Expect(pod.Spec.Containers[0].Command).To(Equal([]string{"/bin/bash", "-c", "sleep infinity"}),
				"pod command should remain original")
			Expect(pod.Status.Conditions).To(ContainElement(And(
				HaveField("Type", corev1.PodReady), HaveField("Status", corev1.ConditionTrue))))

			By("Reverting the invalid change and adding metadata to verify that a new revision clears the old Failed condition")
			updateSandboxTemplate(sbx, func(spec *corev1.PodTemplateSpec) {
				spec.Spec.Containers[0].Command = []string{"/bin/bash", "-c", "sleep infinity"}
				spec.Annotations = map[string]string{"e2e-recovered": "true"}
			})
			waitInplaceUpdateSucceeded(sbx, 3*time.Minute)
			waitReady(sbx)
			waitInplacePodObserved(ctx, sbx, pod.UID, map[string]string{"test-container": initialImage}, nil)
		})
	})

	// =========================================================================
	// Transient failure (no delivery, stays InplaceUpdating)
	// =========================================================================

	Context("Transient failure recovery", func() {
		It("should deliver when a corrected image supersedes a failed target", func() {
			sbx := newClaimSandbox(fmt.Sprintf("claim-badimg-%d", time.Now().UnixNano()))
			Expect(k8sClient.Create(ctx, sbx)).To(Succeed())
			waitSandboxRunning(sbx)
			originalPod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, originalPod)).To(Succeed())
			originalPodUID := originalPod.UID

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

			By("Verifying Ready condition stays False while the update is pending")
			readyCond := getReadyCondition(sbx)
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCond.Reason).To(Equal(agentsv1alpha1.SandboxReadyReasonInplaceUpdating))

			By("Correcting the template image with a new in-place target")
			updateSandboxTemplate(sbx, func(spec *corev1.PodTemplateSpec) {
				spec.Spec.Containers[0].Image = updateImage
			})
			waitPodImage(sbx, updateImage, 2*time.Minute)

			By("Verifying the corrected target completes on the same Pod")
			Eventually(func() string {
				cond := getInplaceUpdateCondition(sbx)
				if cond == nil {
					return ""
				}
				return string(cond.Status) + "/" + cond.Reason
			}, 2*time.Minute, time.Second).Should(Equal(string(metav1.ConditionTrue) + "/" + agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded))
			waitReady(sbx)
			waitInplacePodObserved(ctx, sbx, originalPodUID, map[string]string{"test-container": updateImage}, nil)
		})
	})

	Context("Successive upgrades preserve unfinished observations", func() {
		DescribeTable("should not succeed a new target while a bad image update remains unfinished", func(resize, completedRound bool) {
			sbx := newClaimSandbox(fmt.Sprintf("claim-rounds-%d", time.Now().UnixNano()))
			// Resize an independent healthy container so image pull failures do not prevent resource verification.
			sbx.Spec.Template.Spec.Containers = append(sbx.Spec.Template.Spec.Containers, corev1.Container{
				Name: "worker", Image: initialImage, Command: []string{"/bin/bash", "-c", "sleep infinity"},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				},
			})
			Expect(k8sClient.Create(ctx, sbx)).To(Succeed())
			waitSandboxRunning(sbx)
			waitInplaceUpdateSucceeded(sbx, 3*time.Minute)
			waitReady(sbx)
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, pod)).To(Succeed())
			uid := pod.UID
			goodImage := initialImage
			if completedRound {
				By("Completing an image upgrade first to verify that the old Succeeded condition does not leak into the next round")
				goodImage = updateImage
				updateSandboxTemplate(sbx, func(spec *corev1.PodTemplateSpec) { spec.Spec.Containers[0].Image = goodImage })
				waitInplaceUpdateSucceeded(sbx, 3*time.Minute)
				waitInplacePodObserved(ctx, sbx, uid, map[string]string{"test-container": goodImage}, nil)
				waitReady(sbx)
			}

			By("Using an unpullable image to create a deterministic unfinished upgrade")
			updateSandboxTemplate(sbx, func(spec *corev1.PodTemplateSpec) { spec.Spec.Containers[0].Image = badImage })
			waitPodImage(sbx, badImage, 2*time.Minute)
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, sbx)).To(Succeed())
				cond := meta.FindStatusCondition(sbx.Status.Conditions, string(agentsv1alpha1.SandboxConditionInplaceUpdate))
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Reason).To(Equal(agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating))
			}, 2*time.Minute, time.Second).Should(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, pod)).To(Succeed())
			previousAnnotation := pod.Annotations[inplaceupdate.PodAnnotationInPlaceUpdateStateKey]
			previousRevision := pod.Labels[agentsv1alpha1.PodLabelTemplateHash]

			By("Delivering new metadata or a healthy container resource target while the earlier image update remains unfinished")
			updateSandboxTemplate(sbx, func(spec *corev1.PodTemplateSpec) {
				spec.Annotations = map[string]string{"e2e-next-round": "delivered"}
				if resize {
					spec.Spec.Containers[1].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("200m")
				}
			})
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, pod)).To(Succeed())
				g.Expect(pod.Annotations["e2e-next-round"]).To(Equal("delivered"))
				g.Expect(pod.Labels[agentsv1alpha1.PodLabelTemplateHash]).NotTo(Equal(previousRevision))
				state, err := inplaceupdate.GetPodInPlaceUpdateState(pod)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(state).NotTo(BeNil())
				g.Expect(state.UpdateImages).To(BeTrue(), "new resource state must not discard earlier image observations")
				g.Expect(state.LastContainerStatuses["test-container"].TargetImage).To(Equal(badImage))
				if resize {
					g.Expect(state.UpdateResources).To(BeTrue())
					g.Expect(state.Revision).To(Equal(pod.Labels[agentsv1alpha1.PodLabelTemplateHash]))
				} else {
					g.Expect(pod.Annotations[inplaceupdate.PodAnnotationInPlaceUpdateStateKey]).To(Equal(previousAnnotation))
				}
			}, 2*time.Minute, time.Second).Should(Succeed())
			if resize {
				By("Waiting for the healthy container resources to be applied without ignoring the bad image")
				waitInplacePodObserved(ctx, sbx, uid, map[string]string{"worker": initialImage},
					map[string]corev1.ResourceRequirements{"worker": sbx.Spec.Template.Spec.Containers[1].Resources})
			}
			Consistently(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, sbx)).To(Succeed())
				cond := meta.FindStatusCondition(sbx.Status.Conditions, string(agentsv1alpha1.SandboxConditionInplaceUpdate))
				ready := meta.FindStatusCondition(sbx.Status.Conditions, string(agentsv1alpha1.SandboxConditionReady))
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(cond.Reason).To(Equal(agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating))
				g.Expect(ready).NotTo(BeNil())
				g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(ready.Reason).To(Equal(agentsv1alpha1.SandboxReadyReasonInplaceUpdating))
				g.Expect(sbx.Status.Phase).To(Equal(agentsv1alpha1.SandboxRunning))
			}, 10*time.Second, time.Second).Should(Succeed())

			By("Rolling back to a valid image while retaining new resources and metadata to restore delivery on the same Pod")
			updateSandboxTemplate(sbx, func(spec *corev1.PodTemplateSpec) { spec.Spec.Containers[0].Image = goodImage })
			waitInplaceUpdateSucceeded(sbx, 3*time.Minute)
			var expectedResources map[string]corev1.ResourceRequirements
			if resize {
				expectedResources = map[string]corev1.ResourceRequirements{"worker": sbx.Spec.Template.Spec.Containers[1].Resources}
			}
			waitInplacePodObserved(ctx, sbx, uid, map[string]string{"test-container": goodImage, "worker": initialImage}, expectedResources)
			waitReady(sbx)
		},
			Entry("unfinished image update followed by metadata-only", false, false),
			Entry("unfinished image update followed by a resize of another container", true, false),
			Entry("a successful round followed by an unfinished image update and a resize", true, true),
		)

		It("should preserve another unfinished container when rewriting one container image target", func() {
			sbx := newClaimSandbox(fmt.Sprintf("claim-multi-rounds-%d", time.Now().UnixNano()))
			sbx.Spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
			}
			sbx.Spec.Template.Spec.Containers = append(sbx.Spec.Template.Spec.Containers, corev1.Container{
				Name: "worker", Image: initialImage, Command: []string{"/bin/bash", "-c", "sleep infinity"},
			})
			Expect(k8sClient.Create(ctx, sbx)).To(Succeed())
			waitSandboxRunning(sbx)
			waitReady(sbx)
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, pod)).To(Succeed())
			uid := pod.UID
			updateSandboxTemplate(sbx, func(spec *corev1.PodTemplateSpec) {
				spec.Spec.Containers[0].Image = badImage
				spec.Spec.Containers[1].Image = badImage
			})
			waitPodImage(sbx, badImage, 2*time.Minute)
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, pod)).To(Succeed())
				state, err := inplaceupdate.GetPodInPlaceUpdateState(pod)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(state).NotTo(BeNil())
				g.Expect(state.LastContainerStatuses).To(HaveLen(2))
				g.Expect(state.LastContainerStatuses["worker"].TargetImage).To(Equal(badImage))
			}, 2*time.Minute, time.Second).Should(Succeed())

			By("Correcting only the main container and resizing it while the worker retains an unpullable target")
			updateSandboxTemplate(sbx, func(spec *corev1.PodTemplateSpec) {
				spec.Spec.Containers[0].Image = updateImage
				spec.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("200m")
			})
			waitInplacePodObserved(ctx, sbx, uid, map[string]string{"test-container": updateImage},
				map[string]corev1.ResourceRequirements{"test-container": sbx.Spec.Template.Spec.Containers[0].Resources})
			Consistently(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, pod)).To(Succeed())
				state, err := inplaceupdate.GetPodInPlaceUpdateState(pod)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(state).NotTo(BeNil())
				g.Expect(state.UpdateImages).To(BeTrue())
				g.Expect(state.LastContainerStatuses["test-container"].TargetImage).To(Equal(updateImage))
				g.Expect(state.LastContainerStatuses["worker"].TargetImage).To(Equal(badImage))
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, sbx)).To(Succeed())
				cond := meta.FindStatusCondition(sbx.Status.Conditions, string(agentsv1alpha1.SandboxConditionInplaceUpdate))
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(cond.Reason).To(Equal(agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating))
				ready := meta.FindStatusCondition(sbx.Status.Conditions, string(agentsv1alpha1.SandboxConditionReady))
				g.Expect(ready).NotTo(BeNil())
				g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			}, 10*time.Second, time.Second).Should(Succeed())

			By("Allowing overall success only after correcting the last container")
			updateSandboxTemplate(sbx, func(spec *corev1.PodTemplateSpec) { spec.Spec.Containers[1].Image = initialImage })
			waitInplaceUpdateSucceeded(sbx, 3*time.Minute)
			waitInplacePodObserved(ctx, sbx, uid,
				map[string]string{"test-container": updateImage, "worker": initialImage},
				map[string]corev1.ResourceRequirements{"test-container": sbx.Spec.Template.Spec.Containers[0].Resources})
			waitReady(sbx)
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

			By("Combining a new image with a QoS change that requires resizing, without using an overridden downscale target")
			updateSandboxTemplate(sbx, func(spec *corev1.PodTemplateSpec) {
				spec.Spec.Containers[0].Image = updateImage
				spec.Spec.Containers[0].Resources = corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("750m"), corev1.ResourceMemory: resource.MustParse("512Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi"),
					},
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

			By("Restoring valid resource targets to deliver the new image on the same Pod")
			originalResources := pod.Spec.Containers[0].Resources.DeepCopy()
			updateSandboxTemplate(sbx, func(spec *corev1.PodTemplateSpec) {
				spec.Spec.Containers[0].Resources = *originalResources
			})
			waitInplaceUpdateSucceeded(sbx, 3*time.Minute)
			waitInplacePodObserved(ctx, sbx, pod.UID, map[string]string{"test-container": updateImage}, nil)
			waitReady(sbx)
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

			By("Verifying that an invalid command in the combined update makes the entire round Failed/Ready=False")
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
