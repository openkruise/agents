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
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	commitutil "github.com/openkruise/agents/pkg/utils/commit"
)

var commitSandboxImage = func() string {
	if img := os.Getenv("COMMIT_TEST_SANDBOX_IMAGE"); img != "" {
		return img
	}
	return "nginx:stable-alpine"
}()

var _ = Describe("Commit", func() {
	var (
		ctx       = context.Background()
		namespace string
		sandbox   *agentsv1alpha1.Sandbox
	)

	BeforeEach(func() {
		namespace = createNamespace(ctx)
		sandbox = &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("commit-e2e-%d", time.Now().UnixNano()),
				Namespace: namespace,
			},
			Spec: agentsv1alpha1.SandboxSpec{
				EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:    "workspace",
									Image:   commitSandboxImage,
									Command: []string{"sleep", "infinity"},
								},
							},
							RestartPolicy: corev1.RestartPolicyNever,
						},
					},
				},
			},
		}
	})

	AfterEach(func() {
		// Clean up commit resources
		commitList := &agentsv1alpha1.CommitList{}
		_ = k8sClient.List(ctx, commitList, client.InNamespace(namespace))
		for i := range commitList.Items {
			_ = k8sClient.Delete(ctx, &commitList.Items[i])
		}
		// Clean up sandbox
		_ = k8sClient.Delete(ctx, sandbox)
		// Clean up namespace
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		_ = k8sClient.Delete(ctx, ns)
	})

	Context("Commit pod not found", func() {
		It("should mark commit as Failed when target pod does not exist", func() {
			commit := &agentsv1alpha1.Commit{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("commit-no-pod-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.CommitSpec{
					PodName:       "nonexistent-pod",
					ContainerName: "workspace",
					Image:         "localhost:5000/test-commit:e2e-no-pod",
				},
			}
			Expect(k8sClient.Create(ctx, commit)).To(Succeed())

			Eventually(func() agentsv1alpha1.CommitPhase {
				got := &agentsv1alpha1.Commit{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name: commit.Name, Namespace: namespace,
				}, got); err != nil {
					return ""
				}
				return got.Status.Phase
			}, 30*time.Second, 2*time.Second).Should(Equal(agentsv1alpha1.CommitPhaseFailed))
		})
	})

	Context("Commit to TLS registry (self-signed cert, no insecureRegistry)", func() {
		// Uses in-cluster TLS registry deployed by hack/setup-local-registry.sh.
		// The registry runs at tls-registry.e2e-registry.svc.cluster.local:5443 with
		// a self-signed CA cert installed into kind nodes' /etc/containerd/certs.d/.
		// hostNetwork Job pods resolve the FQDN via /etc/hosts entries on kind nodes.
		// No insecureRegistry flag is needed — nerdctl trusts the CA via hosts.toml.
		const tlsRegistryHost = "tls-registry.e2e-registry.svc.cluster.local:5443"

		It("should commit and push successfully to TLS registry with certs.d configured", func() {
			// Create sandbox and wait for Running
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			waitSandboxRunning(ctx, namespace, sandbox.Name)

			// Create commit targeting the TLS registry (no auth needed, no insecureRegistry)
			commit := &agentsv1alpha1.Commit{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("commit-tls-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.CommitSpec{
					PodName:       sandbox.Name,
					ContainerName: "workspace",
					Image:         fmt.Sprintf("%s/test-commit:tls-%d", tlsRegistryHost, time.Now().UnixNano()),
				},
			}
			Expect(k8sClient.Create(ctx, commit)).To(Succeed())

			// Should reach Succeeded — full TLS push via certs.d CA trust
			Eventually(func() agentsv1alpha1.CommitPhase {
				got := &agentsv1alpha1.Commit{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name: commit.Name, Namespace: namespace,
				}, got); err != nil {
					return ""
				}
				return got.Status.Phase
			}, 180*time.Second, 5*time.Second).Should(Equal(agentsv1alpha1.CommitPhaseSucceeded))

			verifyCommitJobCompleted(ctx, namespace, commit.Name)
		})
	})

	Context("Commit deletion", func() {
		It("should clean up resources when commit is deleted", func() {
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			waitSandboxRunning(ctx, namespace, sandbox.Name)

			commit := &agentsv1alpha1.Commit{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("commit-delete-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.CommitSpec{
					PodName:       sandbox.Name,
					ContainerName: "workspace",
					Image:         "localhost:5000/test-commit:e2e-delete",
				},
			}
			Expect(k8sClient.Create(ctx, commit)).To(Succeed())

			// Wait for finalizer to be added
			Eventually(func() []string {
				got := &agentsv1alpha1.Commit{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name: commit.Name, Namespace: namespace,
				}, got); err != nil {
					return nil
				}
				return got.Finalizers
			}, 30*time.Second, 2*time.Second).Should(ContainElement(agentsv1alpha1.CommitFinalizer))

			// Delete the commit
			Expect(k8sClient.Delete(ctx, commit)).To(Succeed())

			// Should be fully deleted
			Eventually(func() bool {
				got := &agentsv1alpha1.Commit{}
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name: commit.Name, Namespace: namespace,
				}, got)
				return err != nil
			}, 30*time.Second, 2*time.Second).Should(BeTrue())
		})
	})
})

// --- Helpers ---

func waitSandboxRunning(ctx context.Context, namespace, name string) {
	Eventually(func() agentsv1alpha1.SandboxPhase {
		got := &agentsv1alpha1.Sandbox{}
		if err := k8sClient.Get(ctx, types.NamespacedName{
			Name: name, Namespace: namespace,
		}, got); err != nil {
			return ""
		}
		return got.Status.Phase
	}, 120*time.Second, 3*time.Second).Should(Equal(agentsv1alpha1.SandboxRunning))
}

func verifyCommitJobCompleted(ctx context.Context, namespace, commitName string) {
	commitGot := &agentsv1alpha1.Commit{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{
		Name: commitName, Namespace: namespace,
	}, commitGot)).To(Succeed())

	jobList := &batchv1.JobList{}
	Expect(k8sClient.List(ctx, jobList,
		client.InNamespace(namespace),
		client.MatchingLabels{commitutil.LabelCommitUID: string(commitGot.UID)},
	)).To(Succeed())
	Expect(jobList.Items).NotTo(BeEmpty())

	// Verify completion time is set
	Expect(commitGot.Status.CompletionTime).NotTo(BeNil())
	Expect(commitGot.Status.StartTime).NotTo(BeNil())
}
