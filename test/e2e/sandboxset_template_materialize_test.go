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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// These tests exercise the "auto-materialise SandboxTemplate" feature added
// to the SandboxSet controller. Every Case relies on a fresh SandboxSet plus
// a dedicated namespace so concurrent runs do not interfere with each other.
var _ = Describe("SandboxSet materialises SandboxTemplate automatically", func() {
	var (
		ctx       = context.Background()
		namespace string
	)

	BeforeEach(func() {
		namespace = createNamespace(ctx)
	})

	buildInlineSandboxSet := func(name, image string) *agentsv1alpha1.SandboxSet {
		return &agentsv1alpha1.SandboxSet{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: agentsv1alpha1.SandboxSetSpec{
				Replicas: 1,
				EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"sandboxset": name},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "main", Image: image}},
						},
					},
				},
			},
		}
	}

	listOwnedSBT := func(sbs *agentsv1alpha1.SandboxSet) []agentsv1alpha1.SandboxTemplate {
		sbtList := &agentsv1alpha1.SandboxTemplateList{}
		Expect(k8sClient.List(ctx, sbtList, client.InNamespace(namespace))).To(Succeed())
		var owned []agentsv1alpha1.SandboxTemplate
		for i := range sbtList.Items {
			ref := metav1.GetControllerOf(&sbtList.Items[i])
			if ref != nil && ref.UID == sbs.UID {
				owned = append(owned, sbtList.Items[i])
			}
		}
		return owned
	}

	It("Case A - inline template is materialised with owner ref", func() {
		sbs := buildInlineSandboxSet(fmt.Sprintf("sbs-mat-a-%d", time.Now().UnixNano()), "nginx:stable-alpine3.23")
		Expect(k8sClient.Create(ctx, sbs)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, sbs) })

		By("Waiting for the SandboxSet status.currentTemplate to be populated")
		Eventually(func() string {
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbs.Name, Namespace: sbs.Namespace}, sbs)).To(Succeed())
			return sbs.Status.CurrentTemplate
		}, time.Minute, time.Second).ShouldNot(BeEmpty())
		Expect(sbs.Status.CurrentTemplate).To(HavePrefix(sbs.Name + "-"))

		By("Verifying the materialised SandboxTemplate exists with the right owner ref")
		sbt := &agentsv1alpha1.SandboxTemplate{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbs.Status.CurrentTemplate, Namespace: namespace}, sbt)).To(Succeed())
		ref := metav1.GetControllerOf(sbt)
		Expect(ref).NotTo(BeNil())
		Expect(ref.UID).To(Equal(sbs.UID))
		Expect(ref.BlockOwnerDeletion).NotTo(BeNil())
		Expect(*ref.BlockOwnerDeletion).To(BeFalse())
	})

	It("Case B - templateRef does not trigger materialisation", func() {
		userSBT := &agentsv1alpha1.SandboxTemplate{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("user-sbt-%d", time.Now().UnixNano()),
				Namespace: namespace,
			},
			Spec: agentsv1alpha1.SandboxTemplateSpec{
				Template: &corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"sandboxset": "ref"}},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "main", Image: "nginx:stable-alpine3.23"}},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, userSBT)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, userSBT) })

		sbs := &agentsv1alpha1.SandboxSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("sbs-mat-b-%d", time.Now().UnixNano()),
				Namespace: namespace,
			},
			Spec: agentsv1alpha1.SandboxSetSpec{
				Replicas: 1,
				EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
					TemplateRef: &agentsv1alpha1.SandboxTemplateRef{Name: userSBT.Name},
				},
			},
		}
		Expect(k8sClient.Create(ctx, sbs)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, sbs) })

		Eventually(func() string {
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbs.Name, Namespace: sbs.Namespace}, sbs)).To(Succeed())
			return sbs.Status.CurrentTemplate
		}, time.Minute, time.Second).Should(Equal(userSBT.Name))

		Consistently(func() int {
			sbtList := &agentsv1alpha1.SandboxTemplateList{}
			Expect(k8sClient.List(ctx, sbtList, client.InNamespace(namespace))).To(Succeed())
			auto := 0
			for i := range sbtList.Items {
				if strings.HasPrefix(sbtList.Items[i].Name, sbs.Name+"-") {
					auto++
				}
			}
			return auto
		}, 10*time.Second, time.Second).Should(Equal(0))
	})

	It("Case C - updating the inline template creates a new SandboxTemplate", func() {
		sbs := buildInlineSandboxSet(fmt.Sprintf("sbs-mat-c-%d", time.Now().UnixNano()), "nginx:stable-alpine3.23")
		Expect(k8sClient.Create(ctx, sbs)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, sbs) })

		var firstName string
		Eventually(func() string {
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbs.Name, Namespace: sbs.Namespace}, sbs)).To(Succeed())
			firstName = sbs.Status.CurrentTemplate
			return firstName
		}, time.Minute, time.Second).ShouldNot(BeEmpty())

		By("Changing the inline image triggers a new materialised SBT")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbs.Name, Namespace: sbs.Namespace}, sbs)).To(Succeed())
		sbs.Spec.Template.Spec.Containers[0].Image = "nginx:stable-alpine3.20"
		Expect(k8sClient.Update(ctx, sbs)).To(Succeed())

		Eventually(func() string {
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbs.Name, Namespace: sbs.Namespace}, sbs)).To(Succeed())
			return sbs.Status.CurrentTemplate
		}, time.Minute, time.Second).ShouldNot(Equal(firstName))

		owned := listOwnedSBT(sbs)
		Expect(len(owned)).To(BeNumerically(">=", 2))
	})

	It("Case D - history limit deletes the oldest orphan SBT", func() {
		sbs := buildInlineSandboxSet(fmt.Sprintf("sbs-mat-d-%d", time.Now().UnixNano()), "nginx:stable-alpine3.23")
		sbs.Spec.Replicas = 0 // keep no sandbox so every old SBT quickly becomes orphan
		Expect(k8sClient.Create(ctx, sbs)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, sbs) })

		// Drive 11 distinct template versions by cycling the image tag.
		images := []string{
			"nginx:stable-alpine3.23", "nginx:stable-alpine3.20",
			"nginx:1.27", "nginx:1.26", "nginx:1.25", "nginx:1.24",
			"nginx:1.23", "nginx:1.22", "nginx:1.21", "nginx:1.20",
			"nginx:1.19",
		}
		for _, img := range images {
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbs.Name, Namespace: sbs.Namespace}, sbs)).To(Succeed())
			sbs.Spec.Template.Spec.Containers[0].Image = img
			Expect(k8sClient.Update(ctx, sbs)).To(Succeed())
			Eventually(func() string {
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbs.Name, Namespace: sbs.Namespace}, sbs)).To(Succeed())
				return sbs.Status.CurrentTemplate
			}, time.Minute, time.Second).ShouldNot(BeEmpty())
		}

		Eventually(func() int {
			return len(listOwnedSBT(sbs))
		}, 2*time.Minute, 2*time.Second).Should(BeNumerically("<=", 10))
	})

	It("Case E - deleting the SandboxSet cascades to owned SBTs", func() {
		sbs := buildInlineSandboxSet(fmt.Sprintf("sbs-mat-e-%d", time.Now().UnixNano()), "nginx:stable-alpine3.23")
		Expect(k8sClient.Create(ctx, sbs)).To(Succeed())

		var sbtName string
		Eventually(func() string {
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbs.Name, Namespace: sbs.Namespace}, sbs)).To(Succeed())
			sbtName = sbs.Status.CurrentTemplate
			return sbtName
		}, time.Minute, time.Second).ShouldNot(BeEmpty())

		Expect(k8sClient.Delete(ctx, sbs)).To(Succeed())

		Eventually(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Name: sbtName, Namespace: namespace}, &agentsv1alpha1.SandboxTemplate{})
			return apierrors.IsNotFound(err)
		}, 2*time.Minute, 2*time.Second).Should(BeTrue())
	})
})
