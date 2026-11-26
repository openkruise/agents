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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubescheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	e2bmodels "github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/utils"
	runtimeclient "github.com/openkruise/agents/pkg/utils/runtime"
	runtimeconfig "github.com/openkruise/agents/pkg/utils/runtime/config"
	"github.com/openkruise/agents/pkg/utils/sidecarutils"
	"github.com/openkruise/agents/pkg/utils/webhookutils"
	"github.com/openkruise/agents/proto/envd/process"
)

var _ = Describe("Sandbox Deferred Startup", Ordered, Serial, func() {
	const (
		mainContainer = "main"
		waitTimeout   = 3 * time.Minute
		pollInterval  = time.Second
	)
	var (
		ctx          context.Context
		namespace    string
		mainImage    = "ubuntu:24.04"
		altImage     = "ubuntu:22.04"
		runtimeImage = "agent-runtime:latest"
		nodeSelector map[string]string
	)

	appCommand := func(origin string) []string {
		return []string{"bash", "-c", `printf '%s' "$VALUE" > /tmp/deferred-value; printf '%s' "$1" > /tmp/deferred-started; exec tail -f /dev/null`, "_", origin}
	}

	BeforeAll(func(ctx SpecContext) {
		if image := os.Getenv("E2E_DEFERRED_STARTUP_IMAGE"); image != "" {
			mainImage = image
		}
		if image := os.Getenv("E2E_DEFERRED_STARTUP_IMAGE_ALT"); image != "" {
			altImage = image
		}
		if image := os.Getenv("E2E_DEFERRED_STARTUP_RUNTIME_IMAGE"); image != "" {
			runtimeImage = image
		}
		if selector := os.Getenv("E2E_DEFERRED_STARTUP_NODE_SELECTOR"); selector != "" {
			var err error
			nodeSelector, err = labels.ConvertSelectorToLabelsMap(selector)
			Expect(err).NotTo(HaveOccurred(), "node selector must contain label key=value pairs")
		}
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
				Name: sidecarutils.ContainerNameRuntimeAgent, Image: runtimeImage,
				ImagePullPolicy: corev1.PullIfNotPresent, Command: []string{"sh", "/workspace/entrypoint.sh"},
				Env: []corev1.EnvVar{{Name: "ENVD_DIR", Value: "/mnt/envd"}}, VolumeMounts: []corev1.VolumeMount{mount},
				RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways),
				// The entrypoint publishes artifacts; envd starts in the main postStart hook.
				StartupProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{
						"sh", "-c", `test -x "$ENVD_DIR/envd" && test -x "$ENVD_DIR/envd-run.sh"`,
					}}},
					PeriodSeconds:    1,
					FailureThreshold: 60,
				},
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
		if CurrentSpecReport().Failed() && os.Getenv("E2E_KEEP_FAILED_NS") == "true" {
			_, err := fmt.Fprintf(GinkgoWriter, "Preserving failed deferred startup namespace %s\n", namespace)
			Expect(err).NotTo(HaveOccurred())
			return
		}
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))).To(Succeed())
	})

	podExec := func(box *agentsv1alpha1.Sandbox, argv ...string) (string, error) {
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		request := clientset.CoreV1().RESTClient().Post().Resource("pods").Namespace(box.Namespace).
			Name(box.Name).SubResource("exec").VersionedParams(&corev1.PodExecOptions{
			Container: mainContainer, Command: argv, Stdout: true, Stderr: true,
		}, kubescheme.ParameterCodec)
		executor, err := remotecommand.NewSPDYExecutor(config.GetConfigOrDie(), "POST", request.URL())
		if err != nil {
			return "", err
		}
		var stdout, stderr bytes.Buffer
		err = executor.StreamWithContext(callCtx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr})
		return stdout.String() + stderr.String(), err
	}

	readyPod := func(g Gomega, box *agentsv1alpha1.Sandbox) *corev1.Pod {
		pod := &corev1.Pod{}
		g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(box), pod)).To(Succeed())
		g.Expect(pod.UID).To(Equal(box.Status.PodInfo.PodUID))
		g.Expect(pod.Spec.NodeSelector).To(Equal(nodeSelector))
		condition := utils.GetPodCondition(&pod.Status, corev1.PodReady)
		g.Expect(condition).NotTo(BeNil())
		g.Expect(condition.Status).To(Equal(corev1.ConditionTrue))
		g.Expect(pod.Status.ContainerStatuses).To(ContainElement(And(
			WithTransform(func(s corev1.ContainerStatus) string { return s.Name }, Equal(mainContainer)),
			WithTransform(func(s corev1.ContainerStatus) bool { return s.Ready && s.State.Running != nil }, BeTrue()),
		)))
		g.Expect(pod.Status.InitContainerStatuses).To(ContainElement(And(
			WithTransform(func(s corev1.ContainerStatus) string { return s.Name }, Equal(sidecarutils.ContainerNameRuntimeAgent)),
			WithTransform(func(s corev1.ContainerStatus) bool { return s.Ready && s.Started != nil && *s.Started }, BeTrue()),
		)))
		return pod
	}

	waitForStock := func(pool *agentsv1alpha1.SandboxSet) *agentsv1alpha1.Sandbox {
		var warm *agentsv1alpha1.Sandbox
		Eventually(func(g Gomega) {
			warm = nil
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), pool)).To(Succeed())
			g.Expect(pool.Status.AvailableReplicas).To(BeNumerically(">=", pool.Spec.Replicas))
			list := &agentsv1alpha1.SandboxList{}
			g.Expect(k8sClient.List(ctx, list, client.InNamespace(pool.Namespace), client.MatchingLabels{
				agentsv1alpha1.LabelSandboxPool: pool.Name, agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.False,
			})).To(Succeed())
			readyCount := int32(0)
			for i := range list.Items {
				box := &list.Items[i]
				ready := utils.GetSandboxCondition(&box.Status, string(agentsv1alpha1.SandboxConditionReady))
				if ready != nil && ready.Status == metav1.ConditionTrue && box.Status.PodInfo.PodUID != "" {
					pod := readyPod(g, box)
					for _, status := range pod.Status.ContainerStatuses {
						g.Expect(status.RestartCount).To(BeZero())
					}
					warm = box.DeepCopy()
					readyCount++
				}
			}
			g.Expect(readyCount).To(BeNumerically(">=", pool.Spec.Replicas))
			g.Expect(warm).NotTo(BeNil())
		}, waitTimeout, pollInterval).Should(Succeed())
		return warm
	}

	newPool := func(mode agentsv1alpha1.StartupMode, command, args []string, templateRef bool, customize ...func(*agentsv1alpha1.SandboxSet)) (*agentsv1alpha1.SandboxSet, *agentsv1alpha1.Sandbox) {
		template := &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			NodeSelector:    nodeSelector,
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
		for _, configure := range customize {
			configure(pool)
		}
		if templateRef {
			tpl := &agentsv1alpha1.SandboxTemplate{ObjectMeta: metav1.ObjectMeta{GenerateName: "deferred-template-", Namespace: namespace}, Spec: agentsv1alpha1.SandboxTemplateSpec{
				StartupMode: mode, Runtimes: pool.Spec.Runtimes, Template: template,
			}}
			Expect(k8sClient.Create(ctx, tpl)).To(Succeed())
			pool.Spec.Template = nil
			pool.Spec.TemplateRef = &agentsv1alpha1.SandboxTemplateRef{Name: tpl.Name}
		}
		Expect(k8sClient.Create(ctx, pool)).To(Succeed())
		if pool.Namespace != namespace {
			ownedPool := pool.DeepCopy()
			DeferCleanup(func(cleanupCtx SpecContext) {
				if CurrentSpecReport().Failed() && os.Getenv("E2E_KEEP_FAILED_NS") == "true" {
					return
				}
				Expect(client.IgnoreNotFound(k8sClient.Delete(cleanupCtx, ownedPool,
					client.Preconditions{UID: &ownedPool.UID}, client.PropagationPolicy(metav1.DeletePropagationForeground)))).To(Succeed())
				Eventually(func() bool {
					return apierrors.IsNotFound(k8sClient.Get(cleanupCtx, client.ObjectKeyFromObject(ownedPool), &agentsv1alpha1.SandboxSet{}))
				}, waitTimeout, pollInterval).Should(BeTrue())
				// Claims detach from the pool; delete only this generated pool's named instances.
				list := &agentsv1alpha1.SandboxList{}
				Expect(k8sClient.List(cleanupCtx, list, client.InNamespace(ownedPool.Namespace),
					client.MatchingLabels{agentsv1alpha1.LabelSandboxPool: ownedPool.Name})).To(Succeed())
				for i := range list.Items {
					box := &list.Items[i]
					Expect(client.IgnoreNotFound(k8sClient.Delete(cleanupCtx, box, client.Preconditions{UID: &box.UID}))).To(Succeed())
					Eventually(func(g Gomega) {
						g.Expect(apierrors.IsNotFound(k8sClient.Get(cleanupCtx, client.ObjectKeyFromObject(box), &agentsv1alpha1.Sandbox{}))).To(BeTrue())
						g.Expect(apierrors.IsNotFound(k8sClient.Get(cleanupCtx, client.ObjectKeyFromObject(box), &corev1.Pod{}))).To(BeTrue())
					}, waitTimeout, pollInterval).Should(Succeed())
				}
			})
		}
		if pool.Spec.Replicas == 0 {
			return pool, nil
		}
		return pool, waitForStock(pool)
	}

	newClaim := func(pool *agentsv1alpha1.SandboxSet, env map[string]string, command []string, skipInit bool, customize ...func(*agentsv1alpha1.SandboxClaim)) *agentsv1alpha1.SandboxClaim {
		claim := &agentsv1alpha1.SandboxClaim{ObjectMeta: metav1.ObjectMeta{GenerateName: "deferred-claim-", Namespace: namespace}, Spec: agentsv1alpha1.SandboxClaimSpec{
			TemplateName: pool.Name, Replicas: ptr.To[int32](1), EnvVars: env, Command: command, SkipInitRuntime: skipInit,
			ClaimTimeout: &metav1.Duration{Duration: waitTimeout}, WaitReadyTimeout: &metav1.Duration{Duration: waitTimeout},
		}}
		for _, configure := range customize {
			configure(claim)
		}
		Expect(k8sClient.Create(ctx, claim)).To(Succeed())
		return claim
	}

	waitForClaim := func(claim *agentsv1alpha1.SandboxClaim) *agentsv1alpha1.Sandbox {
		var claimed *agentsv1alpha1.Sandbox
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(claim), claim)).To(Succeed())
			g.Expect(claim.Status.Phase).To(Equal(agentsv1alpha1.SandboxClaimPhaseCompleted))
			g.Expect(claim.Status.ClaimedReplicas).To(Equal(*claim.Spec.Replicas), claim.Status.Message)
			list := &agentsv1alpha1.SandboxList{}
			g.Expect(k8sClient.List(ctx, list, client.InNamespace(namespace))).To(Succeed())
			matches := []agentsv1alpha1.Sandbox{}
			for _, box := range list.Items {
				if box.Annotations[agentsv1alpha1.AnnotationOwner] == string(claim.UID) {
					matches = append(matches, box)
				}
			}
			g.Expect(matches).To(HaveLen(int(*claim.Spec.Replicas)))
			for i := range matches {
				box := &matches[i]
				pod := readyPod(g, box)
				for _, status := range pod.Status.ContainerStatuses {
					g.Expect(status.RestartCount).To(BeZero())
				}
				g.Expect(box.Labels[agentsv1alpha1.LabelSandboxIsClaimed]).To(Equal(agentsv1alpha1.True))
				g.Expect(box.Labels[agentsv1alpha1.LabelSandboxClaimName]).To(Equal(claim.Name))
				if box.Spec.StartupMode == agentsv1alpha1.StartupModeDeferred {
					request, err := runtimeclient.GetInitRuntimeRequest(box)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(request).NotTo(BeNil())
					g.Expect(request.StartupDelivered).To(BeTrue())
					g.Expect(request.StartupPodUID).To(Equal(string(pod.UID)))
				}
			}
			claimed = matches[0].DeepCopy()
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
			pod := readyPod(g, box)
			request, err := runtimeclient.GetInitRuntimeRequest(box)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(request).NotTo(BeNil())
			g.Expect(request.StartupDelivered).To(BeTrue())
			g.Expect(request.StartupPodUID).To(Equal(string(pod.UID)))
			got, err := podExec(box, "cat", utils.SandboxEnvClaimDir+"/.env_ready")
			g.Expect(err).NotTo(HaveOccurred(), got)
			g.Expect(got).To(Equal(string(pod.UID)))
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
			Expect(pod.Spec.Containers[0].VolumeMounts).To(ContainElement(And(
				WithTransform(func(m corev1.VolumeMount) string { return m.MountPath }, Equal("/mnt/envd")),
				WithTransform(func(m corev1.VolumeMount) bool { return m.ReadOnly }, BeFalse()),
			)))
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

	It("starts an Immediate template before claim without rewriting its command or args", func(specCtx SpecContext) {
		ctx = specCtx
		command := appCommand("immediate")
		args := []string{"untouched argument", ""}
		pool, warm := newPool(agentsv1alpha1.StartupModeImmediate, command, args, false)
		checkApplication(warm, "immediate", "pool-value")
		pod := readyPod(Default, warm)
		Expect(pod.Spec.Containers[0].Command).To(Equal(command))
		Expect(pod.Spec.Containers[0].Args).To(Equal(args))
		box := waitForClaim(newClaim(pool, map[string]string{"VALUE": "claim-value"}, nil, false))
		Expect(box.Name).To(Equal(warm.Name))
		Expect(box.Status.PodInfo.PodUID).To(Equal(pod.UID))
		checkApplication(box, "immediate", "pool-value")
		got, err := podExec(box, "test", "!", "-e", utils.SandboxEnvClaimDir+"/.env_ready")
		Expect(err).NotTo(HaveOccurred(), got)
	})

	It("preserves quoted multiline and empty argv across explicit template Command and Args", func(specCtx SpecContext) {
		ctx = specCtx
		command := []string{"bash", "-c", `printf '%s\000' "$@" > /tmp/deferred-argv; printf '%s' "$VALUE" > /tmp/deferred-value; printf '%s' template > /tmp/deferred-started; exec tail -f /dev/null`, "_"}
		args := []string{"a b'c\"d", "line one\nline two", "", "$(touch /tmp/unexpected-args); $HOME `false`", "last"}
		pool, warm := newPool(agentsv1alpha1.StartupModeDeferred, command, args, false)
		pod := readyPod(Default, warm)
		Expect(pod.Spec.Containers[0].Args).To(Equal(append(append([]string{"--"}, command...), args...)))
		got, err := podExec(warm, "test", "!", "-e", "/tmp/deferred-argv")
		Expect(err).NotTo(HaveOccurred(), got)
		box := waitForClaim(newClaim(pool, map[string]string{"VALUE": "argv-value"}, nil, false))
		Expect(box.Status.PodInfo.PodUID).To(Equal(pod.UID))
		checkApplication(box, "template", "argv-value")
		got, err = podExec(box, "cat", "/tmp/deferred-argv")
		Expect(err).NotTo(HaveOccurred(), got)
		Expect(got).To(Equal(strings.Join(args, "\x00") + "\x00"))
		got, err = podExec(box, "test", "!", "-e", "/tmp/unexpected-args")
		Expect(err).NotTo(HaveOccurred(), got)
		checkSignal(box)
	})

	DescribeTable("keeps launcher environment isolated while SDK exec follows init defaults", func(specCtx SpecContext, skipInit bool) {
		ctx = specCtx
		pool, warm := newPool(agentsv1alpha1.StartupModeDeferred, appCommand("template"), nil, false)
		// Forward the native envd port so this case also works for out-of-cluster CI runners.
		cfg := config.GetConfigOrDie()
		transport, upgrader, err := spdy.RoundTripperFor(cfg)
		Expect(err).NotTo(HaveOccurred())
		url := clientset.CoreV1().RESTClient().Post().Resource("pods").Namespace(namespace).Name(warm.Name).SubResource("portforward").URL()
		stop, ready := make(chan struct{}), make(chan struct{})
		var forwardOut, forwardErr bytes.Buffer
		forwarder, err := portforward.NewOnAddresses(spdy.NewDialer(upgrader, &http.Client{Transport: transport, Timeout: 10 * time.Second}, "POST", url),
			[]string{"127.0.0.1"}, []string{fmt.Sprintf("0:%d", utils.RuntimePort)}, stop, ready, &forwardOut, &forwardErr)
		Expect(err).NotTo(HaveOccurred())
		finished := make(chan struct{})
		var forwardResult error
		go func() {
			forwardResult = forwarder.ForwardPorts()
			close(finished)
		}()
		defer func() {
			close(stop)
			Eventually(finished, 10*time.Second).Should(BeClosed())
			Expect(forwardResult).NotTo(HaveOccurred(), forwardErr.String())
		}()
		select {
		case <-ready:
		case <-finished:
			Fail(fmt.Sprintf("envd port-forward failed: %v", forwardResult))
		case <-specCtx.Done():
			Fail("envd port-forward cancelled")
		case <-time.After(10 * time.Second):
			Fail("envd port-forward did not become ready")
		}
		ports, err := forwarder.GetPorts()
		Expect(err).NotTo(HaveOccurred())
		Expect(ports).To(HaveLen(1))
		checkBaseline := func(box *agentsv1alpha1.Sandbox, sdkValue string) {
			got, err := podExec(box, "bash", "-c", `printf '%s' "$VALUE"`)
			Expect(err).NotTo(HaveOccurred(), got)
			Expect(got).To(Equal("pool-value"))
			got, err = podExec(box, "bash", "-c", `count=0
for executable in /proc/[0-9]*/exe; do
    target=$(readlink "$executable" 2>/dev/null) || continue
    [[ "$target" == /mnt/envd/envd ]] || continue
    count=$((count + 1))
    while IFS= read -r -d '' entry; do
        case "$entry" in VALUE=*) printf '%s' "${entry#VALUE=}";; esac
    done < "${executable%/exe}/environ"
done
[[ "$count" == 1 ]]`)
			Expect(err).NotTo(HaveOccurred(), got)
			Expect(got).To(Equal("pool-value"))
			native := box.DeepCopy()
			native.Annotations[agentsv1alpha1.AnnotationRuntimeURL] = fmt.Sprintf("http://127.0.0.1:%d", ports[0].Local)
			result, err := runtimeclient.NewRuntime(native).Process().Run(ctx, runtimeclient.RunCommandRequest{
				ProcessConfig: &process.ProcessConfig{Cmd: "bash", Args: []string{"-c", `printf '%s' "$VALUE"`}},
				Timeout:       10 * time.Second, ProcessTimeout: 10 * time.Second, AuthUser: "root",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Error).NotTo(HaveOccurred())
			Expect(result.Exited).To(BeTrue())
			Expect(result.ExitCode).To(BeZero(), strings.Join(result.Stderr, ""))
			Expect(strings.Join(result.Stdout, "")).To(Equal(sdkValue))
		}
		checkBaseline(warm, "")
		box := waitForClaim(newClaim(pool, map[string]string{"VALUE": "claim-env-value"}, nil, skipInit))
		Expect(box.Status.PodInfo.PodUID).To(Equal(warm.Status.PodInfo.PodUID))
		checkApplication(box, "template", "claim-env-value")
		sdkValue := "claim-env-value"
		if skipInit {
			sdkValue = ""
		}
		// The HTTP handshake timeout must not terminate the upgraded SPDY stream.
		Consistently(finished, 11*time.Second, pollInterval).ShouldNot(BeClosed())
		checkBaseline(box, sdkValue)
		checkSignal(box)
	}, Entry("with initialization", false), Entry("when initialization is skipped", true))

	It("passes exec readiness and liveness before claim then runs both real probes without restart", func(specCtx SpecContext) {
		ctx = specCtx
		probeCommand := func(marker string) []string {
			return []string{"bash", "-c", `test "$VALUE" = probe-value && test -f /tmp/deferred-started && printf '%s' "$VALUE" > "$1"`, "_", marker}
		}
		readiness := probeCommand("/tmp/deferred-readiness")
		liveness := probeCommand("/tmp/deferred-liveness")
		pool, warm := newPool(agentsv1alpha1.StartupModeDeferred, appCommand("template"), nil, false, func(p *agentsv1alpha1.SandboxSet) {
			main := &p.Spec.Template.Spec.Containers[0]
			main.ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: readiness}}, PeriodSeconds: 1, TimeoutSeconds: 2, FailureThreshold: 3}
			main.LivenessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: liveness}}, PeriodSeconds: 1, TimeoutSeconds: 2, FailureThreshold: 3}
		})
		pod := readyPod(Default, warm)
		Expect(pod.Spec.Containers[0].ReadinessProbe.Exec.Command).To(Equal(append([]string{"/bin/bash", utils.RunWithEnvsScript, "--probe", "--"}, readiness...)))
		Expect(pod.Spec.Containers[0].LivenessProbe.Exec.Command).To(Equal(append([]string{"/bin/bash", utils.RunWithEnvsScript, "--probe", "--"}, liveness...)))
		Consistently(func(g Gomega) {
			current := readyPod(g, warm)
			g.Expect(current.UID).To(Equal(pod.UID))
			g.Expect(current.Status.ContainerStatuses[0].RestartCount).To(BeZero())
			got, err := podExec(warm, "bash", "-c", "test ! -e /tmp/deferred-started && test ! -e /tmp/deferred-readiness && test ! -e /tmp/deferred-liveness")
			g.Expect(err).NotTo(HaveOccurred(), got)
		}, 6*time.Second, pollInterval).Should(Succeed())
		box := waitForClaim(newClaim(pool, map[string]string{"VALUE": "probe-value"}, nil, false))
		checkApplication(box, "template", "probe-value")
		Eventually(func(g Gomega) {
			for _, marker := range []string{"/tmp/deferred-readiness", "/tmp/deferred-liveness"} {
				got, err := podExec(box, "cat", marker)
				g.Expect(err).NotTo(HaveOccurred(), got)
				g.Expect(got).To(Equal("probe-value"))
			}
		}, waitTimeout, pollInterval).Should(Succeed())
		Consistently(func(g Gomega) {
			current := readyPod(g, box)
			g.Expect(current.UID).To(Equal(pod.UID))
			g.Expect(current.Status.ContainerStatuses[0].RestartCount).To(BeZero())
		}, 6*time.Second, pollInterval).Should(Succeed())
		checkSignal(box)
	})

	It("ignores a stale wrong-UID readiness file until claim publishes the actual Pod UID", func(specCtx SpecContext) {
		ctx = specCtx
		pool, warm := newPool(agentsv1alpha1.StartupModeDeferred, appCommand("template"), nil, false)
		pod := readyPod(Default, warm)
		got, err := podExec(warm, "bash", "-c", `umask 077; printf '%s' stale-pod-uid > /mnt/envd/claim/.env_ready`)
		Expect(err).NotTo(HaveOccurred(), got)
		Consistently(func(g Gomega) {
			current := readyPod(g, warm)
			g.Expect(current.UID).To(Equal(pod.UID))
			g.Expect(current.Status.ContainerStatuses[0].RestartCount).To(BeZero())
			got, err := podExec(warm, "bash", "-c", "test ! -e /tmp/deferred-started && cat /mnt/envd/claim/.env_ready")
			g.Expect(err).NotTo(HaveOccurred(), got)
			g.Expect(got).To(Equal("stale-pod-uid"))
		}, 5*time.Second, pollInterval).Should(Succeed())
		box := waitForClaim(newClaim(pool, map[string]string{"VALUE": "fresh-value"}, appCommand("claim"), false))
		Expect(box.Status.PodInfo.PodUID).To(Equal(pod.UID))
		checkApplication(box, "claim", "fresh-value")
		checkSignal(box)
	})

	It("replays immutable startup files after a targeted same-Pod main container restart", func(specCtx SpecContext) {
		ctx = specCtx
		pool, _ := newPool(agentsv1alpha1.StartupModeDeferred, appCommand("template"), nil, false, func(p *agentsv1alpha1.SandboxSet) {
			p.Spec.Template.Spec.TerminationGracePeriodSeconds = ptr.To[int64](1)
			p.Spec.Template.Spec.Containers[0].LivenessProbe = &corev1.Probe{
				ProbeHandler:  corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"bash", "-c", "test ! -e /mnt/envd/claim/restart-request"}}},
				PeriodSeconds: 1, TimeoutSeconds: 2, FailureThreshold: 1,
			}
		})
		command := []string{"bash", "-c", `trap '' TERM; rm -f /mnt/envd/claim/restart-request; printf '%s\n' started >> /mnt/envd/claim/application-runs; printf '%s' "$VALUE" > /tmp/deferred-value; printf '%s' "$1" > /tmp/deferred-started; exec tail -f /dev/null`, "_", "restart 'quoted'\ncommand"}
		box := waitForClaim(newClaim(pool, map[string]string{"VALUE": "restart 'value'\nline"}, command, false))
		checkApplication(box, command[4], "restart 'value'\nline")
		checkSignal(box)
		pod := readyPod(Default, box)
		Expect(pod.Status.ContainerStatuses[0].RestartCount).To(BeZero())
		checksumCommand := []string{"bash", "-c", "sha256sum /mnt/envd/claim/env.sh /mnt/envd/claim/cmd.sh /mnt/envd/claim/.env_ready"}
		before, err := podExec(box, checksumCommand...)
		Expect(err).NotTo(HaveOccurred(), before)
		got, err := podExec(box, "cat", "/mnt/envd/claim/application-runs")
		Expect(err).NotTo(HaveOccurred(), got)
		Expect(got).To(Equal("started\n"))
		By("Failing only this sandbox's liveness probe so kubelet forces one main-container restart")
		killOutput, killErr := podExec(box, "touch", "/mnt/envd/claim/restart-request")
		Eventually(func(g Gomega) {
			current := readyPod(g, box)
			g.Expect(current.UID).To(Equal(pod.UID))
			status := current.Status.ContainerStatuses[0]
			g.Expect(status.RestartCount).To(Equal(int32(1)))
			g.Expect(status.ContainerID).NotTo(Equal(pod.Status.ContainerStatuses[0].ContainerID))
			g.Expect(status.LastTerminationState.Terminated).NotTo(BeNil())
			g.Expect(status.LastTerminationState.Terminated.ExitCode).To(Equal(int32(137)))
			got, err := podExec(box, "cat", "/mnt/envd/claim/application-runs")
			g.Expect(err).NotTo(HaveOccurred(), got)
			g.Expect(got).To(Equal("started\nstarted\n"))
		}, waitTimeout, pollInterval).Should(Succeed(), "kill request returned %v: %s", killErr, killOutput)
		checkApplication(box, command[4], "restart 'value'\nline")
		after, err := podExec(box, checksumCommand...)
		Expect(err).NotTo(HaveOccurred(), after)
		Expect(after).To(Equal(before))
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(box), box)).To(Succeed())
		request, err := runtimeclient.GetInitRuntimeRequest(box)
		Expect(err).NotTo(HaveOccurred())
		Expect(request.Command).To(Equal(command))
		checkSignal(box)
	})

	It("recovers startup publication after killing a lock holder without reclaiming a live publisher", func(specCtx SpecContext) {
		ctx = specCtx
		pool, warm := newPool(agentsv1alpha1.StartupModeDeferred, appCommand("template"), nil, false)
		pod := readyPod(Default, warm)
		got, err := podExec(warm, "bash", "-c", `bash -c 'exec 9>/mnt/envd/claim/.startup-publish.lock; flock -n 9; printf "%s" "$BASHPID" > /tmp/deferred-publisher-pid; exec sleep 600' >/dev/null 2>&1 &`)
		Expect(err).NotTo(HaveOccurred(), got)
		Eventually(func() error {
			_, err := podExec(warm, "test", "-s", "/tmp/deferred-publisher-pid")
			return err
		}, waitTimeout, pollInterval).Should(Succeed())
		callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = runtimeclient.InjectStartupData(callCtx, warm, runtimeconfig.StartupData{
			EnvVars: map[string]string{"VALUE": "must-not-publish"}, Command: appCommand("must-not-start"),
		})
		cancel()
		Expect(err).To(HaveOccurred(), "a live publisher must retain exclusive ownership")
		got, err = podExec(warm, "bash", "-c", `kill -0 "$(cat /tmp/deferred-publisher-pid)" && test ! -e /mnt/envd/claim/.env_ready && test ! -e /tmp/deferred-started`)
		Expect(err).NotTo(HaveOccurred(), got)
		got, err = podExec(warm, "bash", "-c", `kill -KILL "$(cat /tmp/deferred-publisher-pid)"`)
		Expect(err).NotTo(HaveOccurred(), got)
		Eventually(func() error {
			_, err := podExec(warm, "flock", "-n", "/mnt/envd/claim/.startup-publish.lock", "true")
			return err
		}, waitTimeout, pollInterval).Should(Succeed())
		box := waitForClaim(newClaim(pool, map[string]string{"VALUE": "recovered-value"}, appCommand("recovered-command"), false))
		Expect(box.Status.PodInfo.PodUID).To(Equal(pod.UID))
		checkApplication(box, "recovered-command", "recovered-value")
		checkSignal(box)
	})

	Describe("manager HTTP", func() {
		var managerURL, apiKey string

		BeforeEach(func() {
			managerURL = strings.TrimRight(os.Getenv("E2E_DEFERRED_STARTUP_MANAGER_URL"), "/")
			if managerURL == "" {
				Skip("E2E_DEFERRED_STARTUP_MANAGER_URL is not set")
			}
			apiKey = os.Getenv(apiKeyEnv)
			if apiKey == "" {
				apiKey = defaultManagerAPIKey
			}
		})

		cleanupSandbox := func(box *agentsv1alpha1.Sandbox) {
			owned := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: box.Name, Namespace: box.Namespace, UID: box.UID}}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: box.Name, Namespace: box.Namespace, UID: box.Status.PodInfo.PodUID}}
			DeferCleanup(func(cleanupCtx SpecContext) {
				if CurrentSpecReport().Failed() && os.Getenv("E2E_KEEP_FAILED_NS") == "true" {
					return
				}
				Expect(client.IgnoreNotFound(k8sClient.Delete(cleanupCtx, owned, client.Preconditions{UID: &owned.UID}))).To(Succeed())
				// Deleting this Pod also terminates its independent background lock holder.
				Expect(client.IgnoreNotFound(k8sClient.Delete(cleanupCtx, pod, client.Preconditions{UID: &pod.UID}))).To(Succeed())
				Eventually(func(g Gomega) {
					g.Expect(apierrors.IsNotFound(k8sClient.Get(cleanupCtx, client.ObjectKeyFromObject(owned), &agentsv1alpha1.Sandbox{}))).To(BeTrue())
					g.Expect(apierrors.IsNotFound(k8sClient.Get(cleanupCtx, client.ObjectKeyFromObject(pod), &corev1.Pod{}))).To(BeTrue())
				}, waitTimeout, pollInterval).Should(Succeed())
			})
		}

		managerPool := func() (*agentsv1alpha1.SandboxSet, *agentsv1alpha1.Sandbox) {
			pool, warm := newPool(agentsv1alpha1.StartupModeDeferred, appCommand("template"), nil, false, func(p *agentsv1alpha1.SandboxSet) {
				p.Namespace = Namespace
			})
			cleanupSandbox(warm)
			got, err := podExec(warm, "bash", "-c", "test ! -e /tmp/deferred-started && test ! -e /mnt/envd/claim/.env_ready")
			Expect(err).NotTo(HaveOccurred(), got)
			return pool, warm
		}

		createSandbox := func(pool *agentsv1alpha1.SandboxSet, value string, neverTimeout bool) (int, []byte) {
			metadata := map[string]string{
				e2bmodels.ExtensionKeyClaimTimeout:            "180",
				e2bmodels.ExtensionKeyWaitReadyTimeout:        "180",
				e2bmodels.ExtensionKeyReserveFailedSandboxFor: "0",
				e2bmodels.ExtensionKeyCreateOnNoStock:         agentsv1alpha1.False,
			}
			if neverTimeout {
				metadata[e2bmodels.ExtensionKeyNeverTimeout] = agentsv1alpha1.True
			}
			body, err := json.Marshal(map[string]any{
				"templateID": pool.Name, "timeout": 300, "envVars": map[string]string{"VALUE": value}, "metadata": metadata,
			})
			Expect(err).NotTo(HaveOccurred())
			callCtx, cancel := context.WithTimeout(ctx, 4*time.Minute)
			defer cancel()
			req, err := http.NewRequestWithContext(callCtx, http.MethodPost, managerURL+"/sandboxes", bytes.NewReader(body))
			Expect(err == nil).To(BeTrue(), "cannot construct manager HTTP request")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(e2bmodels.HeaderApiKey, apiKey)
			httpClient := &http.Client{Timeout: 4 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			defer httpClient.CloseIdleConnections()
			resp, err := httpClient.Do(req)
			Expect(err == nil).To(BeTrue(), "manager HTTP request failed")
			const maxResponseBytes = 64 << 10
			responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
			closeErr := resp.Body.Close()
			Expect(readErr == nil).To(BeTrue(), "cannot read manager HTTP response")
			Expect(closeErr == nil).To(BeTrue(), "cannot close manager HTTP response")
			Expect(len(responseBody)).To(BeNumerically("<=", maxResponseBytes), "manager response exceeds 64 KiB")
			return resp.StatusCode, responseBody
		}

		createdSandbox := func(pool *agentsv1alpha1.SandboxSet, body []byte, value string) *agentsv1alpha1.Sandbox {
			// Decode only resource metadata; never print the response's access tokens.
			var created struct {
				Metadata map[string]string `json:"metadata"`
			}
			Expect(json.Unmarshal(body, &created) == nil).To(BeTrue(), "invalid manager create response")
			parts := strings.SplitN(created.Metadata[e2bmodels.MetadataKeySandboxResource], "/", 2)
			Expect(len(parts)).To(Equal(2), "missing sandbox-resource metadata")
			Expect(parts[0] == pool.Namespace && parts[1] != "").To(BeTrue(), "unexpected sandbox-resource metadata")
			box := &agentsv1alpha1.Sandbox{}
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: parts[0], Name: parts[1]}, box)).To(Succeed())
				g.Expect(box.Status.PodInfo.PodUID).NotTo(BeEmpty())
			}, waitTimeout, pollInterval).Should(Succeed())
			cleanupSandbox(box)
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(box), box)).To(Succeed())
				g.Expect(utils.IsSandboxReady(box)).To(BeTrue())
				readyPod(g, box)
				g.Expect(box.Labels[agentsv1alpha1.LabelSandboxPool]).To(Equal(pool.Name))
				g.Expect(box.Labels[agentsv1alpha1.LabelSandboxIsClaimed]).To(Equal(agentsv1alpha1.True))
				g.Expect(box.Labels[agentsv1alpha1.LabelSandboxClaimMethod]).To(Equal(agentsv1alpha1.ClaimMethodHotStart))
				g.Expect(box.Labels[agentsv1alpha1.LabelSandboxClaimName]).To(BeEmpty())
				request, err := runtimeclient.GetInitRuntimeRequest(box)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(request).NotTo(BeNil())
				g.Expect(request.EnvVars).To(Equal(map[string]string{"VALUE": value}))
			}, waitTimeout, pollInterval).Should(Succeed())
			checkApplication(box, "template", value)
			checkSignal(box)
			return box
		}

		It("retries only publication after one known exit 42 on the same warm Pod", func(specCtx SpecContext) {
			ctx = specCtx
			pool, warm := managerPool()
			got, err := podExec(warm, "bash", "-c", `set -e
cp -- /usr/bin/flock /tmp/deferred-original-flock
cat > /usr/bin/flock <<'SCRIPT'
#!/bin/bash
if [[ ! -e /tmp/deferred-flock-calls ]]; then
    printf '%s\n' 42 > /tmp/deferred-flock-calls
    exit 42
fi
printf '%s\n' retry >> /tmp/deferred-flock-calls
exec /tmp/deferred-original-flock "$@"
SCRIPT`)
			Expect(err).NotTo(HaveOccurred(), got)
			status, body := createSandbox(pool, "manager-retry-value", false)
			Expect(status).To(Equal(http.StatusCreated))
			box := createdSandbox(pool, body, "manager-retry-value")
			Expect(box.UID).To(Equal(warm.UID))
			Expect(box.Status.PodInfo.PodUID).To(Equal(warm.Status.PodInfo.PodUID))
			got, err = podExec(box, "cat", "/tmp/deferred-flock-calls")
			Expect(err).NotTo(HaveOccurred(), got)
			Expect(got).To(Equal("42\nretry\n"))
			Expect(readyPod(Default, box).Status.ContainerStatuses[0].RestartCount).To(BeZero())
		})

		It("deletes a known failed warm Pod with reserve-failed zero and claims healthy replenished stock", func(specCtx SpecContext) {
			ctx = specCtx
			pool, warm := managerPool()
			got, err := podExec(warm, "bash", "-c", `set -e
cp -- /usr/bin/flock /tmp/deferred-original-flock
cat > /usr/bin/flock <<'SCRIPT'
#!/bin/bash
exit 42
SCRIPT`)
			Expect(err).NotTo(HaveOccurred(), got)
			status, body := createSandbox(pool, "manager-replacement-value", false)
			Expect(status).To(Equal(http.StatusCreated))
			box := createdSandbox(pool, body, "manager-replacement-value")
			Expect(box.UID).NotTo(Equal(warm.UID))
			Expect(box.Status.PodInfo.PodUID).NotTo(Equal(warm.Status.PodInfo.PodUID))
			Eventually(func(g Gomega) {
				g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(warm), &agentsv1alpha1.Sandbox{}))).To(BeTrue())
				g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(warm), &corev1.Pod{}))).To(BeTrue())
			}, waitTimeout, pollInterval).Should(Succeed())
		})

		It("preserves a never-timeout claimed sandbox after a lock-busy unknown publication", func(specCtx SpecContext) {
			ctx = specCtx
			pool, warm := managerPool()
			sandboxUID, podUID := warm.UID, warm.Status.PodInfo.PodUID
			got, err := podExec(warm, "bash", "-c", `bash -c 'exec 9>>/mnt/envd/claim/.startup-publish.lock; /usr/bin/flock -n 9 || exit; printf "%s" "$BASHPID" > /tmp/held; exec sleep 600' </dev/null >/dev/null 2>&1 &`)
			Expect(err).NotTo(HaveOccurred(), got)
			Eventually(func() error {
				_, err := podExec(warm, "test", "-s", "/tmp/held")
				return err
			}, waitTimeout, pollInterval).Should(Succeed())
			status, body := createSandbox(pool, "manager-locked-value", true)
			Expect(status).To(Equal(http.StatusInternalServerError))
			var failure struct {
				Message string `json:"message"`
			}
			Expect(json.Unmarshal(body, &failure) == nil).To(BeTrue(), "invalid manager error response")
			Expect(strings.Contains(failure.Message, runtimeclient.ErrStartupPublicationUncertain.Error())).To(BeTrue(), "manager must report uncertain startup publication")
			checkPending := func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(warm), warm)).To(Succeed())
				g.Expect(warm.UID).To(Equal(sandboxUID))
				g.Expect(warm.Status.PodInfo.PodUID).To(Equal(podUID))
				g.Expect(warm.DeletionTimestamp.IsZero()).To(BeTrue())
				g.Expect(warm.Labels[agentsv1alpha1.LabelSandboxIsClaimed]).To(Equal(agentsv1alpha1.True))
				g.Expect(warm.Spec.ShutdownTime).To(BeNil())
				g.Expect(warm.Spec.PauseTime).To(BeNil())
				g.Expect(warm.Labels[agentsv1alpha1.LabelSandboxReservedFailed]).NotTo(Equal(agentsv1alpha1.True))
				request, err := runtimeclient.GetInitRuntimeRequest(warm)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(request).NotTo(BeNil())
				g.Expect(request.StartupDelivered).To(BeFalse())
				g.Expect(request.EnvVars).To(Equal(map[string]string{"VALUE": "manager-locked-value"}))
				pod := readyPod(g, warm)
				g.Expect(pod.Status.ContainerStatuses[0].RestartCount).To(BeZero())
				got, err := podExec(warm, "bash", "-c", `kill -0 "$(cat /tmp/held)" && test ! -e /mnt/envd/claim/.env_ready && test ! -e /tmp/deferred-started`)
				g.Expect(err).NotTo(HaveOccurred(), got)
			}
			Eventually(checkPending, waitTimeout, pollInterval).Should(Succeed())
			Consistently(checkPending, 6*time.Second, pollInterval).Should(Succeed())
		})
	})

	It("preserves a claimed PID-1 TERM handler and exit code across one container restart", func(specCtx SpecContext) {
		ctx = specCtx
		pool, _ := newPool(agentsv1alpha1.StartupModeDeferred, nil, nil, false)
		command := []string{"bash", "-c", `trap 'printf "%s" "$VALUE" > /mnt/envd/claim/term-value; exit 42' TERM; printf '%s\n' started >> /mnt/envd/claim/term-runs; while true; do sleep 1; done`}
		box := waitForClaim(newClaim(pool, map[string]string{"VALUE": "term-value"}, command, false))
		checkSignal(box)
		Eventually(func() (string, error) { return podExec(box, "cat", "/mnt/envd/claim/term-runs") }, waitTimeout, pollInterval).Should(Equal("started\n"))
		pod := readyPod(Default, box)
		termOutput, termErr := podExec(box, "bash", "-c", "kill -TERM 1")
		Eventually(func(g Gomega) {
			current := readyPod(g, box)
			g.Expect(current.UID).To(Equal(pod.UID))
			status := current.Status.ContainerStatuses[0]
			g.Expect(status.RestartCount).To(Equal(int32(1)))
			g.Expect(status.LastTerminationState.Terminated).NotTo(BeNil())
			g.Expect(status.LastTerminationState.Terminated.ExitCode).To(Equal(int32(42)))
			value, err := podExec(box, "cat", "/mnt/envd/claim/term-value")
			g.Expect(err).NotTo(HaveOccurred(), value)
			g.Expect(value).To(Equal("term-value"))
			value, err = podExec(box, "cat", "/mnt/envd/claim/term-runs")
			g.Expect(err).NotTo(HaveOccurred(), value)
			g.Expect(value).To(Equal("started\nstarted\n"))
		}, waitTimeout, pollInterval).Should(Succeed(), "TERM request returned %v: %s", termErr, termOutput)
		checkSignal(box)
	})

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

	It("keeps two templateRef pools sharing one SandboxTemplate isolated", func(specCtx SpecContext) {
		ctx = specCtx
		firstPool, firstWarm := newPool(agentsv1alpha1.StartupModeDeferred, appCommand("template"), nil, true)
		secondPool, secondWarm := newPool(agentsv1alpha1.StartupModeDeferred, nil, nil, false, func(p *agentsv1alpha1.SandboxSet) {
			p.Spec.Template = nil
			p.Spec.TemplateRef = firstPool.Spec.TemplateRef.DeepCopy()
		})
		Expect(secondPool.Spec.TemplateRef.Name).To(Equal(firstPool.Spec.TemplateRef.Name))
		template := &agentsv1alpha1.SandboxTemplate{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: firstPool.Spec.TemplateRef.Name}, template)).To(Succeed())
		originalSpec := template.Spec.DeepCopy()
		firstClaim := newClaim(firstPool, map[string]string{"VALUE": "first-shared"}, appCommand("first-command"), false)
		secondClaim := newClaim(secondPool, map[string]string{"VALUE": "second-shared"}, nil, false)
		first, second := waitForClaim(firstClaim), waitForClaim(secondClaim)
		Expect(first.Name).To(Equal(firstWarm.Name))
		Expect(second.Name).To(Equal(secondWarm.Name))
		Expect(first.Status.PodInfo.PodUID).NotTo(Equal(second.Status.PodInfo.PodUID))
		Expect(first.Annotations[agentsv1alpha1.AnnotationOwner]).To(Equal(string(firstClaim.UID)))
		Expect(second.Annotations[agentsv1alpha1.AnnotationOwner]).To(Equal(string(secondClaim.UID)))
		checkApplication(first, "first-command", "first-shared")
		checkApplication(second, "template", "second-shared")
		checkSignal(first)
		checkSignal(second)
		got, err := podExec(second, "test", "!", "-e", utils.SandboxEnvClaimDir+"/cmd.sh")
		Expect(err).NotTo(HaveOccurred(), got)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(template), template)).To(Succeed())
		Expect(template.Spec).To(Equal(*originalSpec), "claim must not mutate the shared template")
	})

	It("claims three distinct warm replicas as one batch without duplicate ownership", func(specCtx SpecContext) {
		ctx = specCtx
		pool, _ := newPool(agentsv1alpha1.StartupModeDeferred, appCommand("template"), nil, false, func(p *agentsv1alpha1.SandboxSet) {
			p.Spec.Replicas = 3
		})
		stock := &agentsv1alpha1.SandboxList{}
		Expect(k8sClient.List(ctx, stock, client.InNamespace(namespace), client.MatchingLabels{
			agentsv1alpha1.LabelSandboxPool: pool.Name, agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.False,
		})).To(Succeed())
		Expect(stock.Items).To(HaveLen(3))
		warmUIDs := make(map[string]types.UID, 3)
		for i := range stock.Items {
			warm := &stock.Items[i]
			warmUIDs[warm.Name] = readyPod(Default, warm).UID
			got, err := podExec(warm, "test", "!", "-e", "/tmp/deferred-started")
			Expect(err).NotTo(HaveOccurred(), got)
		}
		claim := newClaim(pool, map[string]string{"VALUE": "batch-value"}, appCommand("batch-command"), false, func(c *agentsv1alpha1.SandboxClaim) {
			c.Spec.Replicas = ptr.To[int32](3)
			c.Spec.CreateOnNoStock = false
		})
		waitForClaim(claim)
		claimed := &agentsv1alpha1.SandboxList{}
		Expect(k8sClient.List(ctx, claimed, client.InNamespace(namespace), client.MatchingLabels{agentsv1alpha1.LabelSandboxClaimName: claim.Name})).To(Succeed())
		Expect(claimed.Items).To(HaveLen(3))
		seen := make(map[types.UID]bool, 3)
		for i := range claimed.Items {
			box := &claimed.Items[i]
			Expect(box.Annotations[agentsv1alpha1.AnnotationOwner]).To(Equal(string(claim.UID)))
			Expect(warmUIDs).To(HaveKeyWithValue(box.Name, box.Status.PodInfo.PodUID))
			Expect(seen[box.Status.PodInfo.PodUID]).To(BeFalse())
			seen[box.Status.PodInfo.PodUID] = true
			checkApplication(box, "batch-command", "batch-value")
			checkSignal(box)
		}
		// Completed claims are terminal: later reconciles must not allocate extra deliveries.
		Consistently(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(claim), claim)).To(Succeed())
			g.Expect(claim.Status.ClaimedReplicas).To(Equal(int32(3)))
			list := &agentsv1alpha1.SandboxList{}
			g.Expect(k8sClient.List(ctx, list, client.InNamespace(namespace), client.MatchingLabels{agentsv1alpha1.LabelSandboxClaimName: claim.Name})).To(Succeed())
			g.Expect(list.Items).To(HaveLen(3))
		}, 5*time.Second, pollInterval).Should(Succeed())
	})

	It("cold-creates a Deferred sandbox from an empty pool when CreateOnNoStock is true", func(specCtx SpecContext) {
		ctx = specCtx
		pool, warm := newPool(agentsv1alpha1.StartupModeDeferred, appCommand("template"), nil, false, func(p *agentsv1alpha1.SandboxSet) {
			p.Spec.Replicas = 0
		})
		Expect(warm).To(BeNil())
		stock := &agentsv1alpha1.SandboxList{}
		Expect(k8sClient.List(ctx, stock, client.InNamespace(namespace))).To(Succeed())
		Expect(stock.Items).To(BeEmpty())
		claim := newClaim(pool, map[string]string{"VALUE": "cold-value"}, appCommand("cold-command"), false, func(c *agentsv1alpha1.SandboxClaim) {
			c.Spec.CreateOnNoStock = true
		})
		box := waitForClaim(claim)
		Expect(box.Labels[agentsv1alpha1.LabelSandboxClaimMethod]).To(Equal(string(agentsv1alpha1.ClaimMethodColdStart)))
		checkApplication(box, "cold-command", "cold-value")
		checkSignal(box)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), pool)).To(Succeed())
		Expect(pool.Spec.Replicas).To(BeZero())
	})

	It("recovers an initial completion record before Stop resume without executing the application twice", func(specCtx SpecContext) {
		ctx = specCtx
		pool, _ := newPool(agentsv1alpha1.StartupModeDeferred, nil, nil, false)
		command := []string{"bash", "-c", `printf '%s\n' started >> /mnt/envd/claim/completion-runs; printf '%s' "$VALUE" > /tmp/deferred-value; printf '%s' completion-command > /tmp/deferred-started; exec tail -f /dev/null`}
		box := waitForClaim(newClaim(pool, map[string]string{"VALUE": "completion-value"}, command, false))
		checkApplication(box, "completion-command", "completion-value")
		oldPod := readyPod(Default, box)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(box), box)).To(Succeed())
		original := box.DeepCopy()
		conditions := make([]metav1.Condition, 0, len(box.Status.Conditions))
		for _, condition := range box.Status.Conditions {
			if condition.Type != string(agentsv1alpha1.RuntimeInitialized) {
				conditions = append(conditions, condition)
			}
		}
		box.Status.Conditions = conditions
		Expect(k8sClient.Status().Patch(ctx, box, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}))).To(Succeed())
		original = box.DeepCopy()
		request, err := runtimeclient.GetInitRuntimeRequest(box)
		Expect(err).NotTo(HaveOccurred())
		request.StartupDelivered = false
		raw, err := json.Marshal(request)
		Expect(err).NotTo(HaveOccurred())
		box.Annotations[agentsv1alpha1.AnnotationInitRuntimeRequest] = string(raw)
		Expect(k8sClient.Patch(ctx, box, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}))).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(box), box)).To(Succeed())
			pod := readyPod(g, box)
			g.Expect(pod.UID).To(Equal(oldPod.UID))
			g.Expect(pod.Status.ContainerStatuses[0].RestartCount).To(BeZero())
			request, err := runtimeclient.GetInitRuntimeRequest(box)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(request.StartupDelivered).To(BeTrue())
			g.Expect(request.StartupPodUID).To(Equal(string(pod.UID)))
			initialized := utils.GetSandboxCondition(&box.Status, string(agentsv1alpha1.RuntimeInitialized))
			g.Expect(initialized).NotTo(BeNil())
			g.Expect(initialized.Status).To(Equal(metav1.ConditionTrue))
			got, err := podExec(box, "cat", "/mnt/envd/claim/completion-runs")
			g.Expect(err).NotTo(HaveOccurred(), got)
			g.Expect(got).To(Equal("started\n"))
		}, waitTimeout, pollInterval).Should(Succeed())
		checkSignal(box)
		box.Spec.Paused = true
		Expect(updateSandboxSpec(ctx, box)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(box), box)).To(Succeed())
			g.Expect(box.Status.Phase).To(Equal(agentsv1alpha1.SandboxPaused))
			g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(box), &corev1.Pod{}))).To(BeTrue())
		}, waitTimeout, pollInterval).Should(Succeed())
		box.Spec.Paused = false
		Expect(updateSandboxSpec(ctx, box)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(box), box)).To(Succeed())
			g.Expect(readyPod(g, box).UID).NotTo(Equal(oldPod.UID))
		}, waitTimeout, pollInterval).Should(Succeed())
		checkApplication(box, "completion-command", "completion-value")
		checkSignal(box)
	})

	It("fences delayed startup completion across Stop resume and replays the new Pod generation", func(specCtx SpecContext) {
		ctx = specCtx
		pool, _ := newPool(agentsv1alpha1.StartupModeDeferred, appCommand("template"), nil, false)
		command := appCommand("generation-command")
		box := waitForClaim(newClaim(pool, map[string]string{"VALUE": "generation 'value'\nline"}, command, false))
		checkApplication(box, "generation-command", "generation 'value'\nline")
		checkSignal(box)
		oldPod := readyPod(Default, box)
		box.Spec.Paused = true
		Expect(updateSandboxSpec(ctx, box)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(box), box)).To(Succeed())
			g.Expect(box.Status.Phase).To(Equal(agentsv1alpha1.SandboxPaused))
			g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(box), &corev1.Pod{}))).To(BeTrue())
		}, waitTimeout, pollInterval).Should(Succeed())
		By("Marking the old delivery unfinished only after its publication signal is no longer present")
		request, err := runtimeclient.GetInitRuntimeRequest(box)
		Expect(err).NotTo(HaveOccurred())
		Expect(request.StartupPodUID).To(Equal(string(oldPod.UID)))
		original := box.DeepCopy()
		request.StartupDelivered = false
		raw, err := json.Marshal(request)
		Expect(err).NotTo(HaveOccurred())
		box.Annotations[agentsv1alpha1.AnnotationInitRuntimeRequest] = string(raw)
		Expect(k8sClient.Patch(ctx, box, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}))).To(Succeed())
		box.Spec.Paused = false
		Expect(updateSandboxSpec(ctx, box)).To(Succeed())
		var newUID types.UID
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(box), box)).To(Succeed())
			pod := readyPod(g, box)
			g.Expect(pod.UID).NotTo(Equal(oldPod.UID))
			newUID = pod.UID
			condition := utils.GetSandboxCondition(&box.Status, string(agentsv1alpha1.RuntimeInitialized))
			g.Expect(condition).NotTo(BeNil())
			g.Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(condition.Reason).To(Equal("StartupDeliveryPending"))
			ready := utils.GetSandboxCondition(&box.Status, string(agentsv1alpha1.SandboxConditionReady))
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		}, waitTimeout, pollInterval).Should(Succeed())
		By("Native Pod readiness must not release the application or publish runtime initialization")
		Consistently(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(box), box)).To(Succeed())
			pod := readyPod(g, box)
			g.Expect(pod.UID).To(Equal(newUID))
			g.Expect(pod.Status.ContainerStatuses[0].RestartCount).To(BeZero())
			request, err := runtimeclient.GetInitRuntimeRequest(box)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(request.StartupDelivered).To(BeFalse())
			g.Expect(request.StartupPodUID).To(Equal(string(oldPod.UID)))
			condition := utils.GetSandboxCondition(&box.Status, string(agentsv1alpha1.RuntimeInitialized))
			g.Expect(condition).NotTo(BeNil())
			g.Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			got, err := podExec(box, "bash", "-c", "test ! -e /tmp/deferred-started && test ! -e /mnt/envd/claim/.env_ready")
			g.Expect(err).NotTo(HaveOccurred(), got)
		}, 6*time.Second, pollInterval).Should(Succeed())
		By("Completing the old delivery without changing its UID, then waiting for controller replay")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(box), box)).To(Succeed())
		original = box.DeepCopy()
		request, err = runtimeclient.GetInitRuntimeRequest(box)
		Expect(err).NotTo(HaveOccurred())
		request.StartupDelivered = true
		Expect(request.StartupPodUID).To(Equal(string(oldPod.UID)))
		raw, err = json.Marshal(request)
		Expect(err).NotTo(HaveOccurred())
		box.Annotations[agentsv1alpha1.AnnotationInitRuntimeRequest] = string(raw)
		Expect(k8sClient.Patch(ctx, box, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}))).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(box), box)).To(Succeed())
			pod := readyPod(g, box)
			g.Expect(pod.UID).To(Equal(newUID))
			g.Expect(pod.Status.ContainerStatuses[0].RestartCount).To(BeZero())
			request, err := runtimeclient.GetInitRuntimeRequest(box)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(request.StartupDelivered).To(BeTrue())
			g.Expect(request.StartupPodUID).To(Equal(string(pod.UID)))
			g.Expect(request.EnvVars).To(Equal(map[string]string{"VALUE": "generation 'value'\nline"}))
			g.Expect(request.Command).To(Equal(command))
			condition := utils.GetSandboxCondition(&box.Status, string(agentsv1alpha1.RuntimeInitialized))
			g.Expect(condition).NotTo(BeNil())
			g.Expect(condition.Status).To(Equal(metav1.ConditionTrue))
		}, waitTimeout, pollInterval).Should(Succeed())
		checkApplication(box, "generation-command", "generation 'value'\nline")
		checkSignal(box)
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
		box.Spec.Template.Spec.Containers[0].Image = altImage
		Expect(updateSandboxSpec(ctx, box)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(box), box)).To(Succeed())
			pod := &corev1.Pod{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(box), pod)).To(Succeed())
			g.Expect(pod.UID).NotTo(Equal(oldUID))
			g.Expect(pod.Spec.Containers[0].Image).To(Equal(altImage))
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
				Consistently(func(g Gomega) {
					pod := readyPod(g, warm)
					g.Expect(pod.Status.ContainerStatuses[0].RestartCount).To(BeZero())
					got, err := podExec(warm, "bash", "-c", "test ! -e /tmp/deferred-started && test ! -e /mnt/envd/claim/.env_ready && test ! -e /mnt/envd/claim/env.sh && test ! -e /mnt/envd/claim/cmd.sh")
					g.Expect(err).NotTo(HaveOccurred(), got)
				}, 3*time.Second, pollInterval).Should(Succeed())
			} else {
				checkApplication(warm, "template", "pool-value")
			}
		},
		Entry("Immediate pools reject a claim command", agentsv1alpha1.StartupModeImmediate, map[string]string(nil), appCommand("claim")),
		Entry("reserved wrapper variables are rejected", agentsv1alpha1.StartupModeDeferred, map[string]string{"_rwe_cmd_script": "/tmp/other"}, []string(nil)),
		Entry("invalid env names are rejected", agentsv1alpha1.StartupModeDeferred, map[string]string{"1INVALID": "value"}, []string(nil)),
		Entry("NUL env values are rejected", agentsv1alpha1.StartupModeDeferred, map[string]string{"VALUE": "before\x00after"}, []string(nil)),
		Entry("env values exceeding 4096 bytes are rejected", agentsv1alpha1.StartupModeDeferred, map[string]string{"VALUE": strings.Repeat("x", 4097)}, []string(nil)),
		Entry("more than 32 env variables are rejected", agentsv1alpha1.StartupModeDeferred, func() map[string]string {
			env := make(map[string]string, 33)
			for i := 0; i < 33; i++ {
				env[fmt.Sprintf("VALUE_%d", i)] = "value"
			}
			return env
		}(), []string(nil)),
		Entry("quote-expanded env scripts exceeding the shared budget are rejected", agentsv1alpha1.StartupModeDeferred,
			map[string]string{"FIRST": strings.Repeat("'", 4096), "SECOND": strings.Repeat("'", 4096)}, []string(nil)),
		Entry("empty command executables are rejected", agentsv1alpha1.StartupModeDeferred, map[string]string(nil), []string{"", "argument"}),
		Entry("NUL command arguments are rejected", agentsv1alpha1.StartupModeDeferred, map[string]string(nil), []string{"printf", "before\x00after"}),
		Entry("quote-expanded command scripts exceeding the shared budget are rejected", agentsv1alpha1.StartupModeDeferred, map[string]string(nil), []string{"printf", strings.Repeat("'", 8192)}),
	)

	DescribeTable("rejects invalid Deferred pool configuration at admission",
		func(specCtx SpecContext, configure func(*agentsv1alpha1.SandboxSet), message string) {
			ctx = specCtx
			// Reuse the concrete pool template without creating stock before the invalid create.
			pool, _ := newPool(agentsv1alpha1.StartupModeDeferred, appCommand("template"), nil, false, func(p *agentsv1alpha1.SandboxSet) { p.Spec.Replicas = 0 })
			invalid := &agentsv1alpha1.SandboxSet{ObjectMeta: metav1.ObjectMeta{Name: "invalid-pool", Namespace: namespace}, Spec: *pool.Spec.DeepCopy()}
			configure(invalid)
			err := k8sClient.Create(ctx, invalid)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(message))
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(invalid), &agentsv1alpha1.SandboxSet{}))).To(BeTrue())
			list := &corev1.PodList{}
			Expect(k8sClient.List(ctx, list, client.InNamespace(namespace))).To(Succeed())
			Expect(list.Items).To(BeEmpty())
		},
		Entry("the startup emptyDir mount must be writable", func(p *agentsv1alpha1.SandboxSet) {
			p.Spec.Template.Spec.Containers[0].VolumeMounts[0].ReadOnly = true
		}, "envd startup mount must be writable"),
		Entry("nested mounts must not shadow the claim directory", func(p *agentsv1alpha1.SandboxSet) {
			p.Spec.Template.Spec.Containers[0].VolumeMounts = append(p.Spec.Template.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "nested", MountPath: "/mnt/envd/claim"})
			p.Spec.Template.Spec.Volumes = append(p.Spec.Template.Spec.Volumes, corev1.Volume{Name: "nested", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
		}, "nested mount shadows"),
		Entry("normalized mount aliases cannot shadow startup files", func(p *agentsv1alpha1.SandboxSet) {
			p.Spec.Template.Spec.Containers[0].VolumeMounts = append(p.Spec.Template.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "alias", MountPath: "/mnt//envd/claim", ReadOnly: true})
			p.Spec.Template.Spec.Volumes = append(p.Spec.Template.Spec.Volumes, corev1.Volume{Name: "alias", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
		}, "nested mount shadows"),
		Entry("Deferred startup requires an agent-runtime", func(p *agentsv1alpha1.SandboxSet) {
			p.Spec.Runtimes = nil
		}, "Deferred startup requires agent-runtime"),
	)

	It("refuses Pod generation for an admitted non-emptyDir startup volume", func(specCtx SpecContext) {
		ctx = specCtx
		// The current pool admission validates the mount, not its backing volume type.
		configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "startup-volume", Namespace: namespace}}
		Expect(k8sClient.Create(ctx, configMap)).To(Succeed())
		pool, _ := newPool(agentsv1alpha1.StartupModeDeferred, appCommand("template"), nil, false, func(p *agentsv1alpha1.SandboxSet) {
			p.Spec.Replicas = 0
			p.Spec.Template.Spec.Volumes[0].VolumeSource = corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: configMap.Name}}}
		})
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), pool)).To(Succeed())
			pool.Spec.Replicas = 1
			g.Expect(k8sClient.Update(ctx, pool)).To(Succeed())
		}, waitTimeout, pollInterval).Should(Succeed())
		var failed *agentsv1alpha1.Sandbox
		Eventually(func(g Gomega) {
			list := &agentsv1alpha1.SandboxList{}
			g.Expect(k8sClient.List(ctx, list, client.InNamespace(namespace), client.MatchingLabels{agentsv1alpha1.LabelSandboxPool: pool.Name})).To(Succeed())
			g.Expect(list.Items).NotTo(BeEmpty())
			failed = list.Items[0].DeepCopy()
			events := &corev1.EventList{}
			g.Expect(k8sClient.List(ctx, events, client.InNamespace(namespace))).To(Succeed())
			g.Expect(events.Items).To(ContainElement(And(
				WithTransform(func(e corev1.Event) types.UID { return e.InvolvedObject.UID }, Equal(failed.UID)),
				WithTransform(func(e corev1.Event) string { return e.Reason }, Equal(agentsv1alpha1.SandboxReadyReasonPodCreateFailed)),
				WithTransform(func(e corev1.Event) string { return e.Message }, ContainSubstring("per-pod emptyDir")),
			)))
			g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(failed), &corev1.Pod{}))).To(BeTrue())
		}, waitTimeout, pollInterval).Should(Succeed())
		Consistently(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(failed), &corev1.Pod{}))
		}, 3*time.Second, pollInterval).Should(BeTrue())
	})

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
