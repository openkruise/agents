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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

const (
	annotationResetRequest    = "agents.kruise.io/reset-request"
	podConditionResetComplete = "ResetComplete"
	resetReasonSucceeded      = "ResetSucceeded"
	resetReasonFailed         = "ResetFailed"
)

type resetRequest struct {
	ResetID     string `json:"resetID"`
	RequestTime string `json:"requestTime"`
}

type resetResult struct {
	ResetID    string `json:"resetID"`
	StartTime  string `json:"startTime"`
	FinishTime string `json:"finishTime"`
	Error      string `json:"error,omitempty"`
}

var _ = Describe("Sandbox Reuse", func() {
	var (
		ctx        = context.Background()
		namespace  string
		sandboxSet *agentsv1alpha1.SandboxSet
	)

	BeforeEach(func() {
		namespace = createNamespace(ctx)
		sandboxSet = &agentsv1alpha1.SandboxSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("reuse-pool-%d", time.Now().UnixNano()),
				Namespace: namespace,
			},
			Spec: agentsv1alpha1.SandboxSetSpec{
				Replicas: 2,
				EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test-container",
									Image: "nginx:stable-alpine3.23",
								},
							},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, sandboxSet)).To(Succeed())

		By("Waiting for 2 sandboxes to be Running")
		Eventually(func() int32 {
			_ = k8sClient.Get(ctx, client.ObjectKeyFromObject(sandboxSet), sandboxSet)
			return sandboxSet.Status.AvailableReplicas
		}, time.Minute*2, time.Second).Should(Equal(int32(2)))
	})

	AfterEach(func() {
		if sandboxSet != nil {
			_ = k8sClient.Delete(ctx, sandboxSet)
		}
	})

	listPoolSandboxes := func() []agentsv1alpha1.Sandbox {
		list := &agentsv1alpha1.SandboxList{}
		Expect(k8sClient.List(ctx, list, client.InNamespace(namespace),
			client.MatchingLabels{agentsv1alpha1.LabelSandboxPool: sandboxSet.Name})).To(Succeed())
		return list.Items
	}

	// simulateClaim patches sandbox metadata to look like it was claimed.
	simulateClaim := func(sbx *agentsv1alpha1.Sandbox) {
		Eventually(func() error {
			latest := &agentsv1alpha1.Sandbox{}
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(sbx), latest); err != nil {
				return err
			}
			patch := client.MergeFrom(latest.DeepCopy())
			latest.Labels[agentsv1alpha1.LabelSandboxIsClaimed] = "true"
			latest.Labels[agentsv1alpha1.LabelSandboxClaimName] = "test-claim"
			latest.Labels["user-label"] = "user-value"
			if latest.Annotations == nil {
				latest.Annotations = map[string]string{}
			}
			latest.Annotations[agentsv1alpha1.AnnotationLock] = "lock-123"
			latest.Annotations[agentsv1alpha1.AnnotationOwner] = "owner-uid"
			latest.Annotations[agentsv1alpha1.AnnotationClaimTime] = time.Now().Format(time.RFC3339)
			latest.Annotations["user-anno"] = "user-value"
			meta, _ := json.Marshal(agentsv1alpha1.UpdatedMetadataInClaim{
				Labels:      []string{"user-label"},
				Annotations: []string{"user-anno"},
			})
			latest.Annotations[agentsv1alpha1.AnnotationUpdatedMetadataInClaim] = string(meta)
			return k8sClient.Patch(ctx, latest, patch)
		}, time.Second*10, time.Second).Should(Succeed())
	}

	// triggerReuse patches the sandbox to trigger reuse.
	triggerReuse := func(sbx *agentsv1alpha1.Sandbox) {
		Eventually(func() error {
			latest := &agentsv1alpha1.Sandbox{}
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(sbx), latest); err != nil {
				return err
			}
			patch := client.MergeFrom(latest.DeepCopy())
			latest.Annotations[agentsv1alpha1.AnnotationReuseEnabled] = "true"
			latest.Annotations[agentsv1alpha1.AnnotationReuse] = "true"
			return k8sClient.Patch(ctx, latest, patch)
		}, time.Second*10, time.Second).Should(Succeed())
	}

	// waitForResetRequest polls until the pod has a reset-request annotation, returns the parsed request.
	waitForResetRequest := func(podKey types.NamespacedName) resetRequest {
		var req resetRequest
		Eventually(func() error {
			pod := &corev1.Pod{}
			if err := k8sClient.Get(ctx, podKey, pod); err != nil {
				return err
			}
			raw := pod.Annotations[annotationResetRequest]
			if raw == "" {
				return fmt.Errorf("reset-request annotation not set yet")
			}
			return json.Unmarshal([]byte(raw), &req)
		}, time.Second*30, time.Second).Should(Succeed())
		return req
	}

	// simulateResetComplete patches the pod status with a ResetComplete condition.
	simulateResetComplete := func(podKey types.NamespacedName, resetID string, success bool, errMsg string) {
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			pod := &corev1.Pod{}
			if err := k8sClient.Get(ctx, podKey, pod); err != nil {
				return err
			}
			status := corev1.ConditionTrue
			reason := resetReasonSucceeded
			if !success {
				status = corev1.ConditionFalse
				reason = resetReasonFailed
			}
			result, _ := json.Marshal(resetResult{
				ResetID:    resetID,
				StartTime:  time.Now().Add(-time.Second).Format(time.RFC3339),
				FinishTime: time.Now().Format(time.RFC3339),
				Error:      errMsg,
			})

			cond := corev1.PodCondition{
				Type:               corev1.PodConditionType(podConditionResetComplete),
				Status:             status,
				Reason:             reason,
				Message:            string(result),
				LastTransitionTime: metav1.Now(),
			}
			found := false
			for i, c := range pod.Status.Conditions {
				if c.Type == corev1.PodConditionType(podConditionResetComplete) {
					pod.Status.Conditions[i] = cond
					found = true
					break
				}
			}
			if !found {
				pod.Status.Conditions = append(pod.Status.Conditions, cond)
			}
			return k8sClient.Status().Update(ctx, pod)
		})
		Expect(err).NotTo(HaveOccurred())
	}

	// waitForReuseCondition polls until the sandbox Reusing condition matches the given reason.
	waitForReuseCondition := func(sbx *agentsv1alpha1.Sandbox, reason string) {
		Eventually(func() string {
			_ = k8sClient.Get(ctx, client.ObjectKeyFromObject(sbx), sbx)
			for _, c := range sbx.Status.Conditions {
				if c.Type == string(agentsv1alpha1.SandboxConditionReusing) {
					return c.Reason
				}
			}
			return ""
		}, time.Second*30, time.Second).Should(Equal(reason))
	}

	// setAnnotation patches a single annotation on the sandbox.
	setAnnotation := func(sbx *agentsv1alpha1.Sandbox, key, value string) {
		Eventually(func() error {
			latest := &agentsv1alpha1.Sandbox{}
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(sbx), latest); err != nil {
				return err
			}
			patch := client.MergeFrom(latest.DeepCopy())
			if latest.Annotations == nil {
				latest.Annotations = map[string]string{}
			}
			latest.Annotations[key] = value
			return k8sClient.Patch(ctx, latest, patch)
		}, time.Second*10, time.Second).Should(Succeed())
	}

	// isSandboxDeleted returns true when the sandbox can no longer be found.
	isSandboxDeleted := func(sbx *agentsv1alpha1.Sandbox) bool {
		return k8sClient.Get(ctx, client.ObjectKeyFromObject(sbx), &agentsv1alpha1.Sandbox{}) != nil
	}

	// triggerReuseAndFail triggers reuse and simulates a reset failure.
	triggerReuseAndFail := func(target *agentsv1alpha1.Sandbox) {
		triggerReuse(target)

		By("Waiting for Reusing phase")
		Eventually(func() agentsv1alpha1.SandboxPhase {
			_ = k8sClient.Get(ctx, client.ObjectKeyFromObject(target), target)
			return target.Status.Phase
		}, time.Second*30, time.Second).Should(Equal(agentsv1alpha1.SandboxReusing))

		By("Simulating reset failure")
		podKey := types.NamespacedName{Name: target.Name, Namespace: target.Namespace}
		req := waitForResetRequest(podKey)
		simulateResetComplete(podKey, req.ResetID, false, "disk full")

		By("Waiting for reuse condition to show failure")
		waitForReuseCondition(target, agentsv1alpha1.SandboxReusingReasonFailed)
	}

	Context("Successful reuse", func() {
		It("should return sandbox to pool with metadata matching baseline sandbox", func() {
			sandboxes := listPoolSandboxes()
			Expect(sandboxes).To(HaveLen(2))

			baseline := &sandboxes[0]
			target := &sandboxes[1]

			By("Simulating a claim on the target sandbox")
			simulateClaim(target)

			By("Triggering reuse on the target sandbox")
			triggerReuse(target)

			By("Waiting for sandbox to enter Reusing phase")
			Eventually(func() agentsv1alpha1.SandboxPhase {
				_ = k8sClient.Get(ctx, client.ObjectKeyFromObject(target), target)
				return target.Status.Phase
			}, time.Second*30, time.Second).Should(Equal(agentsv1alpha1.SandboxReusing))

			By("Simulating agent-runtime reset completion")
			podKey := types.NamespacedName{Name: target.Name, Namespace: target.Namespace}
			req := waitForResetRequest(podKey)
			simulateResetComplete(podKey, req.ResetID, true, "")

			By("Waiting for sandbox to return to Running phase")
			Eventually(func() agentsv1alpha1.SandboxPhase {
				_ = k8sClient.Get(ctx, client.ObjectKeyFromObject(target), target)
				return target.Status.Phase
			}, time.Minute, time.Second).Should(Equal(agentsv1alpha1.SandboxRunning))

			By("Refreshing baseline sandbox")
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(baseline), baseline)).To(Succeed())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(target), target)).To(Succeed())

			By("Verifying ReuseCount is incremented")
			Expect(target.Status.ReuseCount).To(Equal(int32(1)))

			By("Verifying target labels match baseline (except ReuseCount in status)")
			Expect(target.Labels[agentsv1alpha1.LabelSandboxIsClaimed]).To(Equal(baseline.Labels[agentsv1alpha1.LabelSandboxIsClaimed]))
			Expect(target.Labels[agentsv1alpha1.LabelSandboxIsClaimed]).To(Equal("false"))
			Expect(target.Labels[agentsv1alpha1.LabelSandboxPool]).To(Equal(baseline.Labels[agentsv1alpha1.LabelSandboxPool]))
			Expect(target.Labels).NotTo(HaveKey(agentsv1alpha1.LabelSandboxClaimName))
			Expect(target.Labels).NotTo(HaveKey("user-label"))

			By("Verifying target has no claim annotations")
			Expect(target.Annotations).NotTo(HaveKey(agentsv1alpha1.AnnotationLock))
			Expect(target.Annotations).NotTo(HaveKey(agentsv1alpha1.AnnotationOwner))
			Expect(target.Annotations).NotTo(HaveKey(agentsv1alpha1.AnnotationClaimTime))
			Expect(target.Annotations).NotTo(HaveKey(agentsv1alpha1.AnnotationReuse))
			Expect(target.Annotations).NotTo(HaveKey(agentsv1alpha1.AnnotationReuseRetainOnFailure))
			Expect(target.Annotations).NotTo(HaveKey(agentsv1alpha1.AnnotationInitRuntimeRequest))
			Expect(target.Annotations).NotTo(HaveKey(agentsv1alpha1.AnnotationRuntimeAccessToken))
			Expect(target.Annotations).NotTo(HaveKey(agentsv1alpha1.AnnotationUpdatedMetadataInClaim))
			Expect(target.Annotations).NotTo(HaveKey("user-anno"))

			By("Verifying Spec fields are cleared")
			Expect(target.Spec.ShutdownTime).To(BeNil())
			Expect(target.Spec.PauseTime).To(BeNil())

			By("Verifying OwnerReferences point to SandboxSet (same as baseline)")
			Expect(target.OwnerReferences).To(HaveLen(1))
			Expect(target.OwnerReferences[0].Name).To(Equal(baseline.OwnerReferences[0].Name))
			Expect(target.OwnerReferences[0].Name).To(Equal(sandboxSet.Name))
		})
	})

	Context("Reuse failure", func() {
		It("should delete sandbox immediately when reset fails without retain annotation", func() {
			sandboxes := listPoolSandboxes()
			Expect(sandboxes).To(HaveLen(2))
			target := &sandboxes[0]

			By("Triggering reuse without any retain-on-failure annotation")
			triggerReuseAndFail(target)

			By("Verifying sandbox is deleted immediately")
			Eventually(func() bool {
				return isSandboxDeleted(target)
			}, time.Second*30, time.Second).Should(BeTrue())
		})

		It("should delete sandbox when retain-on-failure annotation has invalid value", func() {
			sandboxes := listPoolSandboxes()
			Expect(sandboxes).To(HaveLen(2))
			target := &sandboxes[0]

			By("Setting invalid retain-on-failure annotation")
			setAnnotation(target, agentsv1alpha1.AnnotationReuseRetainOnFailure, "not-a-duration")

			triggerReuseAndFail(target)

			By("Verifying sandbox is deleted immediately")
			Eventually(func() bool {
				return isSandboxDeleted(target)
			}, time.Second*30, time.Second).Should(BeTrue())
		})

		It("should set ShutdownTime when retain-on-failure annotation has valid duration", func() {
			sandboxes := listPoolSandboxes()
			Expect(sandboxes).To(HaveLen(2))
			target := &sandboxes[0]

			By("Setting valid retain-on-failure annotation")
			setAnnotation(target, agentsv1alpha1.AnnotationReuseRetainOnFailure, "5m")

			triggerReuseAndFail(target)

			By("Verifying ShutdownTime is set")
			Eventually(func() bool {
				_ = k8sClient.Get(ctx, client.ObjectKeyFromObject(target), target)
				return target.Spec.ShutdownTime != nil
			}, time.Second*30, time.Second).Should(BeTrue())

			By("Verifying sandbox still exists (not deleted)")
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(target), target)).To(Succeed())
		})

		It("should delete sandbox after retain duration expires", func() {
			sandboxes := listPoolSandboxes()
			Expect(sandboxes).To(HaveLen(2))
			target := &sandboxes[0]

			By("Setting short retain-on-failure annotation")
			setAnnotation(target, agentsv1alpha1.AnnotationReuseRetainOnFailure, "10s")

			triggerReuseAndFail(target)

			By("Waiting for ShutdownTime to be set")
			Eventually(func() bool {
				_ = k8sClient.Get(ctx, client.ObjectKeyFromObject(target), target)
				return target.Spec.ShutdownTime != nil
			}, time.Second*30, time.Second).Should(BeTrue())

			By("Waiting for sandbox to be deleted after retain duration expires")
			Eventually(func() bool {
				return isSandboxDeleted(target)
			}, time.Second*60, time.Second).Should(BeTrue())
		})
	})
})
