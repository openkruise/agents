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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	e2bmodels "github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/tracing"
)

const (
	managerNamespace      = "sandbox-system"
	managerDeploymentName = "sandbox-manager"
	managerPodSelector    = "component=sandbox-manager"
	managerContainerName  = "controller"

	// managerURLEnv points the suite at the sandbox-manager E2B API, e.g.
	// "http://localhost:7788" behind a kubectl port-forward. The Full Chain
	// suite skips itself when it is unset so workflows that deploy only the
	// controller are unaffected.
	managerURLEnv = "TRACING_E2E_MANAGER_URL"
	// apiKeyEnv overrides the E2B API key; defaults to the admin key from
	// config/sandbox-manager ("some-api-key").
	apiKeyEnv            = "E2B_API_KEY"
	defaultManagerAPIKey = "some-api-key"
)

// The Tracing Full Chain suite verifies the manager-side half of the tracing
// pipeline that the Tracing Stdout suite cannot cover: a real HTTP request to
// the sandbox-manager E2B API with a caller-provided X-Request-ID must
// produce a manager root span whose TraceID equals the request ID
// (requestID == TraceID contract), inject that trace context into the created
// Sandbox CR annotation, and finally show up in the controller's spans — all
// under --tracing-mode=std on both components, asserting on their stdout.
//
// The SandboxSet is created with replicas=0 so the claim takes the
// create-on-no-stock path: a fresh Sandbox CR is created inside the request,
// guaranteeing the annotation carries this request's traceparent and that the
// controller performs a write (CreatePod), so its spans are never dropped as
// no-op by the FilteringSpanProcessor.
var _ = Describe("Tracing Full Chain", func() {
	var (
		ctx         = context.Background()
		managerURL  string
		apiKey      string
		sandboxSet  *agentsv1alpha1.SandboxSet
		sandboxName string
		httpClient  = &http.Client{Timeout: 4 * time.Minute}
	)

	BeforeEach(func() {
		managerURL = strings.TrimRight(os.Getenv(managerURLEnv), "/")
		if managerURL == "" {
			Skip(managerURLEnv + " is not set; sandbox-manager is not reachable")
		}
		apiKey = os.Getenv(apiKeyEnv)
		if apiKey == "" {
			apiKey = defaultManagerAPIKey
		}
		for _, target := range []struct{ ns, name, arg string }{
			{managerNamespace, managerDeploymentName, "--tracing-mode=std"},
			{controllerNamespace, controllerDeploymentName, "--tracing-mode=std"},
		} {
			deploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: target.ns,
				Name:      target.name,
			}, deploy)).To(Succeed())
			if !deploymentHasArg(deploy, target.arg) {
				Skip(target.name + " is not running with " + target.arg)
			}
		}
	})

	AfterEach(func() {
		if sandboxName != "" {
			_ = k8sClient.Delete(ctx, &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{
				Namespace: Namespace, Name: sandboxName,
			}})
		}
		if sandboxSet != nil {
			_ = k8sClient.Delete(ctx, sandboxSet)
		}
	})

	It("should propagate the request ID as trace ID from manager through CR annotation to controller", func() {
		By("Creating an empty SandboxSet so the claim creates a fresh Sandbox CR")
		templateID := fmt.Sprintf("tracing-chain-%d", time.Now().UnixNano())
		sandboxSet = &agentsv1alpha1.SandboxSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      templateID,
				Namespace: Namespace,
			},
			Spec: agentsv1alpha1.SandboxSetSpec{
				Replicas: 0,
				EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "sandbox",
									Image: "nginx:stable-alpine3.20",
								},
							},
							TerminationGracePeriodSeconds: ptrInt64(1),
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, sandboxSet)).To(Succeed())

		By("POSTing /sandboxes with a caller-provided X-Request-ID")
		requestID := randomHex(16)
		body, err := json.Marshal(map[string]any{
			"templateID": templateID,
			"timeout":    300,
			"metadata": map[string]string{
				// The template has no agent-runtime, so skip runtime init.
				e2bmodels.ExtensionKeySkipInitRuntime: "true",
				// Bound the blocking wait-ready inside the claim.
				e2bmodels.ExtensionKeyClaimTimeout: "180",
			},
		})
		Expect(err).NotTo(HaveOccurred())
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, managerURL+"/sandboxes", bytes.NewReader(body))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(e2bmodels.HeaderApiKey, apiKey)
		req.Header.Set("X-Request-ID", requestID)
		resp, err := httpClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		respBody, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Body.Close()).To(Succeed())
		Expect(resp.StatusCode).To(Equal(http.StatusCreated), "create sandbox failed: %s", string(respBody))

		By("Verifying the response echoes the caller-provided X-Request-ID unchanged")
		Expect(resp.Header.Get("X-Request-ID")).To(Equal(requestID))

		var created e2bmodels.Sandbox
		Expect(json.Unmarshal(respBody, &created)).To(Succeed())
		// sandboxID is "<namespace>--<name>"; recover the CR name for lookup
		// and cleanup.
		nsAndName := strings.SplitN(created.SandboxID, "--", 2)
		Expect(nsAndName).To(HaveLen(2), "unexpected sandboxID format: %s", created.SandboxID)
		Expect(nsAndName[0]).To(Equal(Namespace))
		sandboxName = nsAndName[1]

		By("Verifying the Sandbox CR annotation carries a traceparent with TraceID == request ID")
		sandbox := &agentsv1alpha1.Sandbox{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: Namespace, Name: sandboxName}, sandbox)).To(Succeed())
		traceparent := sandbox.Annotations[tracing.TraceContextAnnotationKey]
		Expect(traceparent).NotTo(BeEmpty(), "trace-context annotation missing on Sandbox CR")
		Expect(traceparent).To(ContainSubstring(requestID),
			"traceparent %q does not carry the request ID as TraceID", traceparent)

		By("Verifying manager stdout exports the request root span with TraceID == request ID")
		Eventually(func(g Gomega) {
			logs := podLogs(ctx, managerNamespace, managerPodSelector, managerContainerName)
			g.Expect(logs).To(ContainSubstring(`"Name": "POST /sandboxes"`))
			g.Expect(logs).To(ContainSubstring(`"TraceID": "` + requestID + `"`))
		}, time.Minute*2, time.Second*5).Should(Succeed())

		By("Verifying controller stdout exports spans joined to the same trace")
		Eventually(func(g Gomega) {
			logs := podLogs(ctx, controllerNamespace, controllerPodSelector, controllerContainerName)
			g.Expect(logs).To(ContainSubstring(`"Name": "` + tracing.SpanControllerReconcile + `"`))
			g.Expect(logs).To(ContainSubstring(`"Name": "` + tracing.SpanControllerCreatePod + `"`))
			g.Expect(logs).To(ContainSubstring(`"TraceID": "` + requestID + `"`))
		}, time.Minute*2, time.Second*5).Should(Succeed())
	})
})

func ptrInt64(v int64) *int64 { return &v }

// podLogs returns the concatenated stdout of all pods matching selector in
// namespace, where the std tracing exporter writes its spans.
func podLogs(ctx context.Context, namespace, selector, container string) string {
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	Expect(err).NotTo(HaveOccurred())
	Expect(pods.Items).NotTo(BeEmpty(), "no pods found for selector %s in %s", selector, namespace)
	var sb strings.Builder
	for i := range pods.Items {
		raw, err := clientset.CoreV1().Pods(namespace).
			GetLogs(pods.Items[i].Name, &corev1.PodLogOptions{Container: container}).
			DoRaw(ctx)
		Expect(err).NotTo(HaveOccurred())
		sb.Write(raw)
	}
	return sb.String()
}
