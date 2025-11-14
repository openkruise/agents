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
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	utils2 "github.com/openkruise/agents/test/e2e/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("Bypass Sandbox Webhook", Ordered, func() {

	BeforeAll(func() {
		By("cleaning up legacy pods")
		ctx := context.Background()
		Expect(k8sClient.DeleteAllOf(ctx, &corev1.Pod{},
			client.InNamespace(Namespace),
			client.MatchingLabels(utils2.GetE2EMarker()),
		)).To(Succeed())
		Expect(k8sClient.DeleteAllOf(ctx, &agentsv1alpha1.Sandbox{},
			client.InNamespace(Namespace),
			client.MatchingLabels(utils2.GetE2EMarker()),
		)).To(Succeed())
		sandboxList := &agentsv1alpha1.SandboxList{}
		Expect(k8sClient.List(ctx, sandboxList)).To(Succeed())
		for _, sandbox := range sandboxList.Items {
			if sandbox.DeletionTimestamp.IsZero() {
				continue
			}
			fmt.Printf("removing finalizer of sandbox %s\n", sandbox.Name)
			patch := client.MergeFrom(sandbox.DeepCopy())
			sandbox.Finalizers = nil
			Expect(k8sClient.Patch(ctx, &sandbox, patch)).To(Succeed())
		}
	})

	It("enabled", func() {
		ctx := context.Background()
		By("create pod")
		pod, cleanup := CreatePod(ctx, "bypass-webhook", "enabled", func(template *corev1.PodTemplateSpec) {
			template.Labels[utils.PodLabelEnableAutoCreateSandbox] = utils.True
			template.Labels["alibabacloud.com/acs"] = utils.True
			template.Annotations = map[string]string{
				utils.PodAnnotationEnablePaused: utils.True,
			}
			template.Spec.Containers[0].Env = append(template.Spec.Containers[0].Env, corev1.EnvVar{
				Name:  "__ECI_VM_IMAGE_ID__",
				Value: "m-bp1826v9yohzj4x12s74",
			})
		})
		fmt.Printf("pod %s/%s created\n", pod.Namespace, pod.Name)
		defer cleanup()

		By("wait pod ready")
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      pod.Name,
				Namespace: pod.Namespace,
			}, pod)).To(Succeed())
			g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning))
			cond := utils.GetPodCondition(&pod.Status, corev1.PodReady)
			g.Expect(cond.Status).To(Equal(corev1.ConditionTrue))
		}, time.Minute, time.Second).Should(Succeed())

		By("pause pod first time")
		Expect(k8sClient.Patch(ctx, pod, client.RawPatch(types.MergePatchType, []byte(
			fmt.Sprintf(`{"metadata":{"annotations":{"%s":"%s"}}}`, utils.PodAnnotationSandboxPause, utils.True))))).
			To(Succeed())
		start := time.Now()

		By("check sandbox creation")
		sandbox := &agentsv1alpha1.Sandbox{}
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      pod.Name,
				Namespace: pod.Namespace,
			}, sandbox)).To(Succeed())
			g.Expect(sandbox.Spec.Paused).To(BeTrue())
			g.Expect(sandbox.Spec.Template.Spec.Containers[0].Image).To(Equal(pod.Spec.Containers[0].Image))
			g.Expect(sandbox.Status.PodInfo.PodIP).To(Equal(pod.Status.PodIP))
			g.Expect(sandbox.Status.PodInfo.NodeName).To(Equal(pod.Spec.NodeName))
			g.Expect(sandbox.Status.PodInfo.Annotations[utils.PodAnnotationSourcePodUID]).To(BeEquivalentTo(pod.UID))
		}, time.Minute, time.Second).Should(Succeed())
		sandboxUID := sandbox.UID

		// patch for clean up
		patch := client.MergeFrom(sandbox)
		sandbox.Labels = utils2.GetE2EMarker()
		Expect(k8sClient.Patch(ctx, sandbox, patch)).To(Succeed())
		defer func() {
			Expect(k8sClient.Delete(ctx, sandbox)).To(Succeed())
		}()

		CheckPodPauseAndResume(ctx, pod, start)

		By("wait 10s")
		time.Sleep(10 * time.Second)

		By("pause pod second time")
		Expect(k8sClient.Patch(ctx, pod, client.RawPatch(types.MergePatchType, []byte(
			fmt.Sprintf(`{"metadata":{"annotations":{"%s":"%s"}}}`, utils.PodAnnotationSandboxPause, utils.True))))).
			To(Succeed())
		start = time.Now()
		By("check sandbox patched")
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      pod.Name,
				Namespace: pod.Namespace,
			}, sandbox)).To(Succeed())
			g.Expect(sandbox.UID).To(Equal(sandboxUID))
			g.Expect(sandbox.Spec.Paused).To(BeTrue())
			g.Expect(sandbox.Status.PodInfo.Annotations[utils.PodAnnotationSourcePodUID]).To(BeEquivalentTo(pod.UID))
		}, time.Minute, time.Second).Should(Succeed())

		CheckPodPauseAndResume(ctx, pod, start)
	})
})

func CheckPodPauseAndResume(ctx context.Context, pod *corev1.Pod, start time.Time) {
	By("wait pod paused")
	Eventually(func(g Gomega) {
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		}, pod)).To(Succeed())
		cond := utils.GetPodCondition(&pod.Status, utils.PodConditionContainersPaused)
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.Status).To(Equal(corev1.ConditionTrue))
	}, 5*time.Minute, time.Second).Should(Succeed())
	fmt.Printf("pause cost: %v\n", time.Since(start))

	By("delete pod")
	Expect(k8sClient.Delete(ctx, pod)).To(Succeed())

	By("wait 10s")
	time.Sleep(10 * time.Second)

	By("recreate pod to resume")
	extraAnnotation := "extra-annotation"
	extraLabel := "extra-label"
	oldImage := pod.Spec.Containers[0].Image
	newPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        pod.Name,
			Namespace:   pod.Namespace,
			Annotations: pod.Annotations,
			Labels:      pod.Labels,
		},
		Spec: pod.Spec,
	}
	newPod.Annotations[extraAnnotation] = extraAnnotation
	newPod.Labels[extraLabel] = extraLabel
	newPod.Spec.Containers[0].Image = "not-exist" // will be overwritten
	Expect(k8sClient.Create(ctx, newPod)).To(Succeed())
	runningOnce := sync.Once{}
	start = time.Now()
	Eventually(func(g Gomega) {
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		}, pod)).To(Succeed())
		g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning))
		runningOnce.Do(func() {
			fmt.Printf("running cost: %v\n", time.Since(start))
		})
		cond := utils.GetPodCondition(&pod.Status, utils.PodConditionContainersResumed)
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.Status).To(Equal(corev1.ConditionTrue))
		g.Expect(pod.Annotations[extraAnnotation]).To(Equal(extraAnnotation))
		g.Expect(pod.Labels[extraLabel]).To(Equal(extraLabel))
		g.Expect(pod.Spec.Containers[0].Image).To(Equal(oldImage))
		g.Expect(pod.Annotations[utils.PodAnnotationSandboxPause]).NotTo(Equal(utils.True))
	}, 5*time.Minute, time.Second).Should(Succeed())
	fmt.Printf("resume cost: %v\n", time.Since(start))
}
