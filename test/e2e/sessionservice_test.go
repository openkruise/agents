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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agents "github.com/openkruise/agents/api/v1alpha1"
	sessionbinding "github.com/openkruise/agents/pkg/sessionservice/binding"
)

var _ = Describe("SessionService", func() {
	ctx := context.Background()
	var service *agents.SessionService

	BeforeEach(func() {
		namespace := createNamespace(ctx)
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))).To(Succeed())
		})
		service = &agents.SessionService{
			ObjectMeta: metav1.ObjectMeta{Name: "session-service", Namespace: namespace},
			Spec: agents.SessionServiceSpec{
				Replicas:       ptr.To[int32](1),
				Ports:          []int32{80},
				UpdateStrategy: agents.SessionServiceUpdateStrategy{MaxSurge: ptr.To(intstr.FromInt(1))},
				SandboxTemplateSpec: agents.SandboxTemplateSpec{
					Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
						AutomountServiceAccountToken: ptr.To(false),
						Containers:                   []corev1.Container{{Name: "main", Image: "nginx:stable-alpine3.23", Env: []corev1.EnvVar{{Name: "TEMPLATE_VERSION", Value: "v1"}}}},
					}},
				},
			},
		}
	})

	// Check both the observed status and actual members so stale status alone
	// cannot make a scaling or rollout assertion pass.
	waitForMembers := func(replicas int32, version string) []agents.Sandbox {
		var members agents.SandboxList
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(service), service)).To(Succeed())
			g.Expect(service.Status.ObservedGeneration).To(Equal(service.Generation))
			g.Expect(service.Status.Replicas).To(Equal(replicas))
			g.Expect(service.Status.ReadyReplicas).To(Equal(replicas))
			g.Expect(service.Status.CurrentRevision).NotTo(BeEmpty())
			g.Expect(service.Status.UpdateRevision).To(Equal(service.Status.CurrentRevision))
			g.Expect(k8sClient.List(ctx, &members, client.InNamespace(service.Namespace), client.MatchingLabels{
				sessionbinding.SessionServiceUIDLabel: string(service.UID),
			})).To(Succeed())
			g.Expect(members.Items).To(HaveLen(int(replicas)))
			for i := range members.Items {
				member := &members.Items[i]
				owner := metav1.GetControllerOf(member)
				g.Expect(owner).NotTo(BeNil())
				g.Expect(owner.UID).To(Equal(service.UID))
				g.Expect(member.DeletionTimestamp).To(BeNil())
				g.Expect(member.Labels[sessionbinding.SessionServiceRevisionLabel]).To(Equal(service.Status.CurrentRevision))
				g.Expect(member.Status.Phase).To(Equal(agents.SandboxRunning))
				g.Expect(meta.IsStatusConditionTrue(member.Status.Conditions, string(agents.SandboxConditionReady))).To(BeTrue())
				g.Expect(member.Spec.Template.Spec.Containers[0].Env).To(ContainElement(corev1.EnvVar{Name: "TEMPLATE_VERSION", Value: version}))
			}
		}, 5*time.Minute, time.Second).Should(Succeed())
		return members.Items
	}

	It("scales members up and down", func() {
		Expect(k8sClient.Create(ctx, service)).To(Succeed())
		waitForMembers(1, "v1")
		initialRevision := service.Status.CurrentRevision

		for _, replicas := range []int32{3, 1} {
			By("Updating the desired replica count")
			Eventually(func() error {
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(service), service); err != nil {
					return err
				}
				service.Spec.Replicas = ptr.To(replicas)
				return k8sClient.Update(ctx, service)
			}, time.Minute, time.Second).Should(Succeed())
			waitForMembers(replicas, "v1")
			Expect(service.Status.CurrentRevision).To(Equal(initialRevision))
		}
	})

	It("replaces members when the template version changes", func() {
		service.Spec.Replicas = ptr.To[int32](2)
		Expect(k8sClient.Create(ctx, service)).To(Succeed())
		originalMembers := waitForMembers(2, "v1")
		originalRevision := service.Status.CurrentRevision

		By("Updating the container environment to create a new template version")
		Eventually(func() error {
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(service), service); err != nil {
				return err
			}
			service.Spec.Template.Spec.Containers[0].Env[0].Value = "v2"
			return k8sClient.Update(ctx, service)
		}, time.Minute, time.Second).Should(Succeed())
		updatedMembers := waitForMembers(2, "v2")
		Expect(service.Status.CurrentRevision).NotTo(Equal(originalRevision))
		for _, updated := range updatedMembers {
			for _, original := range originalMembers {
				Expect(updated.UID).NotTo(Equal(original.UID))
			}
		}
		var revision appsv1.ControllerRevision
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: service.Namespace, Name: service.Status.CurrentRevision}, &revision)).To(Succeed())
		owner := metav1.GetControllerOf(&revision)
		Expect(owner).NotTo(BeNil())
		Expect(owner.UID).To(Equal(service.UID))
	})

	It("preserves old members during an image pull failure and recovers after correction", func() {
		service.Spec.Replicas = ptr.To[int32](2)
		Expect(k8sClient.Create(ctx, service)).To(Succeed())
		originalMembers := waitForMembers(2, "v1")
		originalRevision := service.Status.CurrentRevision
		validImage := service.Spec.Template.Spec.Containers[0].Image
		missingImage := "nginx:sessionservice-missing-" + string(service.UID)

		By("Publishing a new version with a nonexistent image tag")
		Eventually(func() error {
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(service), service); err != nil {
				return err
			}
			service.Spec.Template.Spec.Containers[0].Image = missingImage
			service.Spec.Template.Spec.Containers[0].Env[0].Value = "v2"
			return k8sClient.Update(ctx, service)
		}, time.Minute, time.Second).Should(Succeed())

		assertOldMembers := func(g Gomega) {
			for _, original := range originalMembers {
				var member agents.Sandbox
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(&original), &member)).To(Succeed())
				g.Expect(member.UID).To(Equal(original.UID))
				g.Expect(member.DeletionTimestamp).To(BeNil())
				g.Expect(member.Status.Phase).To(Equal(agents.SandboxRunning))
				g.Expect(meta.IsStatusConditionTrue(member.Status.Conditions, string(agents.SandboxConditionReady))).To(BeTrue())
			}
		}

		By("Observing an actual image pull failure on the replacement Pod")
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(service), service)).To(Succeed())
			g.Expect(service.Status.ObservedGeneration).To(Equal(service.Generation))
			g.Expect(service.Status.UpdateRevision).NotTo(BeEmpty())
			g.Expect(service.Status.UpdateRevision).NotTo(Equal(originalRevision))
			assertOldMembers(g)
			var members agents.SandboxList
			g.Expect(k8sClient.List(ctx, &members, client.InNamespace(service.Namespace), client.MatchingLabels{
				sessionbinding.SessionServiceUIDLabel: string(service.UID),
			})).To(Succeed())
			g.Expect(members.Items).To(HaveLen(3))
			var pods corev1.PodList
			g.Expect(k8sClient.List(ctx, &pods, client.InNamespace(service.Namespace))).To(Succeed())
			pullFailed := false
			for i := range members.Items {
				member := &members.Items[i]
				if member.Labels[sessionbinding.SessionServiceRevisionLabel] != service.Status.UpdateRevision {
					continue
				}
				g.Expect(member.Spec.Template.Spec.Containers[0].Image).To(Equal(missingImage))
				for j := range pods.Items {
					pod := &pods.Items[j]
					if !metav1.IsControlledBy(pod, member) {
						continue
					}
					for _, status := range pod.Status.ContainerStatuses {
						if status.Name == "main" && status.State.Waiting != nil {
							reason := status.State.Waiting.Reason
							pullFailed = pullFailed || reason == "ErrImagePull" || reason == "ImagePullBackOff"
						}
					}
				}
			}
			g.Expect(pullFailed).To(BeTrue())
		}, 5*time.Minute, time.Second).Should(Succeed())
		failedRevision := service.Status.UpdateRevision

		By("Keeping both old Ready members while the rollout is blocked")
		Consistently(func(g Gomega) {
			assertOldMembers(g)
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(service), service)).To(Succeed())
			g.Expect(service.Status.CurrentRevision).To(Equal(originalRevision))
			g.Expect(service.Status.UpdateRevision).To(Equal(failedRevision))
			g.Expect(service.Status.ReadyReplicas).To(Equal(int32(2)))
		}, 15*time.Second, time.Second).Should(Succeed())

		By("Correcting only the image while retaining the new version environment")
		Eventually(func() error {
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(service), service); err != nil {
				return err
			}
			service.Spec.Template.Spec.Containers[0].Image = validImage
			return k8sClient.Update(ctx, service)
		}, time.Minute, time.Second).Should(Succeed())
		updatedMembers := waitForMembers(2, "v2")
		Expect(service.Status.CurrentRevision).NotTo(Equal(originalRevision))
		Expect(service.Status.CurrentRevision).NotTo(Equal(failedRevision))
		for _, updated := range updatedMembers {
			Expect(updated.Spec.Template.Spec.Containers[0].Image).To(Equal(validImage))
			for _, original := range originalMembers {
				Expect(updated.UID).NotTo(Equal(original.UID))
			}
		}
	})

})
