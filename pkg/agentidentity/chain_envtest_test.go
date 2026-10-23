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

package agentidentity

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	corev1 "k8s.io/api/core/v1"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	securityv1alpha1 "github.com/openkruise/agents/api/security/v1alpha1"
	"github.com/openkruise/agents/pkg/identity/jwtissuer"
	"github.com/openkruise/agents/pkg/identity/oidc"
)

// chain is a running control plane: a real API server with the security CRDs
// installed, both controllers reconciling against it, and the issuer serving
// its discovery document and JWKS over TLS on a real port.
type chain struct {
	client    client.Client
	issuer    *jwtissuer.Issuer
	issuerURL string
	keyID     string
	// trusting accepts the fixture's self-signed certificate. Nothing in
	// production has one, which is why discovery needs a client injected here.
	trusting *http.Client
}

// TestIdentityChainAgainstARealAPIServer exercises everything phase 1 has
// landed so far in one run: the issuer endpoints, token issuance verified
// against nothing but the published key set, and the readiness chain from an
// AgentAuthenticationConfig through to an AgentIdentity.
//
// The readiness chain is the part that cannot be checked any other way. It
// spans two controllers, a watch that fans out, and the status subresource, and
// it is where a deadlock shipped: authenticationRefs is required with at least
// one entry, an identity stays NotReady until its config is Ready, and until
// the AgentAuthenticationConfig controller existed nothing ever made it so.
func TestIdentityChainAgainstARealAPIServer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	c := startChain(t, ctx)

	t.Run("discovery advertises the configured issuer", func(t *testing.T) {
		var document struct {
			Issuer                           string   `json:"issuer"`
			JWKSURI                          string   `json:"jwks_uri"`
			IDTokenSigningAlgValuesSupported []string `json:"id_token_signing_alg_values_supported"`
		}
		fetchJSONOver(t, c, c.issuerURL+jwtissuer.DiscoveryPath, &document)

		if document.Issuer != c.issuerURL {
			t.Errorf("iss is %q, want %q", document.Issuer, c.issuerURL)
		}
		if want := c.issuerURL + jwtissuer.JWKSPath; document.JWKSURI != want {
			t.Errorf("jwks_uri is %q, want %q", document.JWKSURI, want)
		}
		if !advertises(document.IDTokenSigningAlgValuesSupported, string(jose.RS256)) {
			t.Errorf("RS256 is not advertised, got %v", document.IDTokenSigningAlgValuesSupported)
		}
	})

	t.Run("the jwks publishes only the public half", func(t *testing.T) {
		keySet := fetchKeySet(t, c)

		if len(keySet.Keys) != 1 {
			t.Fatalf("published %d keys, want 1", len(keySet.Keys))
		}
		key := keySet.Keys[0]
		if key.KeyID != c.keyID {
			t.Errorf("kid is %q, want %q", key.KeyID, c.keyID)
		}
		if !key.IsPublic() {
			t.Error("the published key carries private material")
		}
		if _, ok := key.Key.(*rsa.PublicKey); !ok {
			t.Errorf("published key is %T, want an RSA public key", key.Key)
		}
	})

	t.Run("a minted agent token verifies against the published jwks", func(t *testing.T) {
		// Nothing local is used to verify. The key comes off the HTTPS endpoint
		// the service serves, which is the point of publishing it: a verifier
		// holding only the issuer URL can check the signature.
		keySet := fetchKeySet(t, c)

		binding := jwtissuer.AgentBinding{
			Namespace:         "default",
			AgentIdentityName: "checkout-agent",
			SandboxUID:        "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0",
		}
		raw, expiry, err := c.issuer.IssueAgentToken(binding, 10*time.Minute)
		if err != nil {
			t.Fatalf("issue agent token: %v", err)
		}

		parsed, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256})
		if err != nil {
			t.Fatalf("parse token: %v", err)
		}

		var claims struct {
			jwt.Claims
			TokenType         string `json:"token_type"`
			Namespace         string `json:"namespace"`
			AgentIdentityName string `json:"agent_identity_name"`
			SandboxUID        string `json:"sandbox_uid"`
		}
		if err := parsed.Claims(&keySet, &claims); err != nil {
			t.Fatalf("verify token against the published jwks: %v", err)
		}

		if claims.Issuer != c.issuerURL {
			t.Errorf("iss is %q, want %q", claims.Issuer, c.issuerURL)
		}
		if want := "agent:default:checkout-agent"; claims.Subject != want {
			t.Errorf("sub is %q, want %q", claims.Subject, want)
		}
		if len(claims.Audience) != 1 || claims.Audience[0] != jwtissuer.AgentTokenAudience {
			t.Errorf("aud is %v, want [%s]", claims.Audience, jwtissuer.AgentTokenAudience)
		}
		if claims.TokenType != jwtissuer.AgentTokenType {
			t.Errorf("token_type is %q, want %q", claims.TokenType, jwtissuer.AgentTokenType)
		}
		if claims.SandboxUID != binding.SandboxUID {
			t.Errorf("sandbox_uid is %q, want %q", claims.SandboxUID, binding.SandboxUID)
		}
		if got := claims.Expiry.Time(); !got.Equal(expiry.Truncate(time.Second)) {
			t.Errorf("exp is %s, want %s", got, expiry.Truncate(time.Second))
		}
	})

	t.Run("a token signed by another key is refused", func(t *testing.T) {
		keySet := fetchKeySet(t, c)

		parsed, err := jwt.ParseSigned(signWithAForeignKey(t, c.issuerURL),
			[]jose.SignatureAlgorithm{jose.RS256})
		if err != nil {
			t.Fatalf("parse forged token: %v", err)
		}
		var claims jwt.Claims
		if err := parsed.Claims(&keySet, &claims); err == nil {
			t.Fatal("a token signed by a foreign key verified against the published jwks")
		}
	})

	t.Run("the readiness chain completes", func(t *testing.T) {
		namespace := createNamespace(t, ctx, c, "chain")

		config := &securityv1alpha1.AgentAuthenticationConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "local-idp", Namespace: namespace},
			Spec: securityv1alpha1.AgentAuthenticationConfigSpec{
				Type: "jwt",
				JWT: &securityv1alpha1.JWTAuthentication{
					DiscoveryURL:    c.issuerURL + jwtissuer.DiscoveryPath,
					AllowedAudience: []string{jwtissuer.AgentTokenAudience},
				},
			},
		}
		if err := c.client.Create(ctx, config); err != nil {
			t.Fatalf("create config: %v", err)
		}

		identity := &securityv1alpha1.AgentIdentity{
			ObjectMeta: metav1.ObjectMeta{Name: "checkout-agent", Namespace: namespace},
			Spec: securityv1alpha1.AgentIdentitySpec{
				AuthenticationRefs: []securityv1alpha1.AuthenticationConfigReference{{
					APIGroup: securityv1alpha1.GroupVersion.Group,
					Kind:     "AgentAuthenticationConfig",
					Name:     "local-idp",
				}},
			},
		}
		if err := c.client.Create(ctx, identity); err != nil {
			t.Fatalf("create identity: %v", err)
		}

		// The config resolves first, off a discovery document the issuer in
		// this same process served over TLS.
		resolved := &securityv1alpha1.AgentAuthenticationConfig{}
		awaitCondition(t, ctx, c.client, client.ObjectKeyFromObject(config), resolved,
			func() []metav1.Condition { return resolved.Status.Conditions },
			func(condition *metav1.Condition) bool { return condition.Status == metav1.ConditionTrue },
			"Ready=True")

		if resolved.Status.Issuer != c.issuerURL {
			t.Errorf("status.issuer is %q, want %q", resolved.Status.Issuer, c.issuerURL)
		}
		if want := c.issuerURL + jwtissuer.JWKSPath; resolved.Status.JWKSURI != want {
			t.Errorf("status.jwksUri is %q, want %q", resolved.Status.JWKSURI, want)
		}
		if resolved.Status.ObservedGeneration != resolved.Generation {
			t.Errorf("observedGeneration is %d, want %d",
				resolved.Status.ObservedGeneration, resolved.Generation)
		}

		// Then the identity follows, which is the half that could never happen.
		ready := &securityv1alpha1.AgentIdentity{}
		awaitCondition(t, ctx, c.client, client.ObjectKeyFromObject(identity), ready,
			func() []metav1.Condition { return ready.Status.Conditions },
			func(condition *metav1.Condition) bool { return condition.Status == metav1.ConditionTrue },
			"Ready=True")

		if ready.Status.Conditions[0].Reason != reasonValidated {
			t.Errorf("reason is %q, want %q", ready.Status.Conditions[0].Reason, reasonValidated)
		}
	})

	t.Run("deleting the config takes the identity back out of Ready", func(t *testing.T) {
		// Withdrawal is the half that matters for revocation, and it depends on
		// the watch fan-out rather than on the identity being touched.
		namespace := createNamespace(t, ctx, c, "withdraw")

		config := &securityv1alpha1.AgentAuthenticationConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "local-idp", Namespace: namespace},
			Spec: securityv1alpha1.AgentAuthenticationConfigSpec{
				Type: "jwt",
				JWT: &securityv1alpha1.JWTAuthentication{
					DiscoveryURL:    c.issuerURL + jwtissuer.DiscoveryPath,
					AllowedAudience: []string{jwtissuer.AgentTokenAudience},
				},
			},
		}
		if err := c.client.Create(ctx, config); err != nil {
			t.Fatalf("create config: %v", err)
		}
		identity := &securityv1alpha1.AgentIdentity{
			ObjectMeta: metav1.ObjectMeta{Name: "checkout-agent", Namespace: namespace},
			Spec: securityv1alpha1.AgentIdentitySpec{
				AuthenticationRefs: []securityv1alpha1.AuthenticationConfigReference{{
					APIGroup: securityv1alpha1.GroupVersion.Group,
					Kind:     "AgentAuthenticationConfig",
					Name:     "local-idp",
				}},
			},
		}
		if err := c.client.Create(ctx, identity); err != nil {
			t.Fatalf("create identity: %v", err)
		}

		observed := &securityv1alpha1.AgentIdentity{}
		awaitCondition(t, ctx, c.client, client.ObjectKeyFromObject(identity), observed,
			func() []metav1.Condition { return observed.Status.Conditions },
			func(condition *metav1.Condition) bool { return condition.Status == metav1.ConditionTrue },
			"Ready=True")

		if err := c.client.Delete(ctx, config); err != nil {
			t.Fatalf("delete config: %v", err)
		}

		awaitCondition(t, ctx, c.client, client.ObjectKeyFromObject(identity), observed,
			func() []metav1.Condition { return observed.Status.Conditions },
			func(condition *metav1.Condition) bool {
				return condition.Status == metav1.ConditionFalse &&
					condition.Reason == reasonAuthenticationConfigNotFound
			},
			"Ready=False with reason "+reasonAuthenticationConfigNotFound)
	})

	t.Run("an identity whose config is missing stays not ready", func(t *testing.T) {
		namespace := createNamespace(t, ctx, c, "dangling")

		identity := &securityv1alpha1.AgentIdentity{
			ObjectMeta: metav1.ObjectMeta{Name: "orphan-agent", Namespace: namespace},
			Spec: securityv1alpha1.AgentIdentitySpec{
				AuthenticationRefs: []securityv1alpha1.AuthenticationConfigReference{{
					APIGroup: securityv1alpha1.GroupVersion.Group,
					Kind:     "AgentAuthenticationConfig",
					Name:     "does-not-exist",
				}},
			},
		}
		if err := c.client.Create(ctx, identity); err != nil {
			t.Fatalf("create identity: %v", err)
		}

		observed := &securityv1alpha1.AgentIdentity{}
		awaitCondition(t, ctx, c.client, client.ObjectKeyFromObject(identity), observed,
			func() []metav1.Condition { return observed.Status.Conditions },
			func(condition *metav1.Condition) bool {
				return condition.Status == metav1.ConditionFalse &&
					condition.Reason == reasonAuthenticationConfigNotFound
			},
			"Ready=False with reason "+reasonAuthenticationConfigNotFound)
	})

	t.Run("an unreachable issuer is reported and leaves no stale issuer", func(t *testing.T) {
		// A port nothing listens on stands in for an outage, and is also what a
		// typo in discoveryUrl looks like.
		namespace := createNamespace(t, ctx, c, "unreachable")

		config := &securityv1alpha1.AgentAuthenticationConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "broken-idp", Namespace: namespace},
			Spec: securityv1alpha1.AgentAuthenticationConfigSpec{
				Type: "jwt",
				JWT: &securityv1alpha1.JWTAuthentication{
					DiscoveryURL:    "https://127.0.0.1:1" + jwtissuer.DiscoveryPath,
					AllowedAudience: []string{jwtissuer.AgentTokenAudience},
				},
			},
		}
		if err := c.client.Create(ctx, config); err != nil {
			t.Fatalf("create config: %v", err)
		}

		observed := &securityv1alpha1.AgentAuthenticationConfig{}
		awaitCondition(t, ctx, c.client, client.ObjectKeyFromObject(config), observed,
			func() []metav1.Condition { return observed.Status.Conditions },
			func(condition *metav1.Condition) bool {
				return condition.Status == metav1.ConditionFalse &&
					condition.Reason == reasonDiscoveryFailed
			},
			"Ready=False with reason "+reasonDiscoveryFailed)

		if observed.Status.Issuer != "" {
			t.Errorf("status.issuer is %q on a failed config, want it cleared", observed.Status.Issuer)
		}
	})

	t.Run("resolution is not repeated for an unchanged generation", func(t *testing.T) {
		// The resolve-once decision is only observable by counting outbound
		// requests, so this is the one case the review discussion could not
		// settle from the code alone.
		namespace := createNamespace(t, ctx, c, "resolveonce")

		before := discoveryRequests.Load()
		config := &securityv1alpha1.AgentAuthenticationConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "local-idp", Namespace: namespace},
			Spec: securityv1alpha1.AgentAuthenticationConfigSpec{
				Type: "jwt",
				JWT: &securityv1alpha1.JWTAuthentication{
					DiscoveryURL:    c.issuerURL + jwtissuer.DiscoveryPath,
					AllowedAudience: []string{jwtissuer.AgentTokenAudience},
				},
			},
		}
		if err := c.client.Create(ctx, config); err != nil {
			t.Fatalf("create config: %v", err)
		}

		observed := &securityv1alpha1.AgentAuthenticationConfig{}
		awaitCondition(t, ctx, c.client, client.ObjectKeyFromObject(config), observed,
			func() []metav1.Condition { return observed.Status.Conditions },
			func(condition *metav1.Condition) bool { return condition.Status == metav1.ConditionTrue },
			"Ready=True")

		resolvedAt := discoveryRequests.Load()
		if resolvedAt == before {
			t.Fatal("the config reported Ready without any discovery request being made")
		}

		// A status-only write does not bump the generation, so the next
		// reconcile it triggers must not re-fetch.
		observed.Status.Conditions[0].Message += " (touched)"
		if err := c.client.Status().Update(ctx, observed); err != nil {
			t.Fatalf("touch status: %v", err)
		}
		time.Sleep(2 * time.Second)

		if after := discoveryRequests.Load(); after != resolvedAt {
			t.Errorf("discovery ran %d more times for an unchanged generation, want 0",
				after-resolvedAt)
		}

		// An edit does bump it, and must be re-resolved.
		if err := c.client.Get(ctx, client.ObjectKeyFromObject(config), observed); err != nil {
			t.Fatalf("get config: %v", err)
		}
		observed.Spec.JWT.AllowedAudience = append(observed.Spec.JWT.AllowedAudience, "second-audience")
		if err := c.client.Update(ctx, observed); err != nil {
			t.Fatalf("edit config: %v", err)
		}

		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if discoveryRequests.Load() > resolvedAt {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Error("an edited config was never re-resolved")
	})
}

