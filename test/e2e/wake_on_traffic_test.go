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
	"net/http"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	e2bmodels "github.com/openkruise/agents/pkg/servers/e2b/models"
)

const (
	wakeManagerNamespace = "sandbox-system"
	wakeManagerService   = "sandbox-manager:http-envoy"
	wakeGatewayService   = "sandbox-gateway:http"
	wakeE2BAdminKey      = "some-api-key"
)

var _ = Describe("WakeOnTraffic", func() {
	var (
		ctx          = context.Background()
		namespace    string
		templateName string
	)

	BeforeEach(func() {
		namespace = createNamespace(ctx)
		templateName = fmt.Sprintf("wake-template-%d", time.Now().UnixNano())
	})

	AfterEach(func() {
		ns := &corev1.Namespace{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: namespace}, ns); err == nil {
			_ = k8sClient.Delete(ctx, ns)
		}
	})

	It("resumes a paused auto-resume sandbox from gateway traffic", func() {
		By("Creating a template pool with an HTTP container")
		sandboxSet := &agentsv1alpha1.SandboxSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      templateName,
				Namespace: namespace,
			},
			Spec: agentsv1alpha1.SandboxSetSpec{
				Replicas: 1,
				EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "nginx",
									Image: "nginx:stable-alpine3.23",
									Ports: []corev1.ContainerPort{
										{
											Name:          "http",
											ContainerPort: 80,
										},
									},
								},
							},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, sandboxSet)).To(Succeed())

		Eventually(func(g Gomega) {
			latest := &agentsv1alpha1.SandboxSet{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      sandboxSet.Name,
				Namespace: sandboxSet.Namespace,
			}, latest)).To(Succeed())
			g.Expect(latest.Status.AvailableReplicas).To(Equal(int32(1)))
		}, 5*time.Minute, time.Second).Should(Succeed())

		By("Creating an E2B sandbox with autoResume.enabled=true and timeout=60")
		createBody, err := json.Marshal(e2bmodels.NewSandboxRequest{
			TemplateID: templateName,
			Timeout:    60,
			AutoResume: &e2bmodels.AutoResumeConfig{Enabled: true},
			Metadata: map[string]string{
				e2bmodels.ExtensionKeySkipInitRuntime: agentsv1alpha1.True,
			},
		})
		Expect(err).NotTo(HaveOccurred())
		createRaw, err := managerAPIProxyRaw(ctx, http.MethodPost, []string{"sandboxes"}, createBody)
		Expect(err).NotTo(HaveOccurred())

		var created e2bmodels.Sandbox
		Expect(json.Unmarshal(createRaw, &created)).To(Succeed())
		Expect(created.SandboxID).NotTo(BeEmpty())

		sandboxKey := sandboxObjectKey(created.SandboxID)
		Eventually(func(g Gomega) {
			sandbox := &agentsv1alpha1.Sandbox{}
			g.Expect(k8sClient.Get(ctx, sandboxKey, sandbox)).To(Succeed())
			g.Expect(sandbox.Annotations).To(HaveKeyWithValue(agentsv1alpha1.AnnotationWakeOnTraffic, "timeout:60s"))
			g.Expect(sandbox.Spec.ShutdownTime).NotTo(BeNil())
		}, time.Minute, time.Second).Should(Succeed())

		By("Pausing the sandbox via E2B")
		_, err = managerAPIProxyRaw(ctx, http.MethodPost, []string{"sandboxes", created.SandboxID, "pause"}, nil)
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			sandbox := &agentsv1alpha1.Sandbox{}
			g.Expect(k8sClient.Get(ctx, sandboxKey, sandbox)).To(Succeed())
			g.Expect(sandbox.Spec.Paused).To(BeTrue())
			g.Expect(sandbox.Annotations).To(HaveKeyWithValue(agentsv1alpha1.AnnotationWakeOnTraffic, "timeout:60s"))
		}, 3*time.Minute, time.Second).Should(Succeed())

		By("Sending data-plane traffic through the gateway")
		wakeStart := time.Now()
		_, err = serviceProxyRaw(ctx, http.MethodGet, wakeGatewayService, nil, nil, map[string]string{
			"e2b-sandbox-id":   created.SandboxID,
			"e2b-sandbox-port": "80",
		})
		Expect(err).NotTo(HaveOccurred())

		By("Verifying the sandbox resumed and wake kept the original annotation")
		Eventually(func(g Gomega) {
			sandbox := &agentsv1alpha1.Sandbox{}
			g.Expect(k8sClient.Get(ctx, sandboxKey, sandbox)).To(Succeed())
			g.Expect(sandbox.Spec.Paused).To(BeFalse())
			g.Expect(sandbox.Spec.ShutdownTime).NotTo(BeNil())
			g.Expect(sandbox.Annotations).To(HaveKeyWithValue(agentsv1alpha1.AnnotationWakeOnTraffic, "timeout:60s"))
			g.Expect(sandbox.Spec.ShutdownTime.Time).To(BeTemporally("~", wakeStart.Add(5*time.Minute), time.Minute))
		}, 3*time.Minute, time.Second).Should(Succeed())
	})
})

func serviceProxyRaw(ctx context.Context, method, service string, pathSegments []string, body []byte, headers map[string]string) ([]byte, error) {
	req := clientset.CoreV1().RESTClient().
		Verb(method).
		Namespace(wakeManagerNamespace).
		Resource("services").
		Name(service).
		SubResource("proxy")
	if len(pathSegments) > 0 {
		req = req.Suffix(pathSegments...)
	}
	for key, value := range headers {
		req = req.SetHeader(key, value)
	}
	if body != nil {
		req = req.SetHeader("Content-Type", "application/json").Body(body)
	}
	return req.DoRaw(ctx)
}

func e2bHeaders() map[string]string {
	return map[string]string{
		"X-API-KEY": wakeE2BAdminKey,
	}
}

func managerAPIProxyRaw(ctx context.Context, method string, pathSegments []string, body []byte) ([]byte, error) {
	return serviceProxyRaw(ctx, method, wakeManagerService, append([]string{"kruise", "api"}, pathSegments...), body, e2bHeaders())
}

func sandboxObjectKey(sandboxID string) client.ObjectKey {
	parts := strings.SplitN(sandboxID, "--", 2)
	Expect(parts).To(HaveLen(2))
	return types.NamespacedName{
		Namespace: parts[0],
		Name:      parts[1],
	}
}
