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
	"k8s.io/apimachinery/pkg/util/intstr"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

var _ = Describe("Sandbox Pause/Resume Lifecycle Hooks", func() {
	var (
		ctx       = context.Background()
		namespace string
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

	// newHookSandbox creates a Sandbox with the envd runtime wiring (same
	// layout as the upgrade tests) and optional prePause/postResume hooks.
	newHookSandbox := func(prePauseScript, postResumeScript string) *agentsv1alpha1.Sandbox {
		alwaysRestart := corev1.ContainerRestartPolicyAlways
		sbx := &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("hook-sandbox-%d", time.Now().UnixNano()),
				Namespace: namespace,
			},
			Spec: agentsv1alpha1.SandboxSpec{
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
									Env:           []corev1.EnvVar{{Name: "ENVD_DIR", Value: "/mnt/envd"}},
									RestartPolicy: &alwaysRestart,
								},
							},
							Containers: []corev1.Container{
								{
									Name:    "test-container",
									Image:   "centos:7",
									Command: []string{"/bin/bash", "-c", "sleep infinity"},
									Env:     []corev1.EnvVar{{Name: "ENVD_DIR", Value: "/mnt/envd"}},
									StartupProbe: &corev1.Probe{
										ProbeHandler: corev1.ProbeHandler{
											TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(49983)},
										},
										FailureThreshold:    20,
										InitialDelaySeconds: 3,
										PeriodSeconds:       1,
										SuccessThreshold:    1,
										TimeoutSeconds:      1,
									},
									VolumeMounts: []corev1.VolumeMount{
										{Name: "envd-volume", MountPath: "/mnt/envd"},
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
								{Name: "envd-volume", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
							},
							RestartPolicy: corev1.RestartPolicyNever,
						},
					},
				},
			},
		}

		if prePauseScript != "" || postResumeScript != "" {
			sbx.Spec.Lifecycle = &agentsv1alpha1.SandboxLifecycle{}
			if prePauseScript != "" {
				sbx.Spec.Lifecycle.PrePause = &agentsv1alpha1.LifecycleAction{
					Exec:           &corev1.ExecAction{Command: []string{"/bin/bash", "-c", prePauseScript}},
					TimeoutSeconds: 30,
				}
			}
			if postResumeScript != "" {
				sbx.Spec.Lifecycle.PostResume = &agentsv1alpha1.LifecycleAction{
					Exec:           &corev1.ExecAction{Command: []string{"/bin/bash", "-c", postResumeScript}},
					TimeoutSeconds: 30,
				}
			}
		}
		return sbx
	}

	waitForRunning := func(sandbox *agentsv1alpha1.Sandbox) {
		Eventually(func() agentsv1alpha1.SandboxPhase {
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}, sandbox)
			return sandbox.Status.Phase
		}, time.Minute*2, time.Millisecond*500).Should(Equal(agentsv1alpha1.SandboxRunning))
	}

	// setPaused patches spec.paused (first-writer-wins update with retries).
	setPaused := func(sandbox *agentsv1alpha1.Sandbox, paused bool) {
		latest := &agentsv1alpha1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}, latest)).To(Succeed())
		latest.Spec.Paused = paused
		Expect(updateSandboxSpec(ctx, latest)).To(Succeed())
	}

	// waitForPauseCompleted waits for SandboxPaused=True, which happens only
	// after the prePause hook terminal result and pod deletion.
	waitForPauseCompleted := func(sandbox *agentsv1alpha1.Sandbox) {
		Eventually(func() bool {
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}, sandbox)
			cond := utils.GetSandboxCondition(&sandbox.Status, string(agentsv1alpha1.SandboxConditionPaused))
			return cond != nil && cond.Status == metav1.ConditionTrue
		}, time.Minute*3, time.Second).Should(BeTrue(), func() string {
			return fmt.Sprintf("sandbox %s did not complete pause; phase=%s", sandbox.Name, sandbox.Status.Phase)
		})
	}

	waitForReady := func(sandbox *agentsv1alpha1.Sandbox) {
		Eventually(func() bool {
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}, sandbox)
			cond := utils.GetSandboxCondition(&sandbox.Status, string(agentsv1alpha1.SandboxConditionReady))
			return cond != nil && cond.Status == metav1.ConditionTrue
		}, time.Minute*3, time.Second).Should(BeTrue())
	}

	hookCondition := func(sandbox *agentsv1alpha1.Sandbox, condType agentsv1alpha1.SandboxConditionType) *metav1.Condition {
		return utils.GetSandboxCondition(&sandbox.Status, string(condType))
	}

	// warmUpWithPauseResume runs one full pause/resume cycle before the
	// assertion cycle. The resume path performs the runtime /init handshake,
	// which is how envd learns the default user; pause/resume hooks send no
	// per-call user credential, so a prePause that runs before any /init would
	// be rejected by envd builds without a built-in fallback user. This mirrors
	// production use, where every delivered sandbox has been runtime-initialized.
	warmUpWithPauseResume := func(sandbox *agentsv1alpha1.Sandbox) {
		By("Warming up with one pause/resume cycle so runtime /init delivers the default user")
		setPaused(sandbox, true)
		waitForPauseCompleted(sandbox)
		setPaused(sandbox, false)
		waitForReady(sandbox)
	}

	It("should run prePause before pod deletion and keep the result observable", func() {
		sandbox := newHookSandbox("echo prepause-done > /tmp/prepause-marker", "")
		By("Creating sandbox with a prePause hook that writes a marker")
		Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
		waitForRunning(sandbox)
		warmUpWithPauseResume(sandbox)

		By("Pausing the sandbox")
		setPaused(sandbox, true)

		By("Waiting for the prePause hook to succeed before the pod disappears")
		Eventually(func() string {
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}, sandbox)
			cond := hookCondition(sandbox, agentsv1alpha1.SandboxConditionPrePauseHook)
			if cond == nil {
				return ""
			}
			return cond.Reason
		}, time.Minute, time.Second).Should(Equal(agentsv1alpha1.SandboxLifecycleHookReasonSucceeded))
		cond := hookCondition(sandbox, agentsv1alpha1.SandboxConditionPrePauseHook)
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))

		By("Waiting for pause completion (pod deleted, SandboxPaused=True)")
		waitForPauseCompleted(sandbox)

		By("Verifying the hook result stays observable after pause completion")
		cond = hookCondition(sandbox, agentsv1alpha1.SandboxConditionPrePauseHook)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(sandbox.Status.Phase).To(Equal(agentsv1alpha1.SandboxPaused))
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}, pod)).NotTo(Succeed())
	})

	It("should record prePause failure and still complete the pause", func() {
		sandbox := newHookSandbox("exit 1", "")
		By("Creating sandbox with a failing prePause hook")
		Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
		waitForRunning(sandbox)
		warmUpWithPauseResume(sandbox)

		By("Pausing the sandbox")
		setPaused(sandbox, true)

		By("Waiting for the prePause hook to reach terminal Failed")
		Eventually(func() string {
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}, sandbox)
			cond := hookCondition(sandbox, agentsv1alpha1.SandboxConditionPrePauseHook)
			if cond == nil {
				return ""
			}
			return cond.Reason
		}, time.Minute, time.Second).Should(Equal(agentsv1alpha1.SandboxLifecycleHookReasonFailed))
		cond := hookCondition(sandbox, agentsv1alpha1.SandboxConditionPrePauseHook)
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Message).To(ContainSubstring("lifecycle transition continues"))

		By("Verifying the pause still completes (pass-through)")
		waitForPauseCompleted(sandbox)

		By("Verifying the Failed result stays observable after pause completion")
		cond = hookCondition(sandbox, agentsv1alpha1.SandboxConditionPrePauseHook)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(agentsv1alpha1.SandboxLifecycleHookReasonFailed))
	})

	It("should run postResume after resume and keep the result observable", func() {
		sandbox := newHookSandbox("", "echo postresume-done > /tmp/postresume-marker")
		By("Creating sandbox with a postResume hook")
		Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
		waitForRunning(sandbox)

		By("Pausing and resuming the sandbox")
		setPaused(sandbox, true)
		waitForPauseCompleted(sandbox)
		setPaused(sandbox, false)

		By("Waiting for the postResume hook to succeed")
		Eventually(func() string {
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}, sandbox)
			cond := hookCondition(sandbox, agentsv1alpha1.SandboxConditionPostResumeHook)
			if cond == nil {
				return ""
			}
			return cond.Reason
		}, time.Minute*3, time.Second).Should(Equal(agentsv1alpha1.SandboxLifecycleHookReasonSucceeded))
		cond := hookCondition(sandbox, agentsv1alpha1.SandboxConditionPostResumeHook)
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))

		By("Waiting for Ready to publish after the hook terminal result")
		waitForReady(sandbox)

		By("Verifying the Succeeded result stays observable with the completed resume")
		cond = hookCondition(sandbox, agentsv1alpha1.SandboxConditionPostResumeHook)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(agentsv1alpha1.SandboxLifecycleHookReasonSucceeded))
	})

	It("should publish Ready after a failed postResume (completion-gated, not success-gated)", func() {
		sandbox := newHookSandbox("", "exit 1")
		By("Creating sandbox with a failing postResume hook")
		Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
		waitForRunning(sandbox)

		By("Pausing and resuming the sandbox")
		setPaused(sandbox, true)
		waitForPauseCompleted(sandbox)
		setPaused(sandbox, false)

		By("Waiting for the postResume hook to reach terminal Failed")
		Eventually(func() string {
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}, sandbox)
			cond := hookCondition(sandbox, agentsv1alpha1.SandboxConditionPostResumeHook)
			if cond == nil {
				return ""
			}
			return cond.Reason
		}, time.Minute*3, time.Second).Should(Equal(agentsv1alpha1.SandboxLifecycleHookReasonFailed))

		By("Verifying Ready publishes after the terminal (failed) hook result")
		waitForReady(sandbox)
		Expect(sandbox.Status.Phase).To(Equal(agentsv1alpha1.SandboxRunning))

		By("Verifying the Failed result stays observable with the completed resume")
		cond := hookCondition(sandbox, agentsv1alpha1.SandboxConditionPostResumeHook)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(agentsv1alpha1.SandboxLifecycleHookReasonFailed))
		readyCond := utils.GetSandboxCondition(&sandbox.Status, string(agentsv1alpha1.SandboxConditionReady))
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
	})
})