// startChain boots the API server, installs the CRDs, and starts a manager
// running the issuer and both controllers against it.
func startChain(t *testing.T, ctx context.Context) *chain {
	t.Helper()

	ctrl.SetLogger(klog.Background())

	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set; run under make test or point it at envtest binaries")
	}

	scheme := apiruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("register the core scheme: %v", err)
	}
	if err := securityv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("register the security scheme: %v", err)
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		Scheme:                scheme,
	}
	restConfig, err := env.Start()
	if err != nil {
		t.Fatalf("start the API server: %v", err)
	}
	t.Cleanup(func() { stopEnvironment(t, env) })

	dir := t.TempDir()
	signingKeyFile, keyID := writeLocalSigningKey(t, dir)
	certFile, keyFile, certPEM := writeLocalServingCertificate(t, dir)

	// The issuer URL has to match the address callers reach, because the served
	// document advertises it and discovery compares the two, so the port is
	// chosen before the manager starts.
	port := freePort(t)
	issuerURL := fmt.Sprintf("https://127.0.0.1:%d", port)

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:  scheme,
		Metrics: ctrlmetrics.Options{BindAddress: "0"},
		// Controller names are registered process-wide for metrics, and other
		// tests in this package register the same ones against their own
		// managers, so the check has to be relaxed here.
		Controller: ctrlconfig.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		t.Fatalf("build manager: %v", err)
	}

	issuer, err := AddIssuer(mgr, IssuerConfig{
		IssuerURL:      issuerURL,
		SigningKeyFile: signingKeyFile,
		TLSCertFile:    certFile,
		TLSKeyFile:     keyFile,
		BindAddress:    fmt.Sprintf("127.0.0.1:%d", port),
	})
	if err != nil {
		t.Fatalf("add the issuer: %v", err)
	}
	if issuer == nil {
		t.Fatal("AddIssuer returned no issuer even though a signing key was configured")
	}

	if err := (&Reconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		t.Fatalf("add the AgentIdentity controller: %v", err)
	}

	// The production wiring resolves issuers over the system roots, which
	// cannot reach a self-signed test server, so discovery is given a client
	// that trusts this fixture. The validation inside discovery is covered
	// against a real served document in the oidc package; what is under test
	// here is the controller chain around it.
	trusting := trustingClient(certPEM)
	if err := (&AuthConfigReconciler{
		Client: mgr.GetClient(),
		discover: func(ctx context.Context, discoveryURL string,
			opts oidc.Options) (oidc.Discovered, error) {
			discoveryRequests.Add(1)
			return discoverOver(ctx, trusting, discoveryURL)
		},
	}).SetupWithManager(mgr); err != nil {
		t.Fatalf("add the AgentAuthenticationConfig controller: %v", err)
	}

	// The manager gets its own cancel so teardown can stop it and then wait.
	// t.Cleanup is LIFO, so a cleanup registered here runs before the caller's
	// cancel: waiting without cancelling first would simply time out.
	managerCtx, stopManager := context.WithCancel(ctx)
	exited := make(chan error, 1)
	go func() { exited <- mgr.Start(managerCtx) }()
	t.Cleanup(func() {
		stopManager()
		select {
		case err := <-exited:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("the manager exited with %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("the manager did not shut down")
		}
	})

	if !mgr.GetCache().WaitForCacheSync(managerCtx) {
		t.Fatal("caches did not sync")
	}

	c, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("build a client: %v", err)
	}

	fixture := &chain{
		client: c, issuer: issuer, issuerURL: issuerURL, keyID: keyID, trusting: trusting,
	}
	waitForEndpoint(t, fixture, issuerURL+jwtissuer.DiscoveryPath)
	return fixture
}

