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
	"encoding/json"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func mustMarshalPatch(tmpl corev1.PodTemplateSpec) runtime.RawExtension {
	data, err := json.Marshal(tmpl)
	if err != nil {
		panic(err)
	}
	return runtime.RawExtension{Raw: data}
}

var _ = Describe("SandboxUpdateOps E2E", func() {
	var (
		ctx          = context.Background()
		namespace    string
		initialImage = "centos:7"
		updateImage  = "centos:8"
		badImage     = "centos:non-existent-tag-999"
		batchLabel   = "e2e-ops-batch"
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

	// newOpsSandbox creates a Sandbox suitable for SandboxUpdateOps testing.
	// It includes the runtime init container, envd volume, and the specified label for selector matching.
	newOpsSandbox := func(name, labelValue string, extraVolumes []corev1.Volume, extraMounts []corev1.VolumeMount) *agentsv1alpha1.Sandbox {
		alwaysRestart := corev1.ContainerRestartPolicyAlways
		mounts := []corev1.VolumeMount{
			{Name: "envd-volume", MountPath: "/mnt/envd"},
		}
		mounts = append(mounts, extraMounts...)

		volumes := []corev1.Volume{
			{
				Name:         "envd-volume",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			},
		}
		volumes = append(volumes, extraVolumes...)

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
					Type: agentsv1alpha1.SandboxUpgradePolicyRecreate,
				},
				EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							InitContainers: []corev1.Container{
								{
									Name:    "runtime",
									Image:   "openkruise/agent-runtime:v0.2.0",
									Command: []string{"sh", "/workspace/entrypoint.sh"},
									VolumeMounts: []corev1.VolumeMount{
										{Name: "envd-volume", MountPath: "/mnt/envd"},
									},
									Env: []corev1.EnvVar{
										{Name: "ENVD_DIR", Value: "/mnt/envd"},
									},
									RestartPolicy: &alwaysRestart,
								},
							},
							Containers: []corev1.Container{
								{
									Name:    "test-container",
									Image:   initialImage,
									Command: []string{"/bin/bash", "-c", "sleep infinity"},
									Env: []corev1.EnvVar{
										{Name: "ENVD_DIR", Value: "/mnt/envd"},
									},
									StartupProbe: &corev1.Probe{
										ProbeHandler: corev1.ProbeHandler{
											TCPSocket: &corev1.TCPSocketAction{
												Port: intstr.FromInt(49983),
											},
										},
										FailureThreshold:    20,
										InitialDelaySeconds: 3,
										PeriodSeconds:       1,
										SuccessThreshold:    1,
										TimeoutSeconds:      1,
									},
									VolumeMounts: mounts,
									Lifecycle: &corev1.Lifecycle{
										PostStart: &corev1.LifecycleHandler{
											Exec: &corev1.ExecAction{
												Command: []string{"sh", "/mnt/envd/envd-run.sh"},
											},
										},
									},
								},
							},
							Volumes:       volumes,
							RestartPolicy: corev1.RestartPolicyNever,
						},
					},
				},
			},
		}
	}

	// waitSandboxRunning waits until the given sandbox reaches Running phase.
	waitSandboxRunning := func(sbx *agentsv1alpha1.Sandbox) {
		Eventually(func() agentsv1alpha1.SandboxPhase {
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, sbx)
			return sbx.Status.Phase
		}, 3*time.Minute, 500*time.Millisecond).Should(Equal(agentsv1alpha1.SandboxRunning), func() string {
			msg := fmt.Sprintf("sandbox %s stuck in phase %s", sbx.Name, sbx.Status.Phase)
			pod := &corev1.Pod{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, pod); err == nil {
				msg += fmt.Sprintf("\npodPhase: %s, conditions: %v, containerStatuses: %v",
					pod.Status.Phase, pod.Status.Conditions, pod.Status.ContainerStatuses)
			}
			eventList := &corev1.EventList{}
			if err := k8sClient.List(ctx, eventList, client.InNamespace(sbx.Namespace)); err == nil {
				for _, evt := range eventList.Items {
					if evt.InvolvedObject.Name == sbx.Name {
						msg += fmt.Sprintf("\nevent: %s %s %s", evt.Reason, evt.Type, evt.Message)
					}
				}
			}
			return msg
		})
	}

	// waitOpsPhase waits for the SandboxUpdateOps to reach the desired phase.
	waitOpsPhase := func(ops *agentsv1alpha1.SandboxUpdateOps, phase agentsv1alpha1.SandboxUpdateOpsPhase, timeout time.Duration) {
		Eventually(func() agentsv1alpha1.SandboxUpdateOpsPhase {
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: ops.Name, Namespace: ops.Namespace}, ops)
			return ops.Status.Phase
		}, timeout, time.Second).Should(Equal(phase), func() string {
			return fmt.Sprintf("ops %s stuck in phase %s, status: replicas=%d updated=%d failed=%d updating=%d",
				ops.Name, ops.Status.Phase, ops.Status.Replicas, ops.Status.UpdatedReplicas,
				ops.Status.FailedReplicas, ops.Status.UpdatingReplicas)
		})
	}

	Context("Webhook Validation", func() {
		It("should reject Update that modifies Lifecycle field", func() {
			By("Creating a Sandbox and waiting for Running")
			sbx := newOpsSandbox(
				fmt.Sprintf("ops-validate-update-%d", time.Now().UnixNano()),
				"validate-update", nil, nil,
			)
			Expect(k8sClient.Create(ctx, sbx)).To(Succeed())
			waitSandboxRunning(sbx)

			By("Creating a SandboxUpdateOps with Lifecycle")
			ops := &agentsv1alpha1.SandboxUpdateOps{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("ops-validate-update-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{batchLabel: "validate-update"},
					},
					Patch: mustMarshalPatch(corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "test-container", Image: updateImage},
							},
						},
					}),
					Lifecycle: &agentsv1alpha1.SandboxLifecycle{
						PreUpgrade: &agentsv1alpha1.UpgradeAction{
							Exec:           &corev1.ExecAction{Command: []string{"/bin/bash", "-c", "exit 0"}},
							TimeoutSeconds: 30,
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, ops)).To(Succeed())
			klog.InfoS("Created SandboxUpdateOps", "name", ops.Name)

			By("Waiting for Ops to reach Completed")
			waitOpsPhase(ops, agentsv1alpha1.SandboxUpdateOpsCompleted, 5*time.Minute)

			By("Attempting to Update Lifecycle field - should be rejected")
			latest := &agentsv1alpha1.SandboxUpdateOps{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ops.Name, Namespace: ops.Namespace}, latest)).To(Succeed())
			latest.Spec.Lifecycle = &agentsv1alpha1.SandboxLifecycle{
				PostUpgrade: &agentsv1alpha1.UpgradeAction{
					Exec:           &corev1.ExecAction{Command: []string{"/bin/bash", "-c", "echo changed"}},
					TimeoutSeconds: 30,
				},
			}
			err := k8sClient.Update(ctx, latest)
			Expect(err).To(HaveOccurred())
			klog.InfoS("Update correctly rejected", "error", err.Error())
			Expect(errors.IsInvalid(err) || errors.IsForbidden(err)).To(BeTrue(),
				"expected Invalid or Forbidden error, got: %v", err)
		})
	})

	Context("Batch Upgrade", func() {
		It("should upgrade 2 sandboxes with MaxUnavailable=1 and lifecycle hooks", func() {
			labelValue := fmt.Sprintf("batch-2-%d", time.Now().UnixNano())
			extraVolumes := []corev1.Volume{
				{Name: "volume1", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "volume2", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			}
			extraMounts := []corev1.VolumeMount{
				{Name: "volume1", MountPath: "/mnt/volume1"},
				{Name: "volume2", MountPath: "/mnt/volume2"},
			}

			By("Creating 2 Sandboxes and waiting for all Running")
			sandboxes := make([]*agentsv1alpha1.Sandbox, 2)
			for i := 0; i < 2; i++ {
				sandboxes[i] = newOpsSandbox(
					fmt.Sprintf("ops-batch-%s-%d", labelValue[:10], i),
					labelValue, extraVolumes, extraMounts,
				)
				Expect(k8sClient.Create(ctx, sandboxes[i])).To(Succeed())
			}
			for i := 0; i < 2; i++ {
				waitSandboxRunning(sandboxes[i])
				klog.InfoS("Sandbox is Running", "name", sandboxes[i].Name, "index", i)
			}

			By("Creating SandboxUpdateOps with MaxUnavailable=1 and lifecycle hooks to update image, remove volume2, add volume3")
			maxUnavailable := intstr.FromInt(1)
			ops := &agentsv1alpha1.SandboxUpdateOps{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("ops-batch-%s", labelValue[:10]),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{batchLabel: labelValue},
					},
					UpdateStrategy: agentsv1alpha1.SandboxUpdateOpsStrategy{
						MaxUnavailable: &maxUnavailable,
					},
					Lifecycle: &agentsv1alpha1.SandboxLifecycle{
						PreUpgrade: &agentsv1alpha1.UpgradeAction{
							Exec:           &corev1.ExecAction{Command: []string{"/bin/bash", "-c", "exit 0"}},
							TimeoutSeconds: 30,
						},
						PostUpgrade: &agentsv1alpha1.UpgradeAction{
							Exec:           &corev1.ExecAction{Command: []string{"/bin/bash", "-c", "exit 0"}},
							TimeoutSeconds: 30,
						},
					},
					Patch: runtime.RawExtension{Raw: []byte(`{
						"spec": {
							"containers": [{
								"name": "test-container",
								"image": "` + updateImage + `",
								"volumeMounts": [
									{"$patch": "delete", "mountPath": "/mnt/volume2"},
									{"name": "volume3", "mountPath": "/mnt/volume3"}
								]
							}],
							"volumes": [
								{"$patch": "delete", "name": "volume2"},
								{"name": "volume3", "emptyDir": {}}
							]
						}
					}`)},
				},
			}
			Expect(k8sClient.Create(ctx, ops)).To(Succeed())
			klog.InfoS("Created batch SandboxUpdateOps", "name", ops.Name)

			By("Waiting for Ops to reach Completed")
			waitOpsPhase(ops, agentsv1alpha1.SandboxUpdateOpsCompleted, 3*time.Minute)

			By("Verifying Ops status counts")
			Expect(ops.Status.Replicas).To(Equal(int32(2)))
			Expect(ops.Status.UpdatedReplicas).To(Equal(int32(2)))
			klog.InfoS("Ops status verified", "replicas", ops.Status.Replicas, "updated", ops.Status.UpdatedReplicas)

			By("Verifying each Sandbox's Pod has new image, volume1, volume3, and no volume2")
			for _, sbx := range sandboxes {
				pod := &corev1.Pod{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, pod)).To(Succeed())
				Expect(pod.Spec.Containers[0].Image).To(Equal(updateImage),
					"sandbox %s should have updated image", sbx.Name)

				mountNames := map[string]bool{}
				for _, m := range pod.Spec.Containers[0].VolumeMounts {
					mountNames[m.Name] = true
				}
				Expect(mountNames).To(HaveKey("volume1"), "sandbox %s should have volume1 mount", sbx.Name)
				Expect(mountNames).To(HaveKey("volume3"), "sandbox %s should have volume3 mount", sbx.Name)
				Expect(mountNames).NotTo(HaveKey("volume2"), "sandbox %s should not have volume2 mount", sbx.Name)

				volNames := map[string]bool{}
				for _, v := range pod.Spec.Volumes {
					volNames[v.Name] = true
				}
				Expect(volNames).To(HaveKey("volume1"), "sandbox %s should have volume1", sbx.Name)
				Expect(volNames).To(HaveKey("volume3"), "sandbox %s should have volume3", sbx.Name)
				Expect(volNames).NotTo(HaveKey("volume2"), "sandbox %s should not have volume2", sbx.Name)
				klog.InfoS("Sandbox pod verified", "name", sbx.Name, "image", pod.Spec.Containers[0].Image)
			}

			By("Deleting SandboxUpdateOps and verifying label cleanup")
			Expect(k8sClient.Delete(ctx, ops)).To(Succeed())
			klog.InfoS("Deleted SandboxUpdateOps", "name", ops.Name)

			// Wait for ops to be fully deleted (finalizer should clean up first)
			Eventually(func(g Gomega) {
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(ops), ops)
				g.Expect(errors.IsNotFound(err)).To(BeTrue())
			}, 30*time.Second, time.Second).Should(Succeed())
			klog.InfoS("SandboxUpdateOps fully deleted", "name", ops.Name)

			By("Verifying sandbox labels are cleaned up")
			for _, sbx := range sandboxes {
				updated := &agentsv1alpha1.Sandbox{}
				Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(sbx), updated)).To(Succeed())
				Expect(updated.Labels).NotTo(HaveKey(agentsv1alpha1.LabelSandboxUpdateOps))
				klog.InfoS("Sandbox label verified clean", "sandbox", updated.Name)
			}
		})

		It("should handle preUpgrade failure with MaxUnavailable=1 and recover after delete recreate", func() {
			labelValue := fmt.Sprintf("batch-fail-%d", time.Now().UnixNano())

			By("Creating 2 Sandboxes and waiting for all Running")
			sandboxes := make([]*agentsv1alpha1.Sandbox, 2)
			for i := 0; i < 2; i++ {
				sandboxes[i] = newOpsSandbox(
					fmt.Sprintf("ops-fail-%s-%d", labelValue[:10], i),
					labelValue, nil, nil,
				)
				Expect(k8sClient.Create(ctx, sandboxes[i])).To(Succeed())
			}
			for i := 0; i < 2; i++ {
				waitSandboxRunning(sandboxes[i])
				klog.InfoS("Sandbox is Running", "name", sandboxes[i].Name, "index", i)
			}

			By("Creating SandboxUpdateOps with preUpgrade=exit 1 and MaxUnavailable=1")
			maxUnavailable := intstr.FromInt(1)
			ops1 := &agentsv1alpha1.SandboxUpdateOps{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("ops-fail-1-%s", labelValue[:10]),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{batchLabel: labelValue},
					},
					UpdateStrategy: agentsv1alpha1.SandboxUpdateOpsStrategy{
						MaxUnavailable: &maxUnavailable,
					},
					Patch: mustMarshalPatch(corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "test-container", Image: updateImage},
							},
						},
					}),
					Lifecycle: &agentsv1alpha1.SandboxLifecycle{
						PreUpgrade: &agentsv1alpha1.UpgradeAction{
							Exec:           &corev1.ExecAction{Command: []string{"/bin/bash", "-c", "exit 1"}},
							TimeoutSeconds: 30,
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, ops1)).To(Succeed())
			klog.InfoS("Created failing Ops", "name", ops1.Name)

			By("Finding the sandbox that was attempted and verifying PreUpgradeFailed")
			var failedSbx *agentsv1alpha1.Sandbox
			Eventually(func(g Gomega) {
				for _, sbx := range sandboxes {
					updated := &agentsv1alpha1.Sandbox{}
					g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(sbx), updated)).To(Succeed())
					if updated.Labels[agentsv1alpha1.LabelSandboxUpdateOps] == ops1.Name {
						failedSbx = updated
						break
					}
				}
				g.Expect(failedSbx).NotTo(BeNil(), "should find a sandbox with ops label")
				g.Expect(failedSbx.Status.Phase).To(Equal(agentsv1alpha1.SandboxUpgrading),
					"failed sandbox should be in Upgrading phase")
				var upgradingCond *metav1.Condition
				for i := range failedSbx.Status.Conditions {
					if failedSbx.Status.Conditions[i].Type == string(agentsv1alpha1.SandboxConditionUpgrading) {
						upgradingCond = &failedSbx.Status.Conditions[i]
						break
					}
				}
				g.Expect(upgradingCond).NotTo(BeNil(), "should have Upgrading condition")
				g.Expect(upgradingCond.Reason).To(Equal(agentsv1alpha1.SandboxUpgradingReasonPreUpgradeFailed),
					"Upgrading condition reason should be PreUpgradeFailed")
			}, 30*time.Second, time.Second).Should(Succeed())
			klog.InfoS("Failed sandbox state verified",
				"sandbox", failedSbx.Name, "phase", failedSbx.Status.Phase)

			By("Verifying MaxUnavailable=1 limited the blast radius")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ops1.Name, Namespace: ops1.Namespace}, ops1)).To(Succeed())
			Expect(ops1.Status.Phase).To(Equal(agentsv1alpha1.SandboxUpdateOpsUpdating),
				"ops should remain in Updating phase since failed occupies maxUnavailable quota")
			Expect(ops1.Status.Replicas).To(Equal(int32(2)),
				"total replicas should be 2")
			Expect(ops1.Status.FailedReplicas).To(Equal(int32(1)),
				"only 1 sandbox should fail with MaxUnavailable=1")
			Expect(ops1.Status.UpdatingReplicas).To(Equal(int32(0)),
				"no sandbox should be actively updating")
			Expect(ops1.Status.UpdatedReplicas).To(Equal(int32(0)),
				"no sandbox should have been successfully updated")
			klog.InfoS("Ops status verified",
				"phase", ops1.Status.Phase,
				"replicas", ops1.Status.Replicas,
				"failedReplicas", ops1.Status.FailedReplicas,
				"updatingReplicas", ops1.Status.UpdatingReplicas,
				"updatedReplicas", ops1.Status.UpdatedReplicas)

			By("Deleting the failed SandboxUpdateOps")
			Expect(k8sClient.Delete(ctx, ops1)).To(Succeed())
			Eventually(func() bool {
				return errors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: ops1.Name, Namespace: ops1.Namespace}, ops1))
			}, 30*time.Second, time.Second).Should(BeTrue())

			By("Verifying sandbox LabelSandboxUpdateOps is cleaned up after ops deletion")
			for _, sbx := range sandboxes {
				updated := &agentsv1alpha1.Sandbox{}
				Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(sbx), updated)).To(Succeed())
				Expect(updated.Labels).NotTo(HaveKey(agentsv1alpha1.LabelSandboxUpdateOps),
					"sandbox %s should not have ops label after ops deletion", sbx.Name)
			}
			klog.InfoS("Sandbox ops labels verified clean after failed ops deletion")

			By("Creating new SandboxUpdateOps with fixed preUpgrade=exit 0")
			ops2 := &agentsv1alpha1.SandboxUpdateOps{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("ops-fail-2-%s", labelValue[:10]),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{batchLabel: labelValue},
					},
					Patch: mustMarshalPatch(corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "test-container", Image: updateImage},
							},
						},
					}),
					Lifecycle: &agentsv1alpha1.SandboxLifecycle{
						PreUpgrade: &agentsv1alpha1.UpgradeAction{
							Exec:           &corev1.ExecAction{Command: []string{"/bin/bash", "-c", "exit 0"}},
							TimeoutSeconds: 30,
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, ops2)).To(Succeed())
			klog.InfoS("Created recovery Ops", "name", ops2.Name)

			By("Waiting for recovery Ops to reach Completed")
			waitOpsPhase(ops2, agentsv1alpha1.SandboxUpdateOpsCompleted, 2*time.Minute)

			By("Verifying all 2 Sandboxes have been upgraded")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ops2.Name, Namespace: ops2.Namespace}, ops2)).To(Succeed())
			Expect(ops2.Status.UpdatedReplicas).To(Equal(int32(2)))
			klog.InfoS("Recovery Ops completed", "updated", ops2.Status.UpdatedReplicas)

			for _, sbx := range sandboxes {
				pod := &corev1.Pod{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, pod)).To(Succeed())
				Expect(pod.Spec.Containers[0].Image).To(Equal(updateImage),
					"sandbox %s should have updated image after recovery", sbx.Name)
			}
		})

		It("should include stderr in Upgrading Condition message when preUpgrade hook fails", func() {
			labelValue := fmt.Sprintf("batch-stderr-%d", time.Now().UnixNano())

			By("Creating a Sandbox and waiting for Running")
			sbx := newOpsSandbox(
				fmt.Sprintf("ops-stderr-%s-0", labelValue[:10]),
				labelValue, nil, nil,
			)
			Expect(k8sClient.Create(ctx, sbx)).To(Succeed())
			waitSandboxRunning(sbx)
			klog.InfoS("Sandbox is Running", "name", sbx.Name)

			By("Creating SandboxUpdateOps with preUpgrade hook that outputs to stderr and exits 1")
			const stderrMessage = "ERROR: something failed in preUpgrade hook"
			ops := &agentsv1alpha1.SandboxUpdateOps{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("ops-stderr-%s", labelValue[:10]),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{batchLabel: labelValue},
					},
					Patch: mustMarshalPatch(corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "test-container", Image: updateImage},
							},
						},
					}),
					Lifecycle: &agentsv1alpha1.SandboxLifecycle{
						PreUpgrade: &agentsv1alpha1.UpgradeAction{
							Exec:           &corev1.ExecAction{Command: []string{"/bin/bash", "-c", fmt.Sprintf("echo \"%s\" >&2; exit 1", stderrMessage)}},
							TimeoutSeconds: 30,
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, ops)).To(Succeed())
			klog.InfoS("Created SandboxUpdateOps with stderr hook", "name", ops.Name)

			By("Verifying Sandbox Upgrading Condition contains stderr message")
			Eventually(func(g Gomega) {
				updated := &agentsv1alpha1.Sandbox{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(sbx), updated)).To(Succeed())
				g.Expect(updated.Status.Phase).To(Equal(agentsv1alpha1.SandboxUpgrading),
					"sandbox should be in Upgrading phase")

				var upgradingCond *metav1.Condition
				for i := range updated.Status.Conditions {
					if updated.Status.Conditions[i].Type == string(agentsv1alpha1.SandboxConditionUpgrading) {
						upgradingCond = &updated.Status.Conditions[i]
						break
					}
				}
				g.Expect(upgradingCond).NotTo(BeNil(), "should have Upgrading condition")
				g.Expect(upgradingCond.Reason).To(Equal(agentsv1alpha1.SandboxUpgradingReasonPreUpgradeFailed),
					"condition reason should be PreUpgradeFailed")
				g.Expect(upgradingCond.Message).To(ContainSubstring(stderrMessage),
					"condition message should contain stderr output, got: %s", upgradingCond.Message)
				klog.InfoS("Verified stderr in condition message",
					"sandbox", updated.Name, "message", upgradingCond.Message)
			}, 60*time.Second, time.Second).Should(Succeed())

			By("Verifying message truncation for long stderr output")
			// Create another sandbox to test truncation with a long stderr message
			longLabelValue := fmt.Sprintf("batch-long-%d", time.Now().UnixNano())
			sbxLong := newOpsSandbox(
				fmt.Sprintf("ops-long-%s-0", longLabelValue[:10]),
				longLabelValue, nil, nil,
			)
			Expect(k8sClient.Create(ctx, sbxLong)).To(Succeed())
			waitSandboxRunning(sbxLong)

			// Generate a long stderr message (> 1024 chars, the default MaxConditionMessageLen)
			longMsg := strings.Repeat("X", 1100)
			opsLong := &agentsv1alpha1.SandboxUpdateOps{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("ops-long-%s", longLabelValue[:10]),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{batchLabel: longLabelValue},
					},
					Patch: mustMarshalPatch(corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "test-container", Image: updateImage},
							},
						},
					}),
					Lifecycle: &agentsv1alpha1.SandboxLifecycle{
						PreUpgrade: &agentsv1alpha1.UpgradeAction{
							Exec:           &corev1.ExecAction{Command: []string{"/bin/bash", "-c", fmt.Sprintf("echo \"%s\" >&2; exit 1", longMsg)}},
							TimeoutSeconds: 30,
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, opsLong)).To(Succeed())
			klog.InfoS("Created SandboxUpdateOps with long stderr", "name", opsLong.Name)

			Eventually(func(g Gomega) {
				updated := &agentsv1alpha1.Sandbox{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(sbxLong), updated)).To(Succeed())
				g.Expect(updated.Status.Phase).To(Equal(agentsv1alpha1.SandboxUpgrading))

				var upgradingCond *metav1.Condition
				for i := range updated.Status.Conditions {
					if updated.Status.Conditions[i].Type == string(agentsv1alpha1.SandboxConditionUpgrading) {
						upgradingCond = &updated.Status.Conditions[i]
						break
					}
				}
				g.Expect(upgradingCond).NotTo(BeNil())
				g.Expect(upgradingCond.Reason).To(Equal(agentsv1alpha1.SandboxUpgradingReasonPreUpgradeFailed))
				// Verify message is truncated: should end with "..." and be at most 1024+3 chars
				g.Expect(len(upgradingCond.Message) <= 1027).To(BeTrue(),
					"condition message should be truncated to at most 1024+3 chars, got %d: %s",
					len(upgradingCond.Message), upgradingCond.Message)
				g.Expect(upgradingCond.Message).To(HaveSuffix("..."),
					"truncated message should end with '...', got: %s", upgradingCond.Message)
				klog.InfoS("Verified truncated condition message",
					"sandbox", updated.Name, "messageLen", len(upgradingCond.Message))
			}, 60*time.Second, time.Second).Should(Succeed())
		})

		It("should handle bad image failure with MaxUnavailable=1 and recover after delete recreate", func() {
			labelValue := fmt.Sprintf("batchimg%d", time.Now().UnixNano())

			By("Creating 2 Sandboxes and waiting for all Running")
			sandboxes := make([]*agentsv1alpha1.Sandbox, 2)
			for i := 0; i < 2; i++ {
				sandboxes[i] = newOpsSandbox(
					fmt.Sprintf("ops-img-%s-%d", labelValue[:10], i),
					labelValue, nil, nil,
				)
				Expect(k8sClient.Create(ctx, sandboxes[i])).To(Succeed())
			}
			for i := 0; i < 2; i++ {
				waitSandboxRunning(sandboxes[i])
				klog.InfoS("Sandbox is Running", "name", sandboxes[i].Name, "index", i)
			}

			By("Creating SandboxUpdateOps with bad image and MaxUnavailable=1")
			maxUnavailable := intstr.FromInt(1)
			ops1 := &agentsv1alpha1.SandboxUpdateOps{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("ops-img-1-%s", labelValue[:10]),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{batchLabel: labelValue},
					},
					UpdateStrategy: agentsv1alpha1.SandboxUpdateOpsStrategy{
						MaxUnavailable: &maxUnavailable,
					},
					Patch: mustMarshalPatch(corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "test-container", Image: badImage},
							},
						},
					}),
				},
			}
			Expect(k8sClient.Create(ctx, ops1)).To(Succeed())
			klog.InfoS("Created bad-image Ops", "name", ops1.Name)

			By("Finding the sandbox that was attempted and verifying UpgradePodFailed")
			var failedSbx *agentsv1alpha1.Sandbox
			Eventually(func(g Gomega) {
				for _, sbx := range sandboxes {
					updated := &agentsv1alpha1.Sandbox{}
					g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(sbx), updated)).To(Succeed())
					if updated.Labels[agentsv1alpha1.LabelSandboxUpdateOps] == ops1.Name {
						failedSbx = updated
						break
					}
				}
				g.Expect(failedSbx).NotTo(BeNil(), "should find a sandbox with ops label")
				g.Expect(failedSbx.Status.Phase).To(Equal(agentsv1alpha1.SandboxUpgrading),
					"failed sandbox should be in Upgrading phase")
				var upgradingCond *metav1.Condition
				for i := range failedSbx.Status.Conditions {
					if failedSbx.Status.Conditions[i].Type == string(agentsv1alpha1.SandboxConditionUpgrading) {
						upgradingCond = &failedSbx.Status.Conditions[i]
						break
					}
				}
				g.Expect(upgradingCond).NotTo(BeNil(), "should have Upgrading condition")
				g.Expect(upgradingCond.Reason).To(Equal(agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed),
					"Upgrading condition reason should be UpgradePodFailed")
			}, 3*time.Minute, time.Second).Should(Succeed())
			klog.InfoS("Failed sandbox state verified",
				"sandbox", failedSbx.Name, "phase", failedSbx.Status.Phase)

			By("Verifying ops status with MaxUnavailable=1")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ops1.Name, Namespace: ops1.Namespace}, ops1)).To(Succeed())
			Expect(ops1.Status.Phase).To(Equal(agentsv1alpha1.SandboxUpdateOpsUpdating),
				"ops should remain in Updating phase since failed occupies maxUnavailable quota")
			Expect(ops1.Status.Replicas).To(Equal(int32(2)))
			Expect(ops1.Status.FailedReplicas).To(Equal(int32(1)))
			Expect(ops1.Status.UpdatingReplicas).To(Equal(int32(0)))
			Expect(ops1.Status.UpdatedReplicas).To(Equal(int32(0)))
			klog.InfoS("Ops status verified",
				"phase", ops1.Status.Phase,
				"replicas", ops1.Status.Replicas,
				"failedReplicas", ops1.Status.FailedReplicas,
				"updatingReplicas", ops1.Status.UpdatingReplicas,
				"updatedReplicas", ops1.Status.UpdatedReplicas)

			By("Deleting the failed SandboxUpdateOps")
			Expect(k8sClient.Delete(ctx, ops1)).To(Succeed())
			Eventually(func() bool {
				return errors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: ops1.Name, Namespace: ops1.Namespace}, ops1))
			}, 30*time.Second, time.Second).Should(BeTrue())

			By("Verifying sandbox LabelSandboxUpdateOps is cleaned up after ops deletion")
			for _, sbx := range sandboxes {
				updated := &agentsv1alpha1.Sandbox{}
				Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(sbx), updated)).To(Succeed())
				Expect(updated.Labels).NotTo(HaveKey(agentsv1alpha1.LabelSandboxUpdateOps),
					"sandbox %s should not have ops label after ops deletion", sbx.Name)
			}
			klog.InfoS("Sandbox ops labels verified clean after bad-image ops deletion")

			By("Creating new SandboxUpdateOps with correct image")
			ops2 := &agentsv1alpha1.SandboxUpdateOps{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("ops-img-2-%s", labelValue[:10]),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{batchLabel: labelValue},
					},
					Patch: mustMarshalPatch(corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "test-container", Image: updateImage},
							},
						},
					}),
				},
			}
			Expect(k8sClient.Create(ctx, ops2)).To(Succeed())
			klog.InfoS("Created recovery Ops", "name", ops2.Name)

			By("Waiting for recovery Ops to reach Completed")
			waitOpsPhase(ops2, agentsv1alpha1.SandboxUpdateOpsCompleted, 2*time.Minute)

			By("Verifying all 2 Sandboxes have been upgraded")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ops2.Name, Namespace: ops2.Namespace}, ops2)).To(Succeed())
			Expect(ops2.Status.UpdatedReplicas).To(Equal(int32(2)))
			klog.InfoS("Recovery Ops completed", "updated", ops2.Status.UpdatedReplicas)

			for _, sbx := range sandboxes {
				pod := &corev1.Pod{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: sbx.Namespace}, pod)).To(Succeed())
				Expect(pod.Spec.Containers[0].Image).To(Equal(updateImage),
					"sandbox %s should have updated image after recovery", sbx.Name)
			}
		})
	})
})
