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
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	runtimeclient "github.com/openkruise/agents/pkg/utils/runtime"
	"github.com/openkruise/agents/pkg/utils/sidecarutils"
	"github.com/openkruise/agents/pkg/utils/webhookutils"
)

var _ = Describe("Sandbox Deferred Startup", Ordered, Serial, func() {
	const (
		mainContainer = "main"
		mainImage     = "ubuntu:24.04"
		waitTimeout   = 3 * time.Minute
		pollInterval  = time.Second
	)
	var (
		ctx       context.Context
		namespace string
	)

	appCommand := func(origin string) []string {
		return []string{"bash", "-c", `printf '%s' "$VALUE" > /tmp/deferred-value; printf '%s' "$1" > /tmp/deferred-started; exec tail -f /dev/null`, "_", origin}
	}

	BeforeAll(func(ctx SpecContext) {
		config := &corev1.ConfigMap{}
		key := types.NamespacedName{Namespace: webhookutils.GetNamespace(), Name: sidecarutils.SandboxInjectionConfigName}
		err := k8sClient.Get(ctx, key, config)
		if err == nil {
			Expect(config.Data[sidecarutils.KEY_RUNTIME_INJECTION_CONFIG]).NotTo(BeEmpty(), "the test cluster must configure agent-runtime injection")
			return
		}
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "cannot read runtime injection configuration: %v", err)
		mount := corev1.VolumeMount{Name: "envd-volume", MountPath: "/mnt/envd"}
		injection := sidecarutils.SidecarInjectConfig{
			MainContainer: corev1.Container{
				Env:          []corev1.EnvVar{{Name: "ENVD_DIR", Value: "/mnt/envd"}},
				VolumeMounts: []corev1.VolumeMount{mount},
				Lifecycle: &corev1.Lifecycle{PostStart: &corev1.LifecycleHandler{
					Exec: &corev1.ExecAction{Command: []string{"bash", "/mnt/envd/envd-run.sh"}},
				}},
				StartupProbe: &corev1.Probe{
					ProbeHandler:     corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(utils.RuntimePort)}},
					PeriodSeconds:    1,
					FailureThreshold: 60,
				},
			},
			Sidecars: []corev1.Container{{
				Name: sidecarutils.ContainerNameRuntimeAgent, Image: "agent-runtime:latest",
				ImagePullPolicy: corev1.PullIfNotPresent, Command: []string{"sh", "/workspace/entrypoint.sh"},
				Env: []corev1.EnvVar{{Name: "ENVD_DIR", Value: "/mnt/envd"}}, VolumeMounts: []corev1.VolumeMount{mount},
				RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways),
			}},
			Volumes: []corev1.Volume{{Name: mount.Name, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
		}
		raw, err := json.Marshal(injection)
		Expect(err).NotTo(HaveOccurred())
		config = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}, Data: map[string]string{sidecarutils.KEY_RUNTIME_INJECTION_CONFIG: string(raw)}}
		// The standalone Kind deployment has no injection ConfigMap; never overwrite an existing one.
		Expect(k8sClient.Create(ctx, config)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			err := k8sClient.Delete(ctx, config, client.Preconditions{UID: &config.UID})
			Expect(client.IgnoreNotFound(err)).To(Succeed())
		})
	})

	BeforeEach(func(specCtx SpecContext) {
		namespace = createNamespace(specCtx)
	})
	AfterEach(func(ctx SpecContext) {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))).To(Succeed())
	})

	podExec := func(box *agentsv1alpha1.Sandbox, argv ...string) (string, error) {
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		args := []string{"exec", "-n", box.Namespace, box.Name, "-c", mainContainer, "--"}
		output, err := exec.CommandContext(callCtx, "kubectl", append(args, argv...)...).CombinedOutput()
		return string(output), err
	}

	waitForStock := func(pool *agentsv1alpha1.SandboxSet) *agentsv1alpha1.Sandbox {
		var warm *agentsv1alpha1.Sandbox
		Eventually(func(g Gomega) {
			warm = nil
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), pool)).To(Succeed())
			g.Expect(pool.Status.AvailableReplicas).To(BeNumerically(">=", 1))
			list := &agentsv1alpha1.SandboxList{}
			g.Expect(k8sClient.List(ctx, list, client.InNamespace(namespace), client.MatchingLabels{
				agentsv1alpha1.LabelSandboxPool: pool.Name, agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.False,
			})).To(Succeed())
			for i := range list.Items {
				box := &list.Items[i]
				ready := utils.GetSandboxCondition(&box.Status, string(agentsv1alpha1.SandboxConditionReady))
				if ready != nil && ready.Status == metav1.ConditionTrue && box.Status.PodInfo.PodUID != "" {
					warm = box.DeepCopy()
					break
				}
			}
			g.Expect(warm).NotTo(BeNil())
		}, waitTimeout, pollInterval).Should(Succeed())
		return warm
	}

	newPool := func(mode agentsv1alpha1.StartupMode, command, args []string, templateRef bool) (*agentsv1alpha1.SandboxSet, *agentsv1alpha1.Sandbox) {
		template := &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			SecurityContext: &corev1.PodSecurityContext{RunAsUser: ptr.To[int64](0)},
			Containers: []corev1.Container{{
				Name: mainContainer, Image: mainImage, Command: command, Args: args,
				Env:          []corev1.EnvVar{{Name: "VALUE", Value: "pool-value"}},
				VolumeMounts: []corev1.VolumeMount{{Name: "envd-volume", MountPath: "/mnt/envd"}},
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("64Mi"),
				}},
			}},
			Volumes:                       []corev1.Volume{{Name: "envd-volume", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
			RestartPolicy:                 corev1.RestartPolicyAlways,
			TerminationGracePeriodSeconds: ptr.To[int64](5),
		}}
		pool := &agentsv1alpha1.SandboxSet{ObjectMeta: metav1.ObjectMeta{GenerateName: "deferred-pool-", Namespace: namespace}, Spec: agentsv1alpha1.SandboxSetSpec{
			Replicas: 1, StartupMode: mode, PauseStrategy: &agentsv1alpha1.PauseStrategy{Type: agentsv1alpha1.PauseStrategyStop},
			Runtimes:                []agentsv1alpha1.RuntimeConfig{{Name: agentsv1alpha1.RuntimeConfigForInjectAgentRuntime}},
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{Template: template},
		}}
		if templateRef {
			tpl := &agentsv1alpha1.SandboxTemplate{ObjectMeta: metav1.ObjectMeta{GenerateName: "deferred-template-", Namespace: namespace}, Spec: agentsv1alpha1.SandboxTemplateSpec{
				StartupMode: mode, Runtimes: pool.Spec.Runtimes, Template: template,
			}}
			Expect(k8sClient.Create(ctx, tpl)).To(Succeed())
			pool.Spec.Template = nil
			pool.Spec.TemplateRef = &agentsv1alpha1.SandboxTemplateRef{Name: tpl.Name}
		}
		Expect(k8sClient.Create(ctx, pool)).To(Succeed())
		return pool, waitForStock(pool)
	}

	newClaim := func(pool *agentsv1alpha1.SandboxSet, env map[string]string, command []string, skipInit bool) *agentsv1alpha1.SandboxClaim {
		claim := &agentsv1alpha1.SandboxClaim{ObjectMeta: metav1.ObjectMeta{GenerateName: "deferred-claim-", Namespace: namespace}, Spec: agentsv1alpha1.SandboxClaimSpec{
			TemplateName: pool.Name, Replicas: ptr.To[int32](1), EnvVars: env, Command: command, SkipInitRuntime: skipInit,
			ClaimTimeout: &metav1.Duration{Duration: waitTimeout}, WaitReadyTimeout: &metav1.Duration{Duration: waitTimeout},
		}}
		Expect(k8sClient.Create(ctx, claim)).To(Succeed())
		return claim
	}

	waitForClaim := func(claim *agentsv1alpha1.SandboxClaim) *agentsv1alpha1.Sandbox {
		var claimed *agentsv1alpha1.Sandbox
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(claim), claim)).To(Succeed())
			g.Expect(claim.Status.Phase).To(Equal(agentsv1alpha1.SandboxClaimPhaseCompleted))
			g.Expect(claim.Status.ClaimedReplicas).To(Equal(int32(1)), claim.Status.Message)
			list := &agentsv1alpha1.SandboxList{}
			g.Expect(k8sClient.List(ctx, list, client.InNamespace(namespace))).To(Succeed())
			matches := []agentsv1alpha1.Sandbox{}
			for _, box := range list.Items {
				if box.Annotations[agentsv1alpha1.AnnotationOwner] == string(claim.UID) {
					matches = append(matches, box)
				}
			}
			g.Expect(matches).To(HaveLen(1))
			claimed = matches[0].DeepCopy()
			request, err := runtimeclient.GetInitRuntimeRequest(claimed)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(request).NotTo(BeNil())
			g.Expect(request.StartupDelivered).To(BeTrue())
			g.Expect(claimed.Status.PodInfo.PodUID).NotTo(BeEmpty())
		}, waitTimeout, pollInterval).Should(Succeed())
		return claimed
	}

	checkApplication := func(box *agentsv1alpha1.Sandbox, origin, value string) {
		Eventually(func(g Gomega) {
			got, err := podExec(box, "cat", "/tmp/deferred-started")
			g.Expect(err).NotTo(HaveOccurred(), got)
			g.Expect(got).To(Equal(origin))
			got, err = podExec(box, "cat", "/tmp/deferred-value")
			g.Expect(err).NotTo(HaveOccurred(), got)
			g.Expect(got).To(Equal(value))
		}, waitTimeout, pollInterval).Should(Succeed())
	}

	checkSignal := func(box *agentsv1alpha1.Sandbox) {
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(box), box)).To(Succeed())
			got, err := podExec(box, "cat", utils.SandboxEnvClaimDir+"/.env_ready")
			g.Expect(err).NotTo(HaveOccurred(), got)
			g.Expect(got).To(Equal(string(box.Status.PodInfo.PodUID)))
		}, waitTimeout, pollInterval).Should(Succeed())
	}

	DescribeTable("claim-time startup",
		func(specCtx SpecContext, templateCommand, args, claimCommand []string, env map[string]string, skipInit, templateRef, idle bool, origin, value string) {
			ctx = specCtx
			pool, warm := newPool(agentsv1alpha1.StartupModeDeferred, templateCommand, args, templateRef)
			Expect(warm.Spec.StartupMode).To(Equal(agentsv1alpha1.StartupModeDeferred))
			pod := getPodOrFail(ctx, client.ObjectKeyFromObject(warm))
			Expect(pod.Spec.Containers[0].Command).To(Equal([]string{"/bin/bash", utils.RunWithEnvsScript}))
			Expect(pod.Spec.InitContainers[0].Name).To(Equal("envd-scripts"))
			Expect(pod.Spec.InitContainers[0].RestartPolicy).To(BeNil())
			var envdVolume *corev1.Volume
			for i := range pod.Spec.Volumes {
				if pod.Spec.Volumes[i].Name == "envd-volume" {
					envdVolume = &pod.Spec.Volumes[i]
					break
				}
			}
			Expect(envdVolume).NotTo(BeNil())
			Expect(envdVolume.EmptyDir).NotTo(BeNil())
			Consistently(func() error {
				_, err := podExec(warm, "bash", "-c", "test ! -e /tmp/deferred-started && test ! -e /mnt/envd/claim/.env_ready")
				return err
			}, 3*time.Second, pollInterval).Should(Succeed(), "the main process must not start before claim")

			claim := newClaim(pool, env, claimCommand, skipInit)
			box := waitForClaim(claim)
			Expect(box.Name).To(Equal(warm.Name))
			Expect(box.Status.PodInfo.PodUID).To(Equal(pod.UID), "claim must reuse the pre-warmed Pod")
			checkSignal(box)
			if idle {
				Eventually(func() (string, error) {
					logs, err := clientset.CoreV1().Pods(namespace).GetLogs(box.Name, &corev1.PodLogOptions{Container: mainContainer}).DoRaw(ctx)
					return string(logs), err
				}, waitTimeout, pollInterval).Should(ContainSubstring("idling with 'tail -f /dev/null'"))
				got, err := podExec(box, "bash", "-c", "test ! -e /tmp/deferred-started && test ! -e /tmp/unexpected-args")
				Expect(err).NotTo(HaveOccurred(), got)
			} else {
				checkApplication(box, origin, value)
			}
			if len(claimCommand) == 0 {
				got, err := podExec(box, "test", "!", "-e", utils.SandboxEnvClaimDir+"/cmd.sh")
				Expect(err).NotTo(HaveOccurred(), got)
			}
			if len(env) > 0 {
				got, err := podExec(box, "stat", "-c", "%a", utils.SandboxEnvClaimDir+"/env.sh")
				Expect(err).NotTo(HaveOccurred(), got)
				Expect(got).To(Equal("600\n"))
			}
			request, err := runtimeclient.GetInitRuntimeRequest(box)
			Expect(err).NotTo(HaveOccurred())
			Expect(request.Command).To(Equal(claimCommand))
			Expect(request.SkipInit).To(Equal(skipInit))
		},
		Entry("injects quoted and multiline values into the template main process", appCommand("template"), []string(nil), []string(nil), map[string]string{"VALUE": "a b'c\n$(touch /tmp/unexpected-args); $HOME `false`"}, false, false, false, "template", "a b'c\n$(touch /tmp/unexpected-args); $HOME `false`"),
		Entry("claim command overrides the template command", appCommand("template"), []string(nil), appCommand("claim"), map[string]string{"VALUE": "claim-value"}, false, false, false, "claim", "claim-value"),
		Entry("claim supplies the command for a command-less templateRef", []string(nil), []string(nil), appCommand("claim"), map[string]string{"VALUE": "claim-value"}, false, true, false, "claim", "claim-value"),
		Entry("args-only templates do not turn Args into the executable", []string(nil), []string{"-c", "touch /tmp/unexpected-args"}, appCommand("claim"), map[string]string{"VALUE": "claim-value"}, false, false, false, "claim", "claim-value"),
		Entry("an empty claim releases a command-less pool into tail", []string(nil), []string(nil), []string(nil), map[string]string(nil), false, false, true, "", ""),
		Entry("an args-only pool without a claim command idles instead of running Args", []string(nil), []string{"-c", "touch /tmp/unexpected-args"}, []string(nil), map[string]string(nil), false, false, true, "", ""),
		Entry("an empty claim still releases the template command", appCommand("template"), []string(nil), []string(nil), map[string]string(nil), false, false, false, "template", "pool-value"),
		Entry("skipInitRuntime does not skip startup data delivery", appCommand("template"), []string(nil), appCommand("claim"), map[string]string{"VALUE": "skip-init-value"}, true, false, false, "claim", "skip-init-value"),
	)

	It("isolates startup files between consecutive claims", func(specCtx SpecContext) {
		ctx = specCtx
		pool, _ := newPool(agentsv1alpha1.StartupModeDeferred, appCommand("template"), nil, false)
		first := waitForClaim(newClaim(pool, map[string]string{"VALUE": "first"}, appCommand("first-command"), false))
		waitForStock(pool)
		second := waitForClaim(newClaim(pool, map[string]string{"VALUE": "second"}, nil, false))
		Expect(first.Status.PodInfo.PodUID).NotTo(Equal(second.Status.PodInfo.PodUID))
		checkApplication(first, "first-command", "first")
		checkApplication(second, "template", "second")
		checkSignal(first)
		checkSignal(second)
		got, err := podExec(second, "test", "!", "-e", utils.SandboxEnvClaimDir+"/cmd.sh")
		Expect(err).NotTo(HaveOccurred(), got)
	})

	It("replays claim env and command after Stop pause and resume", func(specCtx SpecContext) {
		ctx = specCtx
		pool, _ := newPool(agentsv1alpha1.StartupModeDeferred, appCommand("template"), nil, false)
		box := waitForClaim(newClaim(pool, map[string]string{"VALUE": "resume-value"}, appCommand("claim"), false))
		checkApplication(box, "claim", "resume-value")
		oldUID := box.Status.PodInfo.PodUID
		By("Pausing the claimed sandbox and waiting for its Pod to be deleted")
		box.Spec.Paused = true
		Expect(updateSandboxSpec(ctx, box)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(box), box)).To(Succeed())
			g.Expect(box.Status.Phase).To(Equal(agentsv1alpha1.SandboxPaused))
			g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(box), &corev1.Pod{}))).To(BeTrue())
		}, waitTimeout, pollInterval).Should(Succeed())
		By("Resuming with a new Pod generation")
		box.Spec.Paused = false
		Expect(updateSandboxSpec(ctx, box)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(box), box)).To(Succeed())
			g.Expect(box.Status.Phase).To(Equal(agentsv1alpha1.SandboxRunning))
			g.Expect(box.Status.PodInfo.PodUID).NotTo(BeEmpty())
			g.Expect(box.Status.PodInfo.PodUID).NotTo(Equal(oldUID))
			condition := utils.GetSandboxCondition(&box.Status, string(agentsv1alpha1.RuntimeInitialized))
			g.Expect(condition).NotTo(BeNil())
			g.Expect(condition.Status).To(Equal(metav1.ConditionTrue))
		}, waitTimeout, pollInterval).Should(Succeed())
		checkApplication(box, "claim", "resume-value")
		checkSignal(box)
	})

	It("replays claim env and command after a Recreate upgrade", func(specCtx SpecContext) {
		ctx = specCtx
		pool, _ := newPool(agentsv1alpha1.StartupModeDeferred, appCommand("template"), nil, false)
		box := waitForClaim(newClaim(pool, map[string]string{"VALUE": "upgrade-value"}, appCommand("claim"), false))
		checkApplication(box, "claim", "upgrade-value")
		oldUID := box.Status.PodInfo.PodUID
		box.Spec.UpgradePolicy = &agentsv1alpha1.SandboxUpgradePolicy{Type: agentsv1alpha1.SandboxUpgradePolicyRecreate}
		Expect(updateSandboxSpec(ctx, box)).To(Succeed())
		box.Spec.Template.Spec.Containers[0].Image = "ubuntu:22.04"
		Expect(updateSandboxSpec(ctx, box)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(box), box)).To(Succeed())
			pod := &corev1.Pod{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(box), pod)).To(Succeed())
			g.Expect(pod.UID).NotTo(Equal(oldUID))
			g.Expect(pod.Spec.Containers[0].Image).To(Equal("ubuntu:22.04"))
			condition := utils.GetSandboxCondition(&box.Status, string(agentsv1alpha1.SandboxConditionUpgrading))
			g.Expect(condition).NotTo(BeNil())
			g.Expect(condition.Reason).To(Equal(agentsv1alpha1.SandboxUpgradingReasonSucceeded))
		}, waitTimeout, pollInterval).Should(Succeed())
		checkApplication(box, "claim", "upgrade-value")
		checkSignal(box)
	})

	DescribeTable("rejects invalid claims without starting the warm process",
		func(specCtx SpecContext, mode agentsv1alpha1.StartupMode, env map[string]string, command []string) {
			ctx = specCtx
			pool, warm := newPool(mode, appCommand("template"), nil, false)
			claim := newClaim(pool, env, command, false)
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(claim), claim)).To(Succeed())
				g.Expect(claim.Status.Phase).To(Equal(agentsv1alpha1.SandboxClaimPhaseCompleted))
				g.Expect(claim.Status.ClaimedReplicas).To(BeZero())
				g.Expect(claim.Status.Conditions).To(ContainElement(WithTransform(func(c metav1.Condition) string { return c.Reason }, Equal("InvalidClaimSpec"))))
			}, waitTimeout, pollInterval).Should(Succeed())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(warm), warm)).To(Succeed())
			Expect(warm.Labels[agentsv1alpha1.LabelSandboxIsClaimed]).To(Equal(agentsv1alpha1.False))
			if mode == agentsv1alpha1.StartupModeDeferred {
				got, err := podExec(warm, "test", "!", "-e", utils.SandboxEnvClaimDir+"/.env_ready")
				Expect(err).NotTo(HaveOccurred(), got)
			} else {
				checkApplication(warm, "template", "pool-value")
			}
		},
		Entry("Immediate pools reject a claim command", agentsv1alpha1.StartupModeImmediate, map[string]string(nil), appCommand("claim")),
		Entry("reserved wrapper variables are rejected", agentsv1alpha1.StartupModeDeferred, map[string]string{"_rwe_cmd_script": "/tmp/other"}, []string(nil)),
	)

	It("rejects an explicitly non-root Deferred pool", func(specCtx SpecContext) {
		ctx = specCtx
		pool := &agentsv1alpha1.SandboxSet{ObjectMeta: metav1.ObjectMeta{GenerateName: "non-root-pool-", Namespace: namespace}, Spec: agentsv1alpha1.SandboxSetSpec{
			Replicas: 1, StartupMode: agentsv1alpha1.StartupModeDeferred,
			Runtimes: []agentsv1alpha1.RuntimeConfig{{Name: agentsv1alpha1.RuntimeConfigForInjectAgentRuntime}},
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: mainContainer, Image: mainImage, SecurityContext: &corev1.SecurityContext{RunAsUser: ptr.To[int64](1000)}, VolumeMounts: []corev1.VolumeMount{{Name: "envd-volume", MountPath: "/mnt/envd"}}}},
				Volumes:    []corev1.Volume{{Name: "envd-volume", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
			}}},
		}}
		err := k8sClient.Create(ctx, pool)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("root main container"))
	})
})