// discoveryRequests counts outbound discovery attempts, so a test can assert
// that an unchanged generation makes none.
var discoveryRequests atomic.Int64

// discoverOver fetches and minimally interprets a discovery document using a
// caller-supplied client.
func discoverOver(ctx context.Context, httpClient *http.Client, discoveryURL string) (oidc.Discovered, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return oidc.Discovered{}, fmt.Errorf("build discovery request: %w", err)
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return oidc.Discovered{}, fmt.Errorf("fetch discovery document: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return oidc.Discovered{}, fmt.Errorf("fetch discovery document: unexpected status %s", response.Status)
	}
	var document struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		return oidc.Discovered{}, fmt.Errorf("decode discovery document: %w", err)
	}
	if document.Issuer == "" {
		return oidc.Discovered{}, errors.New("discovery document has an empty issuer")
	}
	if !strings.HasPrefix(discoveryURL, document.Issuer) {
		return oidc.Discovered{}, errors.New("discovery document issuer does not match the discovery URL")
	}
	return oidc.Discovered{Issuer: document.Issuer, JWKSURI: document.JWKSURI}, nil
}

// stopEnvironment tears down the API server. envtest signals SIGTERM, which
// Windows does not have, so there the processes are killed outright instead of
// being left behind.
func stopEnvironment(t *testing.T, env *envtest.Environment) {
	t.Helper()
	err := env.Stop()
	if runtime.GOOS != "windows" {
		if err != nil {
			t.Errorf("stop the API server: %v", err)
		}
		return
	}
	for _, image := range []string{"kube-apiserver.exe", "etcd.exe"} {
		_ = exec.Command("taskkill", "/F", "/IM", image).Run()
	}
}

