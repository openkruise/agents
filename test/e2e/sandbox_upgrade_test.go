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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

var _ = PDescribe("Sandbox Upgrade Lifecycle", func() {
	var (
		ctx          = context.Background()
		namespace    string
		initialImage string
		updateImage  string
		failedImage  string
	)

	BeforeEach(func() {
		namespace = createNamespace(ctx)
		initialImage = "centos:7"
		updateImage = "centos:8"
		failedImage = "nginx:alpine3.20"
	})

	AfterEach(func() {
		// Clean up namespace
		ns := &corev1.Namespace{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: namespace}, ns); err == nil {
			_ = k8sClient.Delete(ctx, ns)
		}
	})

	// newUpgradeSandbox creates a Sandbox with optional lifecycle hooks.
	newUpgradeSandbox := func(preScript, postScript string) *agentsv1alpha1.Sandbox {
		alwaysRestart := corev1.ContainerRestartPolicyAlways
		sbx := &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("upgrade-sandbox-%d", time.Now().UnixNano()),
				Namespace: namespace,
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
									Command: []string{"sh", "/workspace/entrypoint_inner.sh"},
									VolumeMounts: []corev1.VolumeMount{
										{
											Name:      "envd-volume",
											MountPath: "/mnt/envd",
										},
									},
									Env: []corev1.EnvVar{
										{
											Name:  "ENVD_DIR",
											Value: "/mnt/envd",
										},
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
										{
											Name:  "ENVD_DIR",
											Value: "/mnt/envd",
										},
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
									VolumeMounts: []corev1.VolumeMount{
										{
											Name:      "envd-volume",
											MountPath: "/mnt/envd",
										},
									},
									Lifecycle: &corev1.Lifecycle{
										PostStart: &corev1.LifecycleHandler{
											Exec: &corev1.ExecAction{
												Command: []string{"sh", "/mnt/envd/envd-run.sh"},
											},
										},
									},
								},
							},
							Volumes: []corev1.Volume{
								{
									Name: "envd-volume",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
							},
							RestartPolicy: corev1.RestartPolicyNever,
						},
					},
				},
			},
		}

		var lifecycle *agentsv1alpha1.SandboxLifecycle
		if preScript != "" || postScript != "" {
			lifecycle = &agentsv1alpha1.SandboxLifecycle{}
			if preScript != "" {
				lifecycle.PreUpgrade = &agentsv1alpha1.UpgradeAction{
					Exec: &corev1.ExecAction{
						Command: []string{"/bin/bash", "-c", preScript},
					},
					TimeoutSeconds: 30,
				}
			}
			if postScript != "" {
				lifecycle.PostUpgrade = &agentsv1alpha1.UpgradeAction{
					Exec: &corev1.ExecAction{
						Command: []string{"/bin/bash", "-c", postScript},
					},
					TimeoutSeconds: 30,
				}
			}
		}
		sbx.Spec.Lifecycle = lifecycle
		return sbx
	}

	// waitForRunning waits until the sandbox reaches Running phase.
	waitForRunning := func(sandbox *agentsv1alpha1.Sandbox) {
		Eventually(func() agentsv1alpha1.SandboxPhase {
			_ = k8sClient.Get(ctx, types.NamespacedName{
				Name:      sandbox.Name,
				Namespace: sandbox.Namespace,
			}, sandbox)
			return sandbox.Status.Phase
		}, time.Minute*2, time.Millisecond*500).Should(Equal(agentsv1alpha1.SandboxRunning), func() string {
			// Only print debug info on failure
			msg := fmt.Sprintf("sandbox %s stuck in phase %s", sandbox.Name, sandbox.Status.Phase)
			pod := &corev1.Pod{}
			if err := k8sClient.Get(ctx, types.NamespacedName{
				Name:      sandbox.Name,
				Namespace: sandbox.Namespace,
			}, pod); err == nil {
				msg += fmt.Sprintf("\npodPhase: %s, podConditions: %v, containerStatuses: %v",
					pod.Status.Phase, pod.Status.Conditions, pod.Status.ContainerStatuses)
			}
			eventList := &corev1.EventList{}
			if err := k8sClient.List(ctx, eventList, client.InNamespace(sandbox.Namespace)); err == nil {
				for _, evt := range eventList.Items {
					if evt.InvolvedObject.Name == sandbox.Name {
						msg += fmt.Sprintf("\nevent: %s %s %s", evt.Reason, evt.Type, evt.Message)
					}
				}
			}
			return msg
		})
	}

	// triggerUpgrade updates the container image to trigger a recreate upgrade.
	triggerUpgrade := func(sandbox *agentsv1alpha1.Sandbox, newImage string) {
		latest := &agentsv1alpha1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
		}, latest)).To(Succeed())
		latest.Spec.Template.Spec.Containers[0].Image = newImage
		Expect(updateSandboxSpec(ctx, latest)).To(Succeed())
	}

	// getUpgradingCondition retrieves the Upgrading condition from sandbox status.
	getUpgradingCondition := func(sandbox *agentsv1alpha1.Sandbox) *metav1.Condition {
		_ = k8sClient.Get(ctx, types.NamespacedName{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
		}, sandbox)
		return utils.GetSandboxCondition(&sandbox.Status, string(agentsv1alpha1.SandboxConditionUpgrading))
	}

	It("should fail preUpgrade hook and block upgrade", func() {
		sandbox := newUpgradeSandbox("exit 1", "ls /")
		By("Creating sandbox with preUpgrade hook that fails")
		Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
		By("Waiting for sandbox to reach Running phase")
		waitForRunning(sandbox)

		By("Recording the initial pod image")
		initialPod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
		}, initialPod)).To(Succeed())
		Expect(initialPod.Spec.Containers[0].Image).To(Equal(initialImage))
		By("Triggering upgrade by changing container image")
		triggerUpgrade(sandbox, updateImage)

		By("Verifying sandbox phase transitions to Upgrading")
		Eventually(func() agentsv1alpha1.SandboxPhase {
			_ = k8sClient.Get(ctx, types.NamespacedName{
				Name:      sandbox.Name,
				Namespace: sandbox.Namespace,
			}, sandbox)
			return sandbox.Status.Phase
		}, time.Second*60, time.Millisecond*500).Should(Equal(agentsv1alpha1.SandboxUpgrading))

		By("Verifying Upgrading condition shows PreUpgrade failure")
		Eventually(func() string {
			cond := getUpgradingCondition(sandbox)
			if cond == nil {
				return ""
			}
			return cond.Reason
		}, time.Second*60, time.Millisecond*500).Should(Equal(agentsv1alpha1.SandboxUpgradingReasonPreUpgradeFailed))

		cond := getUpgradingCondition(sandbox)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Message).NotTo(BeEmpty())
		Expect(sandbox.Status.Phase).To(Equal(agentsv1alpha1.SandboxUpgrading))
		klog.InfoS("preUpgrade failure condition", "reason", cond.Reason, "message", cond.Message)

		By("Verifying old pod still exists with original image")
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
		}, pod)).To(Succeed())
		Expect(pod.Spec.Containers[0].Image).To(Equal(initialImage))

		By("Fixing preUpgrade script to a valid command")
		latest := &agentsv1alpha1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
		}, latest)).To(Succeed())
		latest.Spec.Lifecycle.PreUpgrade.Exec = &corev1.ExecAction{Command: []string{"/bin/bash", "-c", "ls /"}}
		Expect(updateSandboxSpec(ctx, latest)).To(Succeed())

		By("Waiting for upgrade to resume and complete after fixing preUpgrade script")
		Eventually(func() agentsv1alpha1.SandboxPhase {
			_ = k8sClient.Get(ctx, types.NamespacedName{
				Name:      sandbox.Name,
				Namespace: sandbox.Namespace,
			}, sandbox)
			return sandbox.Status.Phase
		}, time.Minute*3, time.Millisecond*500).Should(Equal(agentsv1alpha1.SandboxRunning))

		By("Verifying Ready condition is True after recovery")
		readyCond := utils.GetSandboxCondition(&sandbox.Status, string(agentsv1alpha1.SandboxConditionReady))
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))

		By("Verifying Upgrading condition is Succeeded after recovery")
		upgradeCond := utils.GetSandboxCondition(&sandbox.Status, string(agentsv1alpha1.SandboxConditionUpgrading))
		Expect(upgradeCond).NotTo(BeNil())
		Expect(upgradeCond.Status).To(Equal(metav1.ConditionTrue))
		Expect(upgradeCond.Reason).To(Equal(agentsv1alpha1.SandboxUpgradingReasonSucceeded))

		By("Verifying pod image is the new version after recovery")
		recoveredPod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
		}, recoveredPod)).To(Succeed())
		Expect(recoveredPod.Spec.Containers[0].Image).To(Equal(updateImage))
	})

	It("should stay upgrading when new pod fails to start", func() {
		sandbox := newUpgradeSandbox("exit 0", "exit 0")

		By("Creating sandbox with preUpgrade hook that succeeds")
		Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())

		By("Waiting for sandbox to reach Running phase")
		waitForRunning(sandbox)

		By("Triggering upgrade with an invalid image to cause pod start failure")
		triggerUpgrade(sandbox, failedImage)

		By("Verifying sandbox phase transitions to Upgrading")
		Eventually(func() agentsv1alpha1.SandboxPhase {
			_ = k8sClient.Get(ctx, types.NamespacedName{
				Name:      sandbox.Name,
				Namespace: sandbox.Namespace,
			}, sandbox)
			return sandbox.Status.Phase
		}, time.Second*60, time.Millisecond*500).Should(Equal(agentsv1alpha1.SandboxUpgrading))

		By("Verifying new pod exists with invalid image and is not ready")
		Eventually(func() bool {
			pod := &corev1.Pod{}
			err := k8sClient.Get(ctx, types.NamespacedName{
				Name:      sandbox.Name,
				Namespace: sandbox.Namespace,
			}, pod)
			if err != nil {
				return false
			}
			// Verify the pod has the new (invalid) image
			return pod.Spec.Containers[0].Image == failedImage
		}, time.Minute*2, time.Millisecond*500).Should(BeTrue())

		By("Verifying sandbox stays in Upgrading phase and does not reach PostUpgrade")
		Eventually(func() string {
			cond := getUpgradingCondition(sandbox)
			if cond == nil {
				return ""
			}
			return cond.Reason
		}, time.Minute*15, time.Second).Should(Equal(agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed))

		By("Fixing upgrade by changing to a valid compatible image and removing preUpgrade")
		latest := &agentsv1alpha1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
		}, latest)).To(Succeed())
		latest.Spec.Template.Spec.Containers[0].Image = updateImage
		latest.Spec.Lifecycle.PreUpgrade = nil
		Expect(updateSandboxSpec(ctx, latest)).To(Succeed())

		By("Waiting for upgrade to resume and complete after fixing image")
		Eventually(func() agentsv1alpha1.SandboxPhase {
			_ = k8sClient.Get(ctx, types.NamespacedName{
				Name:      sandbox.Name,
				Namespace: sandbox.Namespace,
			}, sandbox)
			return sandbox.Status.Phase
		}, time.Minute*3, time.Millisecond*500).Should(Equal(agentsv1alpha1.SandboxRunning))

		By("Verifying Ready condition is True after recovery")
		readyCond := utils.GetSandboxCondition(&sandbox.Status, string(agentsv1alpha1.SandboxConditionReady))
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))

		By("Verifying Upgrading condition is Succeeded after recovery")
		upgradeCond := getUpgradingCondition(sandbox)
		Expect(upgradeCond).NotTo(BeNil())
		Expect(upgradeCond.Status).To(Equal(metav1.ConditionTrue))
		Expect(upgradeCond.Reason).To(Equal(agentsv1alpha1.SandboxUpgradingReasonSucceeded))

		By("Verifying pod image is the new valid version after recovery")
		recoveredPod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
		}, recoveredPod)).To(Succeed())
		Expect(recoveredPod.Spec.Containers[0].Image).To(Equal(updateImage))
	})

	It("should fail postUpgrade hook and block completion", func() {
		sandbox := newUpgradeSandbox("exit 0", "exit 1")

		By("Creating sandbox with preUpgrade success and postUpgrade failure")
		Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())

		By("Waiting for sandbox to reach Running phase")
		waitForRunning(sandbox)

		By("Triggering upgrade by changing container image")
		triggerUpgrade(sandbox, updateImage)

		By("Verifying sandbox phase transitions to Upgrading")
		Eventually(func() agentsv1alpha1.SandboxPhase {
			_ = k8sClient.Get(ctx, types.NamespacedName{
				Name:      sandbox.Name,
				Namespace: sandbox.Namespace,
			}, sandbox)
			return sandbox.Status.Phase
		}, time.Second*60, time.Millisecond*500).Should(Equal(agentsv1alpha1.SandboxUpgrading))

		By("Verifying Upgrading condition shows PostUpgrade failure")
		Eventually(func() bool {
			cond := getUpgradingCondition(sandbox)
			if cond == nil {
				return false
			}
			return cond.Reason == agentsv1alpha1.SandboxUpgradingReasonPostUpgradeFailed && cond.Message != ""
		}, time.Minute*60, time.Millisecond*500).Should(BeTrue())

		cond := getUpgradingCondition(sandbox)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		klog.InfoS("postUpgrade failure condition", "reason", cond.Reason, "message", cond.Message)

		By("Recording pod UID before fixing postUpgrade")
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
		}, pod)).To(Succeed())
		Expect(pod.Spec.Containers[0].Image).To(Equal(updateImage))
		podUID := pod.UID

		By("Fixing postUpgrade script to a successful one")
		latest := &agentsv1alpha1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
		}, latest)).To(Succeed())
		latest.Spec.Lifecycle.PostUpgrade.Exec = &corev1.ExecAction{Command: []string{"/bin/bash", "-c", "exit 0"}}
		Expect(updateSandboxSpec(ctx, latest)).To(Succeed())

		By("Waiting for sandbox to reach Running phase after fixing postUpgrade")
		Eventually(func() agentsv1alpha1.SandboxPhase {
			_ = k8sClient.Get(ctx, types.NamespacedName{
				Name:      sandbox.Name,
				Namespace: sandbox.Namespace,
			}, sandbox)
			return sandbox.Status.Phase
		}, time.Minute*3, time.Millisecond*500).Should(Equal(agentsv1alpha1.SandboxRunning))

		By("Verifying Upgrading condition is Succeeded")
		upgradeCond := getUpgradingCondition(sandbox)
		Expect(upgradeCond).NotTo(BeNil())
		Expect(upgradeCond.Status).To(Equal(metav1.ConditionTrue))
		Expect(upgradeCond.Reason).To(Equal(agentsv1alpha1.SandboxUpgradingReasonSucceeded))

		By("Verifying pod UID remains the same (no pod recreation)")
		recoveredPod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
		}, recoveredPod)).To(Succeed())
		Expect(recoveredPod.UID).To(Equal(podUID))
	})

	It("should complete full upgrade lifecycle successfully", func() {
		sandbox := newUpgradeSandbox("echo pre && exit 0", "echo post && exit 0")

		By("Adding volume1 and volume2 with corresponding volumeMounts to sandbox")
		sandbox.Spec.Template.Spec.Volumes = append(sandbox.Spec.Template.Spec.Volumes,
			corev1.Volume{
				Name: "volume1",
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			},
			corev1.Volume{
				Name: "volume2",
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			},
		)
		sandbox.Spec.Template.Spec.Containers[0].VolumeMounts = append(sandbox.Spec.Template.Spec.Containers[0].VolumeMounts,
			corev1.VolumeMount{Name: "volume1", MountPath: "/mnt/volume1"},
			corev1.VolumeMount{Name: "volume2", MountPath: "/mnt/volume2"},
		)

		By("Creating sandbox with both preUpgrade and postUpgrade hooks that succeed")
		Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())

		By("Waiting for sandbox to reach Running phase")
		waitForRunning(sandbox)

		By("Recording initial update revision")
		Expect(sandbox.Status.UpdateRevision).NotTo(BeEmpty())
		initialRevision := sandbox.Status.UpdateRevision

		By("Triggering upgrade: change image, remove volume2, add volume3")
		latest := &agentsv1alpha1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
		}, latest)).To(Succeed())
		latest.Spec.Template.Spec.Containers[0].Image = updateImage
		// Replace volume2 with volume3 in volumeMounts
		var newMounts []corev1.VolumeMount
		for _, m := range latest.Spec.Template.Spec.Containers[0].VolumeMounts {
			if m.Name != "volume2" {
				newMounts = append(newMounts, m)
			}
		}
		newMounts = append(newMounts, corev1.VolumeMount{Name: "volume3", MountPath: "/mnt/volume3"})
		latest.Spec.Template.Spec.Containers[0].VolumeMounts = newMounts
		// Replace volume2 with volume3 in volumes
		var newVolumes []corev1.Volume
		for _, v := range latest.Spec.Template.Spec.Volumes {
			if v.Name != "volume2" {
				newVolumes = append(newVolumes, v)
			}
		}
		newVolumes = append(newVolumes, corev1.Volume{
			Name: "volume3",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		})
		latest.Spec.Template.Spec.Volumes = newVolumes
		Expect(updateSandboxSpec(ctx, latest)).To(Succeed())

		By("Verifying sandbox transitions through Upgrading phase")
		Eventually(func() agentsv1alpha1.SandboxPhase {
			_ = k8sClient.Get(ctx, types.NamespacedName{
				Name:      sandbox.Name,
				Namespace: sandbox.Namespace,
			}, sandbox)
			return sandbox.Status.Phase
		}, time.Second*60, time.Millisecond*500).Should(Equal(agentsv1alpha1.SandboxUpgrading))

		By("Waiting for full upgrade lifecycle to complete - back to Running")
		Eventually(func() agentsv1alpha1.SandboxPhase {
			_ = k8sClient.Get(ctx, types.NamespacedName{
				Name:      sandbox.Name,
				Namespace: sandbox.Namespace,
			}, sandbox)
			return sandbox.Status.Phase
		}, time.Minute*3, time.Millisecond*500).Should(Equal(agentsv1alpha1.SandboxRunning))

		By("Verifying Ready condition is True")
		readyCond := utils.GetSandboxCondition(&sandbox.Status, string(agentsv1alpha1.SandboxConditionReady))
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))

		By("Verifying Upgrading condition is Succeeded")
		upgradeCond := utils.GetSandboxCondition(&sandbox.Status, string(agentsv1alpha1.SandboxConditionUpgrading))
		Expect(upgradeCond).NotTo(BeNil())
		Expect(upgradeCond.Status).To(Equal(metav1.ConditionTrue))
		Expect(upgradeCond.Reason).To(Equal(agentsv1alpha1.SandboxUpgradingReasonSucceeded))

		By("Verifying update revision has changed")
		Expect(sandbox.Status.UpdateRevision).NotTo(Equal(initialRevision))

		By("Verifying pod has updated image, volume1, volume3 and no volume2")
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
		}, pod)).To(Succeed())
		Expect(pod.Spec.Containers[0].Image).To(Equal(updateImage))
		// Verify volume mounts: should have envd-volume, volume1, volume3 but not volume2
		mountNames := make(map[string]bool)
		for _, m := range pod.Spec.Containers[0].VolumeMounts {
			mountNames[m.Name] = true
		}
		Expect(mountNames).To(HaveKey("volume1"))
		Expect(mountNames).To(HaveKey("volume3"))
		Expect(mountNames).NotTo(HaveKey("volume2"))
		// Verify volumes: should have envd-volume, volume1, volume3 but not volume2
		volNames := make(map[string]bool)
		for _, v := range pod.Spec.Volumes {
			volNames[v.Name] = true
		}
		Expect(volNames).To(HaveKey("volume1"))
		Expect(volNames).To(HaveKey("volume3"))
		Expect(volNames).NotTo(HaveKey("volume2"))
	})

	// Rollback scenarios
	It("rollback: preUpgrade failed, rollback image to initial and remove lifecycle, pod UID unchanged", func() {
		sandbox := newUpgradeSandbox("exit 1", "exit 0")

		By("Creating sandbox with preUpgrade hook that fails")
		Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())

		By("Waiting for sandbox to reach Running phase")
		waitForRunning(sandbox)

		By("Recording initial pod UID")
		initialPod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
		}, initialPod)).To(Succeed())
		initialPodUID := initialPod.UID

		By("Triggering upgrade by changing container image")
		triggerUpgrade(sandbox, updateImage)

		By("Verifying sandbox enters Upgrading with PreUpgradeFailed")
		Eventually(func() string {
			cond := getUpgradingCondition(sandbox)
			if cond == nil {
				return ""
			}
			return cond.Reason
		}, time.Second*60, time.Millisecond*500).Should(Equal(agentsv1alpha1.SandboxUpgradingReasonPreUpgradeFailed))

		By("Rolling back: revert image to initial and remove lifecycle")
		latest := &agentsv1alpha1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
		}, latest)).To(Succeed())
		latest.Spec.Template.Spec.Containers[0].Image = initialImage
		latest.Spec.Lifecycle = nil
		Expect(updateSandboxSpec(ctx, latest)).To(Succeed())

		By("Waiting for sandbox to return to Running phase")
		Eventually(func() agentsv1alpha1.SandboxPhase {
			_ = k8sClient.Get(ctx, types.NamespacedName{
				Name:      sandbox.Name,
				Namespace: sandbox.Namespace,
			}, sandbox)
			return sandbox.Status.Phase
		}, time.Minute*3, time.Millisecond*500).Should(Equal(agentsv1alpha1.SandboxRunning))

		By("Verifying pod UID remains the same (no pod recreation)")
		rollbackPod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
		}, rollbackPod)).To(Succeed())
		Expect(rollbackPod.UID).To(Equal(initialPodUID))
		Expect(rollbackPod.Spec.Containers[0].Image).To(Equal(initialImage))
	})

	It("rollback: preUpgrade succeeded but pod start failed, rollback to initial image with postUpgrade success", func() {
		sandbox := newUpgradeSandbox("exit 0", "exit 0")

		By("Creating sandbox with preUpgrade and postUpgrade hooks that succeed")
		Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())

		By("Waiting for sandbox to reach Running phase")
		waitForRunning(sandbox)

		By("Triggering upgrade with incompatible image to cause pod start failure")
		triggerUpgrade(sandbox, failedImage)

		By("Verifying sandbox enters Upgrading with UpgradePodFailed")
		Eventually(func() string {
			cond := getUpgradingCondition(sandbox)
			if cond == nil {
				return ""
			}
			return cond.Reason
		}, time.Minute*3, time.Second).Should(Equal(agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed))

		By("Rolling back: revert image to initial and remove preUpgrade, keep postUpgrade")
		latest := &agentsv1alpha1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
		}, latest)).To(Succeed())
		latest.Spec.Template.Spec.Containers[0].Image = initialImage
		latest.Spec.Lifecycle.PreUpgrade = nil
		Expect(updateSandboxSpec(ctx, latest)).To(Succeed())

		By("Waiting for sandbox to return to Running phase after rollback")
		Eventually(func() agentsv1alpha1.SandboxPhase {
			_ = k8sClient.Get(ctx, types.NamespacedName{
				Name:      sandbox.Name,
				Namespace: sandbox.Namespace,
			}, sandbox)
			return sandbox.Status.Phase
		}, time.Minute*3, time.Millisecond*500).Should(Equal(agentsv1alpha1.SandboxRunning))

		By("Verifying Upgrading condition is Succeeded")
		upgradeCond := getUpgradingCondition(sandbox)
		Expect(upgradeCond).NotTo(BeNil())
		Expect(upgradeCond.Status).To(Equal(metav1.ConditionTrue))
		Expect(upgradeCond.Reason).To(Equal(agentsv1alpha1.SandboxUpgradingReasonSucceeded))

		By("Verifying pod image is the initial version after rollback")
		rollbackPod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
		}, rollbackPod)).To(Succeed())
		Expect(rollbackPod.Spec.Containers[0].Image).To(Equal(initialImage))
	})

	It("rollback: postUpgrade failed, rollback to initial image with postUpgrade fixed to success", func() {
		sandbox := newUpgradeSandbox("exit 0", "exit 1")

		By("Creating sandbox with preUpgrade success and postUpgrade failure")
		Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())

		By("Waiting for sandbox to reach Running phase")
		waitForRunning(sandbox)

		By("Triggering upgrade by changing container image")
		triggerUpgrade(sandbox, updateImage)

		By("Verifying sandbox enters Upgrading with PostUpgradeFailed")
		Eventually(func() string {
			cond := getUpgradingCondition(sandbox)
			if cond == nil {
				return ""
			}
			return cond.Reason
		}, time.Minute*60, time.Millisecond*500).Should(Equal(agentsv1alpha1.SandboxUpgradingReasonPostUpgradeFailed))

		By("Rolling back: revert image to initial, remove preUpgrade, fix postUpgrade to succeed")
		latest := &agentsv1alpha1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
		}, latest)).To(Succeed())
		latest.Spec.Template.Spec.Containers[0].Image = initialImage
		latest.Spec.Lifecycle.PreUpgrade = nil
		latest.Spec.Lifecycle.PostUpgrade.Exec = &corev1.ExecAction{Command: []string{"/bin/bash", "-c", "exit 0"}}
		Expect(updateSandboxSpec(ctx, latest)).To(Succeed())

		By("Waiting for sandbox to return to Running phase after rollback")
		Eventually(func() agentsv1alpha1.SandboxPhase {
			_ = k8sClient.Get(ctx, types.NamespacedName{
				Name:      sandbox.Name,
				Namespace: sandbox.Namespace,
			}, sandbox)
			return sandbox.Status.Phase
		}, time.Minute*3, time.Millisecond*500).Should(Equal(agentsv1alpha1.SandboxRunning))

		By("Verifying Upgrading condition is Succeeded")
		upgradeCond := getUpgradingCondition(sandbox)
		Expect(upgradeCond).NotTo(BeNil())
		Expect(upgradeCond.Status).To(Equal(metav1.ConditionTrue))
		Expect(upgradeCond.Reason).To(Equal(agentsv1alpha1.SandboxUpgradingReasonSucceeded))

		By("Verifying pod image is the initial version after rollback")
		rollbackPod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
		}, rollbackPod)).To(Succeed())
		Expect(rollbackPod.Spec.Containers[0].Image).To(Equal(initialImage))
	})
})
