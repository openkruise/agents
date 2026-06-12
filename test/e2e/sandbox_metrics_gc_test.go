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
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

const (
	// metricsPollTimeout is the upper bound for /metrics endpoint based
	// assertions. The GC controller is event-driven and typically reacts within
	// a few seconds; we allow 2 minutes to absorb scrape jitter and CI latency.
	metricsPollTimeout = 2 * time.Minute
	// metricsPollInterval controls how often we re-poll the /metrics endpoint.
	metricsPollInterval = time.Second
	// sandboxPhaseTimeout is the upper bound for waiting on Sandbox phase
	// transitions, matching the convention used elsewhere in this suite.
	sandboxPhaseTimeout  = 10 * time.Minute
	sandboxPhaseInterval = time.Second
)

var _ = Describe("Sandbox Metrics GC", func() {
	var (
		ctx       = context.Background()
		namespace string
	)

	BeforeEach(func() {
		namespace = createNamespace(ctx)
	})

	Context("single sandbox deletion", func() {
		It("should cleanup prometheus metrics after sandbox is deleted", func() {
			sandbox := newMetricsGCSandbox(namespace, "test-sandbox-metrics-gc")

			By("Creating a sandbox")
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, sandbox)
			})

			By("Waiting for sandbox to be Running")
			Eventually(func() agentsv1alpha1.SandboxPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name: sandbox.Name, Namespace: sandbox.Namespace,
				}, sandbox)
				return sandbox.Status.Phase
			}, sandboxPhaseTimeout, sandboxPhaseInterval).Should(Equal(agentsv1alpha1.SandboxRunning))

			labels := map[string]string{
				"namespace": namespace,
				"name":      sandbox.Name,
			}

			By("Verifying sandbox_info series is published while sandbox is alive")
			Eventually(func() bool {
				body, err := fetchControllerMetrics(ctx)
				if err != nil {
					return false
				}
				return metricSeriesExists(body, "sandbox_info", labels)
			}, metricsPollTimeout, metricsPollInterval).Should(BeTrue(),
				"sandbox_info should be exposed for the running sandbox")

			By("Capturing baseline reconcile count for sandbox-metrics-gc controller")
			baseline := readGCReconcileTotal(ctx)

			By("Deleting the sandbox")
			Expect(k8sClient.Delete(ctx, sandbox)).To(Succeed())

			By("Verifying sandbox_info series is dropped after deletion")
			Eventually(func() bool {
				body, err := fetchControllerMetrics(ctx)
				if err != nil {
					// transient scrape error: keep retrying
					return false
				}
				return !metricSeriesExists(body, "sandbox_info", labels)
			}, metricsPollTimeout, metricsPollInterval).Should(BeTrue(),
				"sandbox_info should be removed after sandbox deletion")

			By("Verifying ancillary per-sandbox series are also dropped")
			body, err := fetchControllerMetrics(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(metricSeriesExists(body, "sandbox_created", labels)).To(BeFalse(),
				"sandbox_created should be dropped")
			Expect(metricSeriesExists(body, "sandbox_status_ready", labels)).To(BeFalse(),
				"sandbox_status_ready should be dropped")

			By("Verifying sandbox-metrics-gc controller observed at least one reconcile")
			Eventually(func() float64 {
				return readGCReconcileTotal(ctx)
			}, metricsPollTimeout, metricsPollInterval).Should(BeNumerically(">", baseline),
				"controller_runtime_reconcile_total{controller=sandbox-metrics-gc} should advance")

			By("Verifying no events were dropped due to a full enqueue channel")
			body, err = fetchControllerMetrics(ctx)
			Expect(err).NotTo(HaveOccurred())
			dropped, derr := getCounterValue(body, "sandbox_metrics_gc_dropped_total",
				map[string]string{"reason": "channel_full"})
			if derr == nil {
				Expect(dropped).To(Equal(float64(0)),
					"sandbox_metrics_gc_dropped_total{reason=channel_full} should remain 0 in steady-state E2E")
			}
		})
	})

	Context("bulk sandbox deletion", func() {
		It("should cleanup metrics for all sandboxes in a SandboxSet after scale-to-zero", func() {
			const replicas int32 = 5

			set := &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-sbs-metrics-gc-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxSetSpec{
					Replicas: replicas,
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{"sandboxset": "true"},
							},
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

			By("Creating a SandboxSet with 5 replicas")
			Expect(k8sClient.Create(ctx, set)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, set)
			})

			By("Waiting for all replicas to become available")
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name: set.Name, Namespace: set.Namespace,
				}, set)
				return set.Status.AvailableReplicas
			}, sandboxPhaseTimeout, sandboxPhaseInterval).Should(Equal(replicas))

			By("Listing sandboxes generated by the SandboxSet")
			sandboxList := &agentsv1alpha1.SandboxList{}
			Expect(k8sClient.List(ctx, sandboxList,
				client.InNamespace(namespace),
				client.MatchingLabels{agentsv1alpha1.LabelSandboxPool: set.Name},
			)).To(Succeed())
			Expect(len(sandboxList.Items)).To(Equal(int(replicas)))

			sandboxNames := make([]string, 0, len(sandboxList.Items))
			for i := range sandboxList.Items {
				sandboxNames = append(sandboxNames, sandboxList.Items[i].Name)
			}

			By("Verifying sandbox_info series are exposed for every replica")
			Eventually(func() bool {
				body, err := fetchControllerMetrics(ctx)
				if err != nil {
					return false
				}
				for _, name := range sandboxNames {
					if !metricSeriesExists(body, "sandbox_info", map[string]string{
						"namespace": namespace, "name": name,
					}) {
						return false
					}
				}
				return true
			}, metricsPollTimeout, metricsPollInterval).Should(BeTrue())

			By("Capturing baseline reconcile count before bulk deletion")
			baseline := readGCReconcileTotal(ctx)

			By("Scaling SandboxSet to zero")
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: set.Name, Namespace: set.Namespace,
			}, set)).To(Succeed())
			set.Spec.Replicas = 0
			Expect(k8sClient.Update(ctx, set)).To(Succeed())

			By("Verifying sandbox_info is dropped for every previously running sandbox")
			Eventually(func() bool {
				body, err := fetchControllerMetrics(ctx)
				if err != nil {
					return false
				}
				for _, name := range sandboxNames {
					if metricSeriesExists(body, "sandbox_info", map[string]string{
						"namespace": namespace, "name": name,
					}) {
						return false
					}
				}
				return true
			}, metricsPollTimeout, metricsPollInterval).Should(BeTrue(),
				"all per-sandbox sandbox_info series must be GC'd after scale-to-zero")

			By("Verifying GC controller processed at least the deleted sandboxes")
			Eventually(func() float64 {
				return readGCReconcileTotal(ctx)
			}, metricsPollTimeout, metricsPollInterval).Should(BeNumerically(">=", baseline+float64(replicas)),
				"controller_runtime_reconcile_total should advance by at least the number of deleted sandboxes")

			By("Verifying no enqueue events were dropped")
			body, err := fetchControllerMetrics(ctx)
			Expect(err).NotTo(HaveOccurred())
			dropped, derr := getCounterValue(body, "sandbox_metrics_gc_dropped_total",
				map[string]string{"reason": "channel_full"})
			if derr == nil {
				Expect(dropped).To(Equal(float64(0)),
					"sandbox_metrics_gc_dropped_total{reason=channel_full} must stay 0 under normal load")
			}
		})
	})

	Context("GC controller self-observability", func() {
		It("should expose controller-runtime reconcile metrics for sandbox-metrics-gc", func() {
			sandbox := newMetricsGCSandbox(namespace, "test-sandbox-metrics-gc-obs")

			By("Creating then deleting a sandbox to drive at least one GC reconcile")
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			Eventually(func() agentsv1alpha1.SandboxPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{
					Name: sandbox.Name, Namespace: sandbox.Namespace,
				}, sandbox)
				return sandbox.Status.Phase
			}, sandboxPhaseTimeout, sandboxPhaseInterval).Should(Equal(agentsv1alpha1.SandboxRunning))
			Expect(k8sClient.Delete(ctx, sandbox)).To(Succeed())

			By("Verifying controller_runtime_reconcile_total exposes the sandbox-metrics-gc series")
			Eventually(func() bool {
				body, err := fetchControllerMetrics(ctx)
				if err != nil {
					return false
				}
				return metricSeriesExists(body, "controller_runtime_reconcile_total", map[string]string{
					"controller": "sandbox-metrics-gc",
					"result":     "success",
				})
			}, metricsPollTimeout, metricsPollInterval).Should(BeTrue())

			By("Verifying controller_runtime_reconcile_time_seconds histogram is exposed")
			body, err := fetchControllerMetrics(ctx)
			Expect(err).NotTo(HaveOccurred())
			ok, err := metricHistogramHasObservation(body,
				"controller_runtime_reconcile_time_seconds",
				map[string]string{"controller": "sandbox-metrics-gc"})
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue(),
				"histogram controller_runtime_reconcile_time_seconds must have at least one observation for sandbox-metrics-gc")

			By("Verifying sandbox_metrics_gc_dropped_total metric family is registered")
			// The metric is registered eagerly by the GC controller's init,
			// so the family should appear in /metrics even with a zero value.
			Expect(body).To(ContainSubstring("sandbox_metrics_gc_dropped_total"),
				"sandbox_metrics_gc_dropped_total must be registered in /metrics")
		})
	})
})

// newMetricsGCSandbox returns a minimal Sandbox spec suitable for verifying
// metric lifecycle. The image and resources mirror the patterns used by
// sandbox_test.go to keep startup latency low.
func newMetricsGCSandbox(namespace, prefix string) *agentsv1alpha1.Sandbox {
	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()),
			Namespace: namespace,
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "test-container",
								Image: "nginx:stable-alpine3.23",
								Ports: []corev1.ContainerPort{
									{Name: "http", ContainerPort: 80},
								},
							},
						},
						RestartPolicy: corev1.RestartPolicyNever,
					},
				},
			},
		},
	}
}

// readGCReconcileTotal returns the total number of successful reconciles
// observed by the sandbox-metrics-gc controller, or 0 if the series is not yet
// present (e.g. on a freshly-started controller). Errors are swallowed so the
// helper is safe inside Eventually polling loops.
func readGCReconcileTotal(ctx context.Context) float64 {
	body, err := fetchControllerMetrics(ctx)
	if err != nil {
		return 0
	}
	val, err := getCounterValue(body, "controller_runtime_reconcile_total", map[string]string{
		"controller": "sandbox-metrics-gc",
		"result":     "success",
	})
	if err != nil {
		return 0
	}
	return val
}