func createNamespace(t *testing.T, ctx context.Context, c *chain, prefix string) string {
	t.Helper()
	namespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{GenerateName: prefix + "-"},
	}
	if err := c.client.Create(ctx, namespace); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	return namespace.Name
}

func awaitCondition(t *testing.T, ctx context.Context, c client.Client, key types.NamespacedName,
	into client.Object, conditions func() []metav1.Condition,
	satisfied func(*metav1.Condition) bool, want string) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	var last *metav1.Condition
	for time.Now().Before(deadline) {
		if err := c.Get(ctx, key, into); err != nil {
			t.Fatalf("get %s: %v", key, err)
		}
		if condition := apiMeta.FindStatusCondition(conditions(), securityv1alpha1.ConditionReady); condition != nil {
			last = condition
			if satisfied(condition) {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if last == nil {
		t.Fatalf("%s never reported a Ready condition, want %s", key, want)
	}
	t.Fatalf("%s settled on Ready=%s reason=%s message=%q, want %s",
		key, last.Status, last.Reason, last.Message, want)
}

func writeLocalSigningKey(t *testing.T, dir string) (path, keyID string) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate a signing key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal the signing key: %v", err)
	}
	path = filepath.Join(dir, "signing.key")
	if err := os.WriteFile(path,
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write the signing key: %v", err)
	}

	loaded, err := jwtissuer.LoadSigningKeyFromFile(path)
	if err != nil {
		t.Fatalf("the generated key was not loadable: %v", err)
	}
	return path, loaded.KeyID
}

