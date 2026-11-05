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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	securityv1alpha1 "github.com/openkruise/agents/api/security/v1alpha1"
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	identitypkg "github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/identity/agenttoken"
	"github.com/openkruise/agents/pkg/identity/jwtissuer"
)

// issuanceTestScheme adds the Sandbox type the handler re-reads to the shared
// test scheme.
var issuanceTestScheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(securityv1alpha1.AddToScheme(s))
	utilruntime.Must(agentsv1alpha1.AddToScheme(s))
	return s
}()

const (
	issuanceNamespace = "team-a"
	issuanceSandbox   = "sbx-1"
	issuanceUID       = "uid-1"
	issuanceAgent     = "agent-a"
)

// newIssuanceIssuer builds an issuer over a freshly generated key and returns
// the public half for verifying what the handler minted.
func newIssuanceIssuer(t *testing.T) (*jwtissuer.Issuer, *ecdsa.PublicKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	issuer, err := jwtissuer.New("https://issuer.test", jwtissuer.SigningKey{KeyID: "test-key", PrivateKey: key})
	if err != nil {
		t.Fatalf("build issuer: %v", err)
	}
	return issuer, &key.PublicKey
}

// selectedSandbox is a Sandbox that selects the named AgentIdentity.
func selectedSandbox(agentName string) *agentsv1alpha1.Sandbox {
	annotations := map[string]string{}
	if agentName != "" {
		annotations[identitypkg.AnnotationAgentName] = agentName
	}
	return &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{
		Namespace:   issuanceNamespace,
		Name:        issuanceSandbox,
		UID:         types.UID(issuanceUID),
		Annotations: annotations,
	}}
}

// readyAgentIdentity is an AgentIdentity whose Ready condition describes its
// current generation.
func readyAgentIdentity(name string) *securityv1alpha1.AgentIdentity {
	agentIdentity := &securityv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Namespace: issuanceNamespace, Name: name, Generation: 1},
	}
	agentIdentity.Status.ObservedGeneration = agentIdentity.Generation
	agentIdentity.Status.Conditions = []metav1.Condition{{
		Type:               securityv1alpha1.ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             "Ready",
		ObservedGeneration: agentIdentity.Generation,
		LastTransitionTime: metav1.Now(),
	}}
	return agentIdentity
}

func issueRequestBody(t *testing.T, req agenttoken.Request) string {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return string(body)
}

func validIssueRequest(t *testing.T) string {
	t.Helper()
	return issueRequestBody(t, agenttoken.Request{
		Namespace:   issuanceNamespace,
		SandboxName: issuanceSandbox,
		SandboxUID:  issuanceUID,
	})
}

// serve runs one request against the handler. withCertificate simulates the
// listener having verified a client certificate, which is the only thing standin
// for the route-scoped mTLS in a unit test.
func serveIssueRequest(t *testing.T, handler http.Handler, method, body string, withCertificate bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, agenttoken.Path, strings.NewReader(body))
	if withCertificate {
		req.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{}}}
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func decodeIssueError(t *testing.T, recorder *httptest.ResponseRecorder) agenttoken.Error {
	t.Helper()
	var body agenttoken.Error
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", recorder.Body.String(), err)
	}
	return body
}

