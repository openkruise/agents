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
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// okactlBinary returns the path to the okactl binary. It checks the OKACTL_BIN
// environment variable first, then falls back to "bin/okactl" in the project root.
func okactlBinary() string {
	if bin := os.Getenv("OKACTL_BIN"); bin != "" {
		return bin
	}
	return "bin/okactl"
}

// runOkactl executes okactl with the given arguments and returns combined output.
func runOkactl(args ...string) (string, error) {
	cmd := exec.Command(okactlBinary(), args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+os.Getenv("KUBECONFIG"))
	output, err := cmd.CombinedOutput()
	return string(output), err
}

var _ = Describe("okactl CLI", func() {
	var (
		ctx       = context.Background()
		namespace string
	)

	BeforeEach(func() {
		namespace = createNamespace(ctx)
	})

	AfterEach(func() {
		ns := &corev1.Namespace{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: namespace}, ns); err == nil {
			_ = k8sClient.Delete(ctx, ns)
		}
	})

	Context("scale sandboxset", func() {
		It("should scale a SandboxSet to the desired replicas", func() {
			By("Creating a SandboxSet with 1 replica")
			sbs := &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("okactl-scale-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxSetSpec{
					Replicas: 1,
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:    "test-container",
										Image:   "nginx:stable-alpine3.23",
										Command: []string{"sleep", "infinity"},
									},
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, sbs)).To(Succeed())

			By("Running okactl scale sbs to 3 replicas (using short name)")
			output, err := runOkactl("-n", namespace, "scale", "sbs", sbs.Name, "--replicas=3")
			Expect(err).NotTo(HaveOccurred(), "okactl output: %s", output)
			Expect(output).To(ContainSubstring("scaled to 3"))

			By("Verifying SandboxSet spec.replicas is now 3")
			updated := &agentsv1alpha1.SandboxSet{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbs.Name, Namespace: namespace}, updated)).To(Succeed())
			Expect(updated.Spec.Replicas).To(Equal(int32(3)))
		})

		It("should fail when SandboxSet does not exist", func() {
			output, err := runOkactl("-n", namespace, "scale", "sbs", "non-existent-sbs", "--replicas=2")
			Expect(err).To(HaveOccurred())
			Expect(output).To(ContainSubstring("not found"))
		})
	})

	Context("set image sandboxset", func() {
		It("should update the container image of a SandboxSet", func() {
			By("Creating a SandboxSet")
			sbs := &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("okactl-setimg-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxSetSpec{
					Replicas: 1,
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:    "app",
										Image:   "nginx:1.25",
										Command: []string{"sleep", "infinity"},
									},
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, sbs)).To(Succeed())

			By("Running okactl set image sbs (using short name)")
			output, err := runOkactl("-n", namespace, "set", "image", "sbs", sbs.Name, "app=nginx:1.27")
			Expect(err).NotTo(HaveOccurred(), "okactl output: %s", output)
			Expect(output).To(ContainSubstring("image updated"))

			By("Verifying the container image was updated")
			updated := &agentsv1alpha1.SandboxSet{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbs.Name, Namespace: namespace}, updated)).To(Succeed())
			Expect(updated.Spec.Template.Spec.Containers[0].Image).To(Equal("nginx:1.27"))
		})

		It("should fail when container name does not exist", func() {
			sbs := &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("okactl-setimg-bad-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxSetSpec{
					Replicas: 1,
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:    "app",
										Image:   "nginx:1.25",
										Command: []string{"sleep", "infinity"},
									},
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, sbs)).To(Succeed())

			output, err := runOkactl("-n", namespace, "set", "image", "sbs", sbs.Name, "nonexistent=nginx:1.27")
			Expect(err).To(HaveOccurred())
			Expect(output).To(ContainSubstring("not found"))
		})
	})

	Context("create suo", func() {
		It("should create a SandboxUpdateOps for claimed sandboxes", func() {
			By("Creating a Sandbox with claim label")
			sbx := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("okactl-suo-sbx-%d", time.Now().UnixNano()),
					Namespace: namespace,
					Labels: map[string]string{
						"okactl-test":                        "create-suo",
						agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.True,
					},
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:    "app",
										Image:   "nginx:1.25",
										Command: []string{"sleep", "infinity"},
									},
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, sbx)).To(Succeed())

			By("Running okactl create suo")
			output, err := runOkactl("-n", namespace, "create", "suo", "-l", "okactl-test=create-suo", "app=nginx:1.27")
			Expect(err).NotTo(HaveOccurred(), "okactl output: %s", output)
			Expect(output).To(ContainSubstring("created"))

			By("Verifying a SandboxUpdateOps was created")
			suoList := &agentsv1alpha1.SandboxUpdateOpsList{}
			Expect(k8sClient.List(ctx, suoList, client.InNamespace(namespace))).To(Succeed())
			Expect(suoList.Items).NotTo(BeEmpty())

			// Verify the SUO targets the correct selector
			suo := suoList.Items[0]
			Expect(suo.Spec.Selector.MatchLabels).To(HaveKeyWithValue("okactl-test", "create-suo"))
		})

		It("should fail when no sandboxes match the selector", func() {
			output, err := runOkactl("-n", namespace, "create", "suo", "-l", "nonexistent=label", "app=nginx:1.27")
			Expect(err).To(HaveOccurred())
			Expect(output).To(ContainSubstring("no sandboxes found"))
		})
	})

	Context("restart sandbox", func() {
		It("should create a ContainerRecreateRequest for the sandbox", func() {
			By("Creating a Sandbox")
			sbx := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("okactl-restart-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:    "app",
										Image:   "nginx:stable-alpine3.23",
										Command: []string{"sleep", "infinity"},
									},
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, sbx)).To(Succeed())

			By("Waiting for the Sandbox to be Running")
			Eventually(func() agentsv1alpha1.SandboxPhase {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: sbx.Name, Namespace: namespace}, sbx)
				return sbx.Status.Phase
			}, 3*time.Minute, time.Second).Should(Equal(agentsv1alpha1.SandboxRunning))

			By("Running okactl restart sandbox")
			output, err := runOkactl("-n", namespace, "restart", "sandbox", sbx.Name, "-c", "app")
			Expect(err).NotTo(HaveOccurred(), "okactl output: %s", output)
			Expect(output).To(ContainSubstring("created"))
			Expect(output).To(ContainSubstring(sbx.Name))

			By("Verifying a ContainerRecreateRequest was created")
			crrGVR := schema.GroupVersionResource{
				Group:    "apps.kruise.io",
				Version:  "v1alpha1",
				Resource: "containerrecreaterequests",
			}
			crrList, err := clientset.Discovery().ServerResourcesForGroupVersion("apps.kruise.io/v1alpha1")
			if err != nil {
				Skip("OpenKruise CRDs not available, skipping CRR verification")
			}
			_ = crrList

			dynClient := clientset.RESTClient()
			_ = dynClient

			// Use unstructured client to list CRRs
			crrItems := &unstructured.UnstructuredList{}
			crrItems.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "apps.kruise.io",
				Version: "v1alpha1",
				Kind:    "ContainerRecreateRequestList",
			})
			err = k8sClient.List(ctx, crrItems, client.InNamespace(namespace))
			if err != nil {
				Skip("Cannot list ContainerRecreateRequests: " + err.Error())
			}

			// Find CRR matching our sandbox
			found := false
			for _, item := range crrItems.Items {
				spec, ok := item.Object["spec"].(map[string]interface{})
				if ok && spec["podName"] == sbx.Name {
					found = true
					break
				}
			}
			Expect(found).To(BeTrue(), "expected a CRR targeting pod %s", sbx.Name)
			_ = crrGVR
		})

		It("should fail when sandbox does not exist", func() {
			output, err := runOkactl("-n", namespace, "restart", "sandbox", "non-existent-sbx", "-c", "app")
			Expect(err).To(HaveOccurred())
			Expect(output).To(ContainSubstring("failed to get sandbox"))
		})
	})

	Context("status sbs", func() {
		It("should show the update progress of a SandboxSet", func() {
			By("Creating a SandboxSet with 1 replica")
			sbs := &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("okactl-status-sbs-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxSetSpec{
					Replicas: 1,
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:    "app",
										Image:   "nginx:1.25",
										Command: []string{"sleep", "infinity"},
									},
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, sbs)).To(Succeed())

			By("Running okactl status sbs")
			output, err := runOkactl("-n", namespace, "status", "sbs", sbs.Name)
			Expect(err).NotTo(HaveOccurred(), "okactl output: %s", output)
			Expect(output).To(ContainSubstring(sbs.Name))
		})

		It("should fail when SandboxSet does not exist", func() {
			output, err := runOkactl("-n", namespace, "status", "sbs", "non-existent-sbs")
			Expect(err).To(HaveOccurred())
			Expect(output).To(ContainSubstring("failed to get sandboxset"))
		})
	})

	Context("status suo", func() {
		It("should show the progress of a SandboxUpdateOps", func() {
			By("Creating a SandboxUpdateOps")
			suo := &agentsv1alpha1.SandboxUpdateOps{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("okactl-status-suo-%d", time.Now().UnixNano()),
					Namespace: namespace,
				},
				Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"okactl-test": "status-suo"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, suo)).To(Succeed())

			By("Running okactl status suo")
			output, err := runOkactl("-n", namespace, "status", "suo", suo.Name)
			Expect(err).NotTo(HaveOccurred(), "okactl output: %s", output)
			Expect(output).To(ContainSubstring(suo.Name))
		})

		It("should fail when SandboxUpdateOps does not exist", func() {
			output, err := runOkactl("-n", namespace, "status", "suo", "non-existent-suo")
			Expect(err).To(HaveOccurred())
			Expect(output).To(ContainSubstring("failed to get sandboxupdateops"))
		})
	})

	Context("error handling", func() {
		It("should show help for unknown subcommands", func() {
			output, err := runOkactl("unknown-command")
			Expect(err).To(HaveOccurred())
			Expect(output).To(Or(ContainSubstring("unknown command"), ContainSubstring("Usage")))
		})

		It("should require --replicas flag for scale", func() {
			output, err := runOkactl("-n", namespace, "scale", "sbs", "some-sbs")
			Expect(err).To(HaveOccurred())
			Expect(strings.ToLower(output)).To(ContainSubstring("required"))
		})
	})
})