func writeLocalServingCertificate(t *testing.T, dir string) (certFile, keyFile string, certPEM []byte) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate a serving key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "agent-identity-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("self-sign the serving certificate: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal the serving key: %v", err)
	}

	certFile = filepath.Join(dir, "tls.crt")
	keyFile = filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatalf("write the certificate: %v", err)
	}
	if err := os.WriteFile(keyFile,
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write the serving key: %v", err)
	}
	return certFile, keyFile, certPEM
}

func trustingClient(certPEM []byte) *http.Client {
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certPEM)
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		},
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}
	return port
}

func waitForEndpoint(t *testing.T, c *chain, address string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		response, err := c.trusting.Get(address)
		if err == nil {
			_ = response.Body.Close()
			return
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never answered: %v", address, lastErr)
}

func fetchKeySet(t *testing.T, c *chain) jose.JSONWebKeySet {
	t.Helper()
	var keySet jose.JSONWebKeySet
	fetchJSONOver(t, c, c.issuerURL+jwtissuer.JWKSPath, &keySet)
	return keySet
}

func fetchJSONOver(t *testing.T, c *chain, address string, into any) {
	t.Helper()
	response, err := c.trusting.Get(address)
	if err != nil {
		t.Fatalf("GET %s: %v", address, err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s returned %s", address, response.Status)
	}
	if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Errorf("GET %s returned Content-Type %q, want JSON", address, contentType)
	}
	if err := json.NewDecoder(response.Body).Decode(into); err != nil {
		t.Fatalf("decode %s: %v", address, err)
	}
}

func signWithAForeignKey(t *testing.T, issuerURL string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate a foreign key: %v", err)
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatalf("build a foreign signer: %v", err)
	}
	raw, err := jwt.Signed(signer).Claims(jwt.Claims{
		Issuer:   issuerURL,
		Subject:  "agent:default:checkout-agent",
		Audience: jwt.Audience{jwtissuer.AgentTokenAudience},
		Expiry:   jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}).Serialize()
	if err != nil {
		t.Fatalf("sign with the foreign key: %v", err)
	}
	return raw
}

func advertises(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