func TestIssueAgentToken(t *testing.T) {
	stale := readyAgentIdentity(issuanceAgent)
	stale.Generation = 2 // the Ready verdict still describes generation 1

	notReady := readyAgentIdentity(issuanceAgent)
	notReady.Status.Conditions[0].Status = metav1.ConditionFalse

	deleting := readyAgentIdentity(issuanceAgent)
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	// The fake client refuses a deletionTimestamp without a finalizer, since the
	// real API server would have removed the object already.
	deleting.Finalizers = []string{"agents.kruise.io/test"}

	tests := []struct {
		name            string
		objects         []client.Object
		method          string
		body            string
		withCertificate bool
		wantStatus      int
		wantCode        string
		wantAgent       string
	}{
		{
			name: "issues a token bound to the re-read sandbox",
			objects: []client.Object{
				selectedSandbox(issuanceAgent),
				readyAgentIdentity(issuanceAgent),
			},
			wantStatus:      http.StatusOK,
			withCertificate: true,
			wantAgent:       issuanceAgent,
		},
		{
			name:       "refuses a request without a verified client certificate",
			objects:    []client.Object{selectedSandbox(issuanceAgent), readyAgentIdentity(issuanceAgent)},
			wantStatus: http.StatusUnauthorized,
			wantCode:   agenttoken.CodeUnauthenticated,
		},
		{
			name: "refuses a method other than POST",
			objects: []client.Object{
				selectedSandbox(issuanceAgent), readyAgentIdentity(issuanceAgent),
			},
			method:          http.MethodGet,
			withCertificate: true,
			wantStatus:      http.StatusMethodNotAllowed,
			wantCode:        agenttoken.CodeMethodNotAllowed,
		},
		{
			name:            "refuses malformed JSON",
			objects:         []client.Object{selectedSandbox(issuanceAgent), readyAgentIdentity(issuanceAgent)},
			body:            "{",
			withCertificate: true,
			wantStatus:      http.StatusBadRequest,
			wantCode:        agenttoken.CodeInvalidRequest,
		},
		{
			name:            "refuses unknown fields",
			objects:         []client.Object{selectedSandbox(issuanceAgent), readyAgentIdentity(issuanceAgent)},
			body:            `{"namespace":"team-a","sandboxName":"sbx-1","sandboxUid":"uid-1","agentIdentityName":"other"}`,
			withCertificate: true,
			wantStatus:      http.StatusBadRequest,
			wantCode:        agenttoken.CodeInvalidRequest,
		},
		{
			name:            "requires every identifying field",
			objects:         []client.Object{selectedSandbox(issuanceAgent), readyAgentIdentity(issuanceAgent)},
			body:            `{"namespace":"team-a","sandboxName":"sbx-1"}`,
			withCertificate: true,
			wantStatus:      http.StatusBadRequest,
			wantCode:        agenttoken.CodeInvalidRequest,
		},
		{
			name:            "reports a missing sandbox",
			objects:         []client.Object{readyAgentIdentity(issuanceAgent)},
			withCertificate: true,
			wantStatus:      http.StatusNotFound,
			wantCode:        agenttoken.CodeSandboxNotFound,
		},
		{
			name:            "refuses a UID that does not match the sandbox",
			objects:         []client.Object{selectedSandbox(issuanceAgent), readyAgentIdentity(issuanceAgent)},
			body:            issueRequestBody(t, agenttoken.Request{Namespace: issuanceNamespace, SandboxName: issuanceSandbox, SandboxUID: "stale-uid"}),
			withCertificate: true,
			wantStatus:      http.StatusConflict,
			wantCode:        agenttoken.CodeSandboxUIDMismatch,
		},
		{
			name:            "refuses a sandbox that selects no identity",
			objects:         []client.Object{selectedSandbox(""), readyAgentIdentity(issuanceAgent)},
			withCertificate: true,
			wantStatus:      http.StatusPreconditionFailed,
			wantCode:        agenttoken.CodeAgentIdentityNotSelected,
		},
		{
			name:            "refuses a missing identity",
			objects:         []client.Object{selectedSandbox(issuanceAgent)},
			withCertificate: true,
			wantStatus:      http.StatusPreconditionFailed,
			wantCode:        agenttoken.CodeAgentIdentityNotFound,
		},
		{
			name:            "refuses an identity with no Ready condition",
			objects:         []client.Object{selectedSandbox(issuanceAgent), &securityv1alpha1.AgentIdentity{ObjectMeta: metav1.ObjectMeta{Namespace: issuanceNamespace, Name: issuanceAgent}}},
			withCertificate: true,
			wantStatus:      http.StatusPreconditionFailed,
			wantCode:        agenttoken.CodeAgentIdentityNotReady,
		},
		{
			name:            "refuses an identity whose verdict is stale",
			objects:         []client.Object{selectedSandbox(issuanceAgent), stale},
			withCertificate: true,
			wantStatus:      http.StatusPreconditionFailed,
			wantCode:        agenttoken.CodeAgentIdentityNotReady,
		},
		{
			name:            "refuses an identity that reports not ready",
			objects:         []client.Object{selectedSandbox(issuanceAgent), notReady},
			withCertificate: true,
			wantStatus:      http.StatusPreconditionFailed,
			wantCode:        agenttoken.CodeAgentIdentityNotReady,
		},
		{
			name:            "refuses an identity being deleted",
			objects:         []client.Object{selectedSandbox(issuanceAgent), deleting},
			withCertificate: true,
			wantStatus:      http.StatusPreconditionFailed,
			wantCode:        agenttoken.CodeAgentIdentityNotReady,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issuer, publicKey := newIssuanceIssuer(t)
			reader := fake.NewClientBuilder().WithScheme(issuanceTestScheme).WithObjects(tt.objects...).Build()
			handler := newAgentTokenHandler(issuer, reader)

			method := tt.method
			if method == "" {
				method = http.MethodPost
			}
			body := tt.body
			if body == "" {
				body = validIssueRequest(t)
			}
			recorder := serveIssueRequest(t, handler, method, body, tt.withCertificate)

			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
			if tt.wantStatus != http.StatusOK {
				if code := decodeIssueError(t, recorder).Code; code != tt.wantCode {
					t.Fatalf("error code = %q, want %q", code, tt.wantCode)
				}
				return
			}

			var resp agenttoken.Response
			if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if resp.RequestID == "" {
				t.Fatal("response carries no request ID")
			}
			expiresAt, err := time.Parse(time.RFC3339, resp.ExpiresAt)
			if err != nil {
				t.Fatalf("expiresAt %q is not RFC3339: %v", resp.ExpiresAt, err)
			}
			if !expiresAt.After(time.Now()) {
				t.Fatalf("expiresAt %s is not in the future", resp.ExpiresAt)
			}

			// The token must describe the Sandbox and identity the handler read,
			// not anything the request could have named.
			parsed, err := josejwt.ParseSigned(resp.AgentToken, []jose.SignatureAlgorithm{jose.ES256})
			if err != nil {
				t.Fatalf("parse token: %v", err)
			}
			var claims struct {
				josejwt.Claims
				TokenType         string `json:"token_type"`
				Namespace         string `json:"namespace"`
				AgentIdentityName string `json:"agent_identity_name"`
				SandboxUID        string `json:"sandbox_uid"`
			}
			if err := parsed.Claims(publicKey, &claims); err != nil {
				t.Fatalf("verify token claims: %v", err)
			}
			if claims.TokenType != jwtissuer.AgentTokenType {
				t.Errorf("token_type = %q, want %q", claims.TokenType, jwtissuer.AgentTokenType)
			}
			if !claims.Audience.Contains(jwtissuer.AgentTokenAudience) || len(claims.Audience) != 1 {
				t.Errorf("aud = %v, want [%s]", claims.Audience, jwtissuer.AgentTokenAudience)
			}
			if claims.Namespace != issuanceNamespace || claims.AgentIdentityName != tt.wantAgent ||
				claims.SandboxUID != issuanceUID {
				t.Errorf("claims = %+v, want sandbox identity %s/%s@%s",
					claims, issuanceNamespace, tt.wantAgent, issuanceUID)
			}
			if wantSubject := "agent:" + issuanceNamespace + ":" + tt.wantAgent; claims.Subject != wantSubject {
				t.Errorf("sub = %q, want %q", claims.Subject, wantSubject)
			}
		})
	}
}

// TestIssueAgentTokenRejectsTrailingContent covers the decoder's refusal of a
// second JSON value after the first, which a client could otherwise use to have
// the server consider one body and a proxy another.
func TestIssueAgentTokenRejectsTrailingContent(t *testing.T) {
	issuer, _ := newIssuanceIssuer(t)
	reader := fake.NewClientBuilder().WithScheme(issuanceTestScheme).
		WithObjects(selectedSandbox(issuanceAgent), readyAgentIdentity(issuanceAgent)).Build()

	body := validIssueRequest(t) + `{"namespace":"other"}`
	recorder := serveIssueRequest(t, newAgentTokenHandler(issuer, reader), http.MethodPost, body, true)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
}
