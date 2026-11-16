/*
Copyright 2026 The Kruise Authors.

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
	"errors"
	"fmt"
	"math/rand"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/templatecreation"
)

// CR-level controller coverage; the custom HTTP listener is not deployed here.
var _ = Describe("SandboxTemplate companion lifecycle", func() {
	var (
		ctx       = context.Background()
		namespace string
	)

	BeforeEach(func() {
		namespace = createNamespace(ctx)
	})

	newVersionID := func() string {
		return fmt.Sprintf("%010x%010x", rand.Uint32(), rand.Uint32())
	}

	createPair := func(warmPool *agentsv1alpha1.SandboxTemplateWarmPoolOptions) (*agentsv1alpha1.SandboxTemplateClaim, *agentsv1alpha1.SandboxTemplate) {
		if warmPool == nil {
			// The manager persists a defaulted warmPool on the HTTP path; a raw
			// CR without BuildOptions declares no pool at all, so mirror the
			// persisted default (enabled, size zero) here.
			warmPool = &agentsv1alpha1.SandboxTemplateWarmPoolOptions{}
		}
		id := newVersionID()
		claim := &agentsv1alpha1.SandboxTemplateClaim{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:  namespace,
				Name:       fmt.Sprintf("tpl-e2e-%d", time.Now().UnixNano()),
				Finalizers: []string{agentsv1alpha1.SandboxTemplateClaimCleanupFinalizer},
				Annotations: map[string]string{
					templatecreation.AllocationAnnotation:       id,
					templatecreation.CreationCompleteAnnotation: "true",
				},
			},
		}
		Expect(k8sClient.Create(ctx, claim)).To(Succeed())

		spec := agentsv1alpha1.SandboxTemplateSpec{
			Template: &corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"e2e": "sandboxtemplate"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "nginx:stable-alpine3.23"}},
				},
			},
		}
		spec.BuildOptions = &agentsv1alpha1.SandboxTemplateBuildOptions{WarmPool: warmPool}
		version := &agentsv1alpha1.SandboxTemplate{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace,
				Name:      id,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion:         agentsv1alpha1.GroupVersion.String(),
					Kind:               "SandboxTemplateClaim",
					Name:               claim.Name,
					UID:                claim.UID,
					Controller:         ptr.To(true),
					BlockOwnerDeletion: ptr.To(true),
				}},
			},
			Spec: spec,
		}
		Expect(k8sClient.Create(ctx, version)).To(Succeed())
		return claim, version
	}

	autoscalerWarmPool := func() *agentsv1alpha1.SandboxTemplateWarmPoolOptions {
		return &agentsv1alpha1.SandboxTemplateWarmPoolOptions{
			Size: ptr.To(int32(0)),
			Autoscaler: &agentsv1alpha1.SandboxTemplatePoolAutoscalerOptions{
				MinReplicas: 0,
				MaxReplicas: 3,
				CapacityPolicy: &agentsv1alpha1.CapacityPolicy{
					TargetAvailable: intstr.FromInt32(0),
					Tolerance:       ptr.To(intstr.FromString("10%")),
					ScaleUp:         &agentsv1alpha1.CapacityScalingRules{StabilizationWindowSeconds: ptr.To(int32(60))},
					ScaleDown:       &agentsv1alpha1.CapacityScalingRules{StabilizationWindowSeconds: ptr.To(int32(300))},
				},
				CronPolicies: []agentsv1alpha1.CronScalingPolicy{{Name: "zero", Schedule: "0 0 * * *", TargetReplicas: 0}},
			},
		}
	}

	waitDefinitionReady := func(name string) {
		Eventually(func() bool {
			v := &agentsv1alpha1.SandboxTemplate{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, v); err != nil {
				return false
			}
			cond := meta.FindStatusCondition(v.Status.Conditions, templatecreation.DefinitionCondition)
			return v.Status.Phase == agentsv1alpha1.SandboxTemplatePhaseReady &&
				v.Status.ObservedGeneration == v.Generation &&
				cond != nil && cond.Status == metav1.ConditionTrue
		}, time.Minute, time.Second).Should(BeTrue())
	}

	waitCompanionPool := func(name string) *agentsv1alpha1.SandboxSet {
		pool := &agentsv1alpha1.SandboxSet{}
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, pool)
		}, time.Minute, time.Second).Should(Succeed())
		return pool
	}

	gone := func(obj client.Object, name string) bool {
		return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, obj))
	}

	It("creates the companion pool for a default warm pool", func() {
		claim, version := createPair(nil)
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, claim) })

		By("Reporting a ready definition-only status")
		waitDefinitionReady(version.Name)

		By("Creating a zero-size companion SandboxSet owned by the version")
		pool := waitCompanionPool(version.Name)
		Expect(pool.Spec.Replicas).To(Equal(int32(0)))
		Expect(pool.Spec.Template).To(BeNil())
		Expect(pool.Spec.TemplateRef).To(Equal(&agentsv1alpha1.SandboxTemplateRef{
			Name:       version.Name,
			Kind:       ptr.To("SandboxTemplate"),
			APIVersion: ptr.To(agentsv1alpha1.GroupVersion.String()),
		}))
		ref := metav1.GetControllerOf(pool)
		Expect(ref).NotTo(BeNil())
		Expect(ref.Kind).To(Equal("SandboxTemplate"))
		Expect(ref.Name).To(Equal(version.Name))
		Expect(ref.UID).To(Equal(version.UID))

		By("Declaring no companion autoscaler for fixed capacity")
		Consistently(func() int {
			list := &agentsv1alpha1.PoolAutoscalerList{}
			Expect(k8sClient.List(ctx, list, client.InNamespace(namespace))).To(Succeed())
			return len(list.Items)
		}, 10*time.Second, time.Second).Should(Equal(0))
	})

	It("declines companions for a disabled warm pool", func() {
		claim, version := createPair(&agentsv1alpha1.SandboxTemplateWarmPoolOptions{Enabled: ptr.To(false)})
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, claim) })

		waitDefinitionReady(version.Name)

		sets := &agentsv1alpha1.SandboxSetList{}
		Consistently(func() int {
			Expect(k8sClient.List(ctx, sets, client.InNamespace(namespace))).To(Succeed())
			return len(sets.Items)
		}, 10*time.Second, time.Second).Should(Equal(0))
		list := &agentsv1alpha1.PoolAutoscalerList{}
		Consistently(func() int {
			Expect(k8sClient.List(ctx, list, client.InNamespace(namespace))).To(Succeed())
			return len(list.Items)
		}, 10*time.Second, time.Second).Should(Equal(0))
	})

	It("creates a sibling companion autoscaler bound to the pool", func() {
		claim, version := createPair(autoscalerWarmPool())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, claim) })

		waitDefinitionReady(version.Name)
		pool := waitCompanionPool(version.Name)

		By("Creating a PoolAutoscaler owned by the version, not the pool")
		pa := &agentsv1alpha1.PoolAutoscaler{}
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: version.Name}, pa)
		}, time.Minute, time.Second).Should(Succeed())
		ref := metav1.GetControllerOf(pa)
		Expect(ref).NotTo(BeNil())
		Expect(ref.Kind).To(Equal("SandboxTemplate"))
		Expect(ref.Name).To(Equal(version.Name))
		Expect(ref.UID).To(Equal(version.UID))
		for _, owner := range pa.OwnerReferences {
			Expect(owner.UID).NotTo(Equal(pool.UID))
		}

		By("Carrying the declared policy and targeting the companion pool")
		Expect(pa.Spec.ScaleTargetRef.Kind).To(Equal("SandboxSet"))
		Expect(pa.Spec.ScaleTargetRef.APIVersion).To(Equal(agentsv1alpha1.GroupVersion.String()))
		Expect(pa.Spec.ScaleTargetRef.Name).To(Equal(version.Name))
		Expect(pa.Spec.MaxReplicas).To(Equal(int32(3)))
		Expect(pa.Spec.CapacityPolicy).NotTo(BeNil())
		Expect(pa.Spec.CapacityPolicy.TargetAvailable).To(Equal(intstr.FromInt32(0)))
		Expect(pa.Spec.CronPolicies).To(HaveLen(1))

		By("Initialising the pool at the declared size, not at capacity")
		Expect(pool.Spec.Replicas).To(Equal(int32(0)))
	})

	It("rejects updates to a managed definition", func() {
		claim, version := createPair(nil)
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, claim) })

		waitDefinitionReady(version.Name)
		waitCompanionPool(version.Name)

		By("Patching the version image through admission")
		Eventually(func() int32 {
			v := &agentsv1alpha1.SandboxTemplate{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: version.Name}, v); err != nil {
				return 0
			}
			v.Spec.Template.Spec.Containers[0].Image = "nginx:stable-alpine3.20"
			updateErr := k8sClient.Update(ctx, v)
			if updateErr == nil {
				return 0
			}
			var statusErr *apierrors.StatusError
			if errors.As(updateErr, &statusErr) {
				return statusErr.Status().Code
			}
			return 0
		}, 30*time.Second, time.Second).Should(BeElementOf(int32(400), int32(422)))
	})

	It("drives durable companion cleanup when the claim is deleted", func() {
		claim, version := createPair(autoscalerWarmPool())
		waitDefinitionReady(version.Name)
		waitCompanionPool(version.Name)
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: version.Name}, &agentsv1alpha1.PoolAutoscaler{})
		}, time.Minute, time.Second).Should(Succeed())

		By("Waiting for Claim removal after foreground companion cleanup")
		Expect(k8sClient.Delete(ctx, claim)).To(Succeed())
		Eventually(func() bool {
			return gone(&agentsv1alpha1.SandboxTemplateClaim{}, claim.Name)
		}, 2*time.Minute, 2*time.Second).Should(BeTrue())
		Expect(gone(&agentsv1alpha1.PoolAutoscaler{}, version.Name)).To(BeTrue())
		Expect(gone(&agentsv1alpha1.SandboxSet{}, version.Name)).To(BeTrue())
		Expect(gone(&agentsv1alpha1.SandboxTemplate{}, version.Name)).To(BeTrue())
	})

	It("leaves an external referencing pool untouched during cleanup", func() {
		claim, version := createPair(nil)
		waitDefinitionReady(version.Name)
		waitCompanionPool(version.Name)

		external := &agentsv1alpha1.SandboxSet{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace,
				Name:      fmt.Sprintf("tpl-e2e-ext-%d", time.Now().UnixNano()),
			},
			Spec: agentsv1alpha1.SandboxSetSpec{
				Replicas: 0,
				EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
					TemplateRef: &agentsv1alpha1.SandboxTemplateRef{Name: version.Name},
				},
			},
		}
		Expect(k8sClient.Create(ctx, external)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, external) })

		By("Deleting the claim completes owned cleanup")
		Expect(k8sClient.Delete(ctx, claim)).To(Succeed())
		Eventually(func() bool {
			return gone(&agentsv1alpha1.SandboxTemplate{}, version.Name)
		}, 2*time.Minute, 2*time.Second).Should(BeTrue())
		Eventually(func() bool {
			return gone(&agentsv1alpha1.SandboxTemplateClaim{}, claim.Name)
		}, 2*time.Minute, 2*time.Second).Should(BeTrue())

		By("The external pool survives unadopted and undamaged")
		survivor := &agentsv1alpha1.SandboxSet{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: external.Name}, survivor)).To(Succeed())
		Expect(survivor.UID).To(Equal(external.UID))
		Expect(survivor.DeletionTimestamp).To(BeNil())
		Expect(survivor.OwnerReferences).To(BeEmpty())
	})
})
