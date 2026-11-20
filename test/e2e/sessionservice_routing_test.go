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
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agents "github.com/openkruise/agents/api/v1alpha1"
	sessionbinding "github.com/openkruise/agents/pkg/sessionservice/binding"
)

// Opt in with SESSION_SERVICE_GATEWAY_URL (the reachable Gateway origin) and
// SESSION_SERVICE_DOMAIN (the base domain suffix). Requests retain the routing
// hostname for Host, TLS verification and the browser cookie jar, while dialing
// the configured origin. No local DNS or hosts-file changes are required.
// SESSION_SERVICE_ROUTING_KEEP_RESOURCES=true retains the test namespaces;
// SESSION_SERVICE_ROUTING_RUN_ID labels them for subsequent inspection.
var _ = Describe("SessionService Gateway routing", Label("sessionservice-routing"), func() {
	const sessionHeader = "x-kruise-agent-session-id"
	ctx := context.Background()
	var service *agents.SessionService
	var browser, headerClient *http.Client
	var requestURL, routingScheme, routingDomain string

	BeforeEach(func() {
		origin, domain := os.Getenv("SESSION_SERVICE_GATEWAY_URL"), os.Getenv("SESSION_SERVICE_DOMAIN")
		if origin == "" || domain == "" {
			Skip("set SESSION_SERVICE_GATEWAY_URL and SESSION_SERVICE_DOMAIN to run Gateway routing E2E")
		}
		endpoint, err := url.Parse(origin)
		Expect(err).NotTo(HaveOccurred())
		Expect(endpoint.Scheme).To(BeElementOf("http", "https"))
		Expect(endpoint.Hostname()).NotTo(BeEmpty())
		Expect(endpoint.Path).To(BeElementOf("", "/"))
		Expect(endpoint.User).To(BeNil())
		Expect(endpoint.RawQuery).To(BeEmpty())
		Expect(endpoint.Fragment).To(BeEmpty())

		labels := map[string]string{"test.agents.kruise.io/run-id": "routing-e2e"}
		if runID := os.Getenv("SESSION_SERVICE_ROUTING_RUN_ID"); runID != "" {
			labels["test.agents.kruise.io/run-id"] = runID
		}
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "sessionservice-routing-", Labels: labels}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		namespace := ns.Name
		DeferCleanup(func() {
			if os.Getenv("SESSION_SERVICE_ROUTING_KEEP_RESOURCES") == "true" {
				GinkgoWriter.Printf("Retained routing E2E namespace: %s\n", namespace)
				return
			}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))).To(Succeed())
		})
		image := os.Getenv("SESSION_SERVICE_ROUTING_IMAGE")
		if image == "" {
			image = "nginx:stable-alpine3.23"
		}
		service = &agents.SessionService{
			ObjectMeta: metav1.ObjectMeta{Name: "routing", Namespace: namespace, Labels: labels},
			Spec: agents.SessionServiceSpec{
				Replicas: ptr.To[int32](2), Ports: []int32{8080},
				SessionPolicy: agents.SessionServiceSessionPolicy{Affinity: "BestEffort"},
				SandboxTemplateSpec: agents.SandboxTemplateSpec{Template: &corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
					AutomountServiceAccountToken: ptr.To(false),
					Containers: []corev1.Container{{
						Name: "main", Image: image,
						Env: []corev1.EnvVar{{Name: "POD_UID", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}}}},
						Command: []string{"sh", "-ec", `
printf 'server { listen 8080; location / { if (!-f /tmp/routing-ready) { return 503; } return 200 "%s\\n"; } }\n' "$POD_UID" > /etc/nginx/conf.d/default.conf
touch /tmp/routing-ready
exec nginx -g 'daemon off;'
`},
						ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"test", "-f", "/tmp/routing-ready"}}}, PeriodSeconds: 1, FailureThreshold: 1},
					}},
				}}},
			},
		}
		address := endpoint.Host
		if endpoint.Port() == "" {
			port := "80"
			if endpoint.Scheme == "https" {
				port = "443"
			}
			address = net.JoinHostPort(endpoint.Hostname(), port)
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", address)
		}
		DeferCleanup(transport.CloseIdleConnections)
		jar, err := cookiejar.New(nil)
		Expect(err).NotTo(HaveOccurred())
		browser = &http.Client{Transport: transport, Jar: jar, Timeout: 12 * time.Second}
		headerClient = &http.Client{Transport: transport, Timeout: 12 * time.Second}
		// Redirects must not turn one routing assertion into another HTTP request.
		browser.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		headerClient.CheckRedirect = browser.CheckRedirect
		routingScheme, routingDomain = endpoint.Scheme, strings.TrimSuffix(domain, ".")
	})

	members := func() []agents.Sandbox {
		var list agents.SandboxList
		Expect(k8sClient.List(ctx, &list, client.InNamespace(service.Namespace), client.MatchingLabels{sessionbinding.SessionServiceUIDLabel: string(service.UID)})).To(Succeed())
		return list.Items
	}
	start := func(affinity string) {
		service.Spec.SessionPolicy.Affinity = affinity
		Expect(k8sClient.Create(ctx, service.DeepCopy(), client.DryRunAll)).To(Succeed())
		Expect(k8sClient.Create(ctx, service)).To(Succeed())
		Expect(service.UID).NotTo(BeEmpty())
		requestURL = fmt.Sprintf("%s://svc-8080-%s.%s/", routingScheme, service.UID, routingDomain)
		Eventually(func(g Gomega) {
			var list agents.SandboxList
			g.Expect(k8sClient.List(ctx, &list, client.InNamespace(service.Namespace), client.MatchingLabels{sessionbinding.SessionServiceUIDLabel: string(service.UID)})).To(Succeed())
			g.Expect(list.Items).To(HaveLen(2))
			for _, box := range list.Items {
				g.Expect(box.DeletionTimestamp).To(BeNil())
				g.Expect(meta.IsStatusConditionTrue(box.Status.Conditions, string(agents.SandboxConditionReady))).To(BeTrue())
			}
		}, 5*time.Minute, time.Second).Should(Succeed())
	}
	request := func(c *http.Client, sessionID string) (*http.Response, string) {
		requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, requestURL, nil)
		Expect(err).NotTo(HaveOccurred())
		if sessionID != "" {
			req.Header.Set(sessionHeader, sessionID)
		}
		response, err := c.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { Expect(response.Body.Close()).To(Succeed()) }()
		body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
		Expect(err).NotTo(HaveOccurred())
		return response, strings.TrimSpace(string(body))
	}
	allocate := func(c *http.Client) (*http.Response, string) {
		// Only initial admission tolerates Gateway informer startup delay. The
		// migration request below is issued exactly once, without this retry loop.
		var response *http.Response
		var body string
		Eventually(func() int {
			response, body = request(c, "")
			return response.StatusCode
		}, time.Minute, time.Second).Should(Equal(http.StatusOK))
		Expect(sessionbinding.ValidSessionID(response.Header.Get(sessionHeader))).To(BeTrue())
		return response, body
	}
	boundPod := func(sessionID, responseUID string) (agents.Sandbox, corev1.Pod) {
		var bound []agents.Sandbox
		for _, box := range members() {
			records, err := sessionbinding.Decode(box.Annotations)
			Expect(err).NotTo(HaveOccurred())
			if record, ok := records[sessionID]; ok {
				Expect(record.Phase).To(BeEmpty())
				bound = append(bound, box)
			}
		}
		Expect(bound).To(HaveLen(1))
		var pods corev1.PodList
		Expect(k8sClient.List(ctx, &pods, client.InNamespace(service.Namespace))).To(Succeed())
		for _, pod := range pods.Items {
			if string(pod.UID) == responseUID {
				Expect(metav1.IsControlledBy(&pod, &bound[0])).To(BeTrue())
				return bound[0], pod
			}
		}
		Fail("response did not identify the Pod owned by the bound Sandbox")
		return agents.Sandbox{}, corev1.Pod{}
	}
	setReady := func(pod corev1.Pod, box agents.Sandbox, ready bool) {
		command := []string{"rm", "-f", "/tmp/routing-ready"}
		if ready {
			command = []string{"touch", "/tmp/routing-ready"}
		}
		execCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		args := append([]string{"exec", "-n", pod.Namespace, pod.Name, "-c", "main", "--"}, command...)
		output, err := exec.CommandContext(execCtx, "kubectl", args...).CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "kubectl exec: %s", output)
		condition := func(g Gomega) {
			var current agents.Sandbox
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(&box), &current)).To(Succeed())
			g.Expect(meta.IsStatusConditionTrue(current.Status.Conditions, string(agents.SandboxConditionReady))).To(Equal(ready))
		}
		Eventually(condition, time.Minute, time.Second).Should(Succeed())
		// Give Gateway informers time to observe the real readiness transition.
		// A stale route still fails the test: the old backend now returns 503.
		Consistently(condition, 2*time.Second, 200*time.Millisecond).Should(Succeed())
	}

	It("keeps affinity through response headers and browser cookies", func() {
		start("BestEffort")
		initial, uid := allocate(browser)
		sid := initial.Header.Get(sessionHeader)
		boundPod(sid, uid)
		cookies := initial.Cookies()
		Expect(cookies).To(ContainElement(SatisfyAll(
			HaveField("Name", "kruise-agent-session-id"), HaveField("Value", sid),
			HaveField("Domain", ""), HaveField("Path", "/"), HaveField("HttpOnly", true),
			HaveField("SameSite", http.SameSiteLaxMode),
		)))
		for _, c := range []*http.Client{headerClient, browser} {
			id := sid
			if c == browser {
				id = "" // The jar sends the cookie without a custom Session header.
			}
			response, body := request(c, id)
			Expect(response.StatusCode).To(Equal(http.StatusOK))
			Expect(response.Header.Get(sessionHeader)).To(Equal(sid))
			Expect(body).To(Equal(uid))
		}
		other, otherUID := allocate(headerClient)
		otherSID := other.Header.Get(sessionHeader)
		Expect(otherSID).NotTo(Equal(sid))
		Expect(otherUID).NotTo(Equal(uid))
		response, body := request(browser, otherSID)
		Expect(response.StatusCode).To(Equal(http.StatusOK))
		Expect(response.Header.Get(sessionHeader)).To(Equal(otherSID))
		Expect(body).To(Equal(otherUID), "explicit header must override the original browser cookie")
		boundPod(otherSID, body)
	})

	It("returns promptly without creating a binding for an unknown session ID", func() {
		start("BestEffort")
		allocate(headerClient) // Ensure the Gateway has observed the service.
		const missingSessionID = "unknown-session-id-001"
		Expect(sessionbinding.ValidSessionID(missingSessionID)).To(BeTrue())
		for _, box := range members() {
			records, err := sessionbinding.Decode(box.Annotations)
			Expect(err).NotTo(HaveOccurred())
			Expect(records).NotTo(HaveKey(missingSessionID))
		}

		started := time.Now()
		response, body := request(headerClient, missingSessionID)
		elapsed := time.Since(started)
		Expect(response.StatusCode).To(Equal(http.StatusNotFound))
		Expect(body).To(Equal("SessionNotFound"))
		Expect(elapsed).To(BeNumerically("<", 2*time.Second), "unknown sessions must not wait for the migration budget")
		Expect(response.Header.Get(sessionHeader)).To(BeEmpty())
		Expect(response.Cookies()).NotTo(ContainElement(HaveField("Name", "kruise-agent-session-id")))
		for _, box := range members() {
			records, err := sessionbinding.Decode(box.Annotations)
			Expect(err).NotTo(HaveOccurred())
			Expect(records).NotTo(HaveKey(missingSessionID))
		}
	})

	It("completes BestEffort migration in the same HTTP request", func() {
		start("BestEffort")
		initial, uid := allocate(headerClient)
		sid := initial.Header.Get(sessionHeader)
		old, pod := boundPod(sid, uid)
		before, err := sessionbinding.Decode(old.Annotations)
		Expect(err).NotTo(HaveOccurred())
		setReady(pod, old, false)
		response, newUID := request(headerClient, sid)
		Expect(response.StatusCode).To(Equal(http.StatusOK), "the single migration request must succeed without client retry")
		Expect(response.Header.Get(sessionHeader)).To(Equal(sid))
		Expect(newUID).NotTo(Equal(uid))
		current, _ := boundPod(sid, newUID)
		Expect(current.UID).NotTo(Equal(old.UID))
		after, err := sessionbinding.Decode(current.Annotations)
		Expect(err).NotTo(HaveOccurred())
		Expect(after[sid].CreatedAt).To(Equal(before[sid].CreatedAt))
	})

	It("keeps Strict bindings while unavailable and reuses them after recovery", func() {
		start("Strict")
		initial, uid := allocate(headerClient)
		sid := initial.Header.Get(sessionHeader)
		old, pod := boundPod(sid, uid)
		setReady(pod, old, false)
		response, body := request(headerClient, sid)
		Expect(response.StatusCode).To(Equal(http.StatusServiceUnavailable))
		Expect(body).To(Equal("BoundSandboxUnavailable"))
		Consistently(func() string {
			current, _ := boundPod(sid, uid)
			return string(current.UID)
		}, 3*time.Second, 200*time.Millisecond).Should(Equal(string(old.UID)))
		setReady(pod, old, true)
		response, body = request(headerClient, sid)
		Expect(response.StatusCode).To(Equal(http.StatusOK))
		Expect(response.Header.Get(sessionHeader)).To(Equal(sid))
		Expect(body).To(Equal(uid))
		boundPod(sid, body)
	})
})
