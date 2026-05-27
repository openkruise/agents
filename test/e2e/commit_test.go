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
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	jobutil "github.com/openkruise/agents/pkg/job"
)

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
				Name:      fmt.Sprintf("commit-e2e-sandbox-%d", time.Now().UnixNano()),
				Namespace: namespace,
			},
			Spec: agentsv1alpha1.SandboxSpec{
				EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:    "workspace",
									Image:   "nginx:stable-alpine3.23",
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

	Context("Commit webhook validation", func() {
		It("should reject commit when target pod does not exist", func() {
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
			err := k8sClient.Create(ctx, commit)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("pod not found"))
		})

		It("should reject commit with invalid image reference", func() {
			commit := &agentsv1alpha1.Commit{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("commit-bad-image-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.CommitSpec{
					PodName:       "some-pod",
					ContainerName: "workspace",
					Image:         "INVALID:::image",
				},
			}
			err := k8sClient.Create(ctx, commit)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("invalid image reference"))
		})
	})

	Context("Commit lifecycle", func() {
		It("should create a Job and transition to Running when pod exists", func() {
			// Create sandbox and wait for it to be Running
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			Eventually(func() agentsv1alpha1.SandboxPhase {
				got := &agentsv1alpha1.Sandbox{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name: sandbox.Name, Namespace: namespace,
				}, got); err != nil {
					return ""
				}
				return got.Status.Phase
			}, 120*time.Second, 3*time.Second).Should(Equal(agentsv1alpha1.SandboxRunning))

			// The sandbox controller creates a pod with the same name as the sandbox
			podName := sandbox.Name

			// Create commit targeting the sandbox pod
			commit := &agentsv1alpha1.Commit{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("commit-running-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.CommitSpec{
					PodName:       podName,
					ContainerName: "workspace",
					Image:         "localhost:5000/test-commit:e2e-running",
				},
			}
			Expect(k8sClient.Create(ctx, commit)).To(Succeed())

			// Should transition to Running (Job created)
			Eventually(func() agentsv1alpha1.CommitPhase {
				got := &agentsv1alpha1.Commit{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name: commit.Name, Namespace: namespace,
				}, got); err != nil {
					return ""
				}
				return got.Status.Phase
			}, 60*time.Second, 3*time.Second).Should(Equal(agentsv1alpha1.CommitRunning))

			// Verify Job was created
			commitGot := &agentsv1alpha1.Commit{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: commit.Name, Namespace: namespace,
			}, commitGot)).To(Succeed())

			jobName := jobutil.MakeJobName(string(commitGot.UID))
			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: jobName, Namespace: namespace,
			}, job)).To(Succeed())

			// Verify Job has correct labels
			Expect(job.Labels[jobutil.LabelCommitName]).To(Equal(commit.Name))

			// Verify StartTime is set
			Expect(commitGot.Status.StartTime).NotTo(BeNil())
		})

	})

	// TTL test is covered by unit tests. In kind E2E environment, the commit Job
	// uses a placeholder image (busybox) that doesn't complete reliably, making
	// TTL-based deletion unreliable to test.

	Context("Commit DryRun", func() {
		It("should create Job with DRY_RUN=true when dryRun is set", func() {
			// Create sandbox and wait for Running
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			Eventually(func() agentsv1alpha1.SandboxPhase {
				got := &agentsv1alpha1.Sandbox{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name: sandbox.Name, Namespace: namespace,
				}, got); err != nil {
					return ""
				}
				return got.Status.Phase
			}, 120*time.Second, 3*time.Second).Should(Equal(agentsv1alpha1.SandboxRunning))

			podName := sandbox.Name

			commit := &agentsv1alpha1.Commit{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("commit-dryrun-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.CommitSpec{
					PodName:       podName,
					ContainerName: "workspace",
					Image:         "localhost:5000/test-commit:e2e-dryrun",
					DryRun:        true,
				},
			}
			Expect(k8sClient.Create(ctx, commit)).To(Succeed())

			// Wait for Running or Succeeded (DryRun job may complete before poll catches Running)
			Eventually(func() bool {
				got := &agentsv1alpha1.Commit{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name: commit.Name, Namespace: namespace,
				}, got); err != nil {
					return false
				}
				return got.Status.Phase == agentsv1alpha1.CommitRunning || got.Status.Phase == agentsv1alpha1.CommitSucceeded
			}, 60*time.Second, 3*time.Second).Should(BeTrue())

			// Verify Job has DRY_RUN=true env
			commitGot := &agentsv1alpha1.Commit{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: commit.Name, Namespace: namespace,
			}, commitGot)).To(Succeed())

			jobName := jobutil.MakeJobName(string(commitGot.UID))
			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: jobName, Namespace: namespace,
			}, job)).To(Succeed())

			// Check DRY_RUN env in container
			found := false
			for _, env := range job.Spec.Template.Spec.Containers[0].Env {
				if env.Name == "DRY_RUN" && env.Value == "true" {
					found = true
					break
				}
			}
			Expect(found).To(BeTrue(), "expected DRY_RUN=true env in job container")
		})

		It("should complete full lifecycle Pending -> Running -> Succeeded with DryRun", func() {
			// Create sandbox and wait for Running
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			Eventually(func() agentsv1alpha1.SandboxPhase {
				got := &agentsv1alpha1.Sandbox{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name: sandbox.Name, Namespace: namespace,
				}, got); err != nil {
					return ""
				}
				return got.Status.Phase
			}, 120*time.Second, 3*time.Second).Should(Equal(agentsv1alpha1.SandboxRunning))

			podName := sandbox.Name

			commit := &agentsv1alpha1.Commit{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("commit-dryrun-full-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.CommitSpec{
					PodName:       podName,
					ContainerName: "workspace",
					Image:         "localhost:5000/test-commit:e2e-dryrun-full",
					DryRun:        true,
				},
			}
			Expect(k8sClient.Create(ctx, commit)).To(Succeed())

			// DryRun job exits 0 immediately, should reach Succeeded
			Eventually(func() agentsv1alpha1.CommitPhase {
				got := &agentsv1alpha1.Commit{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name: commit.Name, Namespace: namespace,
				}, got); err != nil {
					return ""
				}
				return got.Status.Phase
			}, 180*time.Second, 3*time.Second).Should(Equal(agentsv1alpha1.CommitSucceeded))

			// Verify CompletionTime and StartTime are set
			final := &agentsv1alpha1.Commit{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: commit.Name, Namespace: namespace,
			}, final)).To(Succeed())
			Expect(final.Status.CompletionTime).NotTo(BeNil())
			Expect(final.Status.StartTime).NotTo(BeNil())
		})
	})

	Context("Commit TLS registry", func() {
		const tlsRegistryHost = "tls-registry.e2e-registry.svc.cluster.local:5443"

		It("should push to HTTPS self-signed registry with insecureRegistry=true", func() {
			// Create sandbox and wait for Running
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			Eventually(func() agentsv1alpha1.SandboxPhase {
				got := &agentsv1alpha1.Sandbox{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name: sandbox.Name, Namespace: namespace,
				}, got); err != nil {
					return ""
				}
				return got.Status.Phase
			}, 120*time.Second, 3*time.Second).Should(Equal(agentsv1alpha1.SandboxRunning))

			podName := sandbox.Name

			// Create commit with insecureRegistry=true targeting TLS registry
			commit := &agentsv1alpha1.Commit{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("commit-tls-insecure-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.CommitSpec{
					PodName:          podName,
					ContainerName:    "workspace",
					Image:            tlsRegistryHost + "/test:insecure",
					InsecureRegistry: true,
				},
			}
			Expect(k8sClient.Create(ctx, commit)).To(Succeed())

			// Should transition to Running (Job created)
			Eventually(func() agentsv1alpha1.CommitPhase {
				got := &agentsv1alpha1.Commit{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name: commit.Name, Namespace: namespace,
				}, got); err != nil {
					return ""
				}
				return got.Status.Phase
			}, 60*time.Second, 3*time.Second).Should(Equal(agentsv1alpha1.CommitRunning))

			// Verify Job was created with INSECURE_REGISTRY=true
			commitGot := &agentsv1alpha1.Commit{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: commit.Name, Namespace: namespace,
			}, commitGot)).To(Succeed())

			jobName := jobutil.MakeJobName(string(commitGot.UID))
			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: jobName, Namespace: namespace,
			}, job)).To(Succeed())

			// Check INSECURE_REGISTRY env in container
			found := false
			for _, env := range job.Spec.Template.Spec.Containers[0].Env {
				if env.Name == "INSECURE_REGISTRY" && env.Value == "true" {
					found = true
					break
				}
			}
			Expect(found).To(BeTrue(), "expected INSECURE_REGISTRY=true env in job container")
		})

		It("should push to HTTPS self-signed registry with certs.d configured on node", func() {
			// Create sandbox and wait for Running
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			Eventually(func() agentsv1alpha1.SandboxPhase {
				got := &agentsv1alpha1.Sandbox{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name: sandbox.Name, Namespace: namespace,
				}, got); err != nil {
					return ""
				}
				return got.Status.Phase
			}, 120*time.Second, 3*time.Second).Should(Equal(agentsv1alpha1.SandboxRunning))

			podName := sandbox.Name

			// Create commit without insecureRegistry (relies on node certs.d)
			commit := &agentsv1alpha1.Commit{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("commit-tls-certs-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.CommitSpec{
					PodName:          podName,
					ContainerName:    "workspace",
					Image:            tlsRegistryHost + "/test:tls",
					InsecureRegistry: false,
				},
			}
			Expect(k8sClient.Create(ctx, commit)).To(Succeed())

			// Should transition to Running (Job created)
			Eventually(func() bool {
				got := &agentsv1alpha1.Commit{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name: commit.Name, Namespace: namespace,
				}, got); err != nil {
					return false
				}
				return got.Status.Phase == agentsv1alpha1.CommitRunning || got.Status.Phase == agentsv1alpha1.CommitSucceeded
			}, 60*time.Second, 3*time.Second).Should(BeTrue())

			// Verify Job has host-containerd-certs volume
			commitGot := &agentsv1alpha1.Commit{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: commit.Name, Namespace: namespace,
			}, commitGot)).To(Succeed())

			jobName := jobutil.MakeJobName(string(commitGot.UID))
			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: jobName, Namespace: namespace,
			}, job)).To(Succeed())

			// Check host-containerd-certs volume exists
			foundVolume := false
			for _, vol := range job.Spec.Template.Spec.Volumes {
				if vol.Name == "host-containerd-certs" {
					foundVolume = true
					break
				}
			}
			Expect(foundVolume).To(BeTrue(), "expected host-containerd-certs volume in job pod")

			// Check volume mount exists with ReadOnly
			foundMount := false
			for _, mount := range job.Spec.Template.Spec.Containers[0].VolumeMounts {
				if mount.Name == "host-containerd-certs" && mount.MountPath == "/etc/containerd/certs.d" && mount.ReadOnly {
					foundMount = true
					break
				}
			}
			Expect(foundMount).To(BeTrue(), "expected host-containerd-certs volume mount at /etc/containerd/certs.d (ReadOnly)")

			// Wait for Succeeded phase (node has certs.d configured, TLS handshake should work)
			Eventually(func() agentsv1alpha1.CommitPhase {
				got := &agentsv1alpha1.Commit{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name: commit.Name, Namespace: namespace,
				}, got); err != nil {
					return ""
				}
				return got.Status.Phase
			}, 180*time.Second, 3*time.Second).Should(Equal(agentsv1alpha1.CommitSucceeded))
		})
	})

	Context("Commit deletion", func() {
		It("should clean up resources when commit is deleted", func() {
			// Create sandbox and wait for Running
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			Eventually(func() agentsv1alpha1.SandboxPhase {
				got := &agentsv1alpha1.Sandbox{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name: sandbox.Name, Namespace: namespace,
				}, got); err != nil {
					return ""
				}
				return got.Status.Phase
			}, 120*time.Second, 3*time.Second).Should(Equal(agentsv1alpha1.SandboxRunning))

			podName := sandbox.Name

			commit := &agentsv1alpha1.Commit{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("commit-delete-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.CommitSpec{
					PodName:       podName,
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

			// Should be fully deleted (finalizer removed)
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
