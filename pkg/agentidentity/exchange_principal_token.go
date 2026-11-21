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
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	securityv1alpha1 "github.com/openkruise/agents/api/security/v1alpha1"
	"github.com/openkruise/agents/pkg/agentidentity/authorization"
	"github.com/openkruise/agents/pkg/identity/jwtissuer"
)

// ExchangePrincipalTokenPath is where the ExchangePrincipalToken endpoint is
// served. It is item 2.7 of the phase plan and §7.3 of the proposal, built as
// plain HTTPS JSON rather than ConnectRPC per zmberg's "we can keep it
// simple, HTTPS is also acceptable" - the same choice IssueAgentToken (item
// 1.5) made. Named under /oauth2/ to sit alongside the OAuth callback path
// §7.5 defines, even though exchanging a Principal Token is not itself part
// of the OAuth2 authorization-code flow.
//
// This is a fixed top-level path on the combined mux AddIssuer builds, unlike
// DiscoveryPath and JWKSPath, which are served relative to the issuer URL's
// own base path so they stay reachable behind an ingress path prefix. If the
// issuer is ever deployed behind such a prefix, this path would need the same
// treatment; it is not given it here because nothing in this phase deploys
// one.
const ExchangePrincipalTokenPath = "/oauth2/exchange-principal-token"

// maxExchangeRequestBodySize bounds the request body ExchangePrincipalToken
// will read. The body carries exactly one ID Token; even a large one is a few
// KiB of base64, so this is generous headroom rather than a tight fit.
const maxExchangeRequestBodySize = 16 << 10 // 16 KiB

// exchangePrincipalTokenRequestBody is the JSON request body: the ID Token
// alone, per §7.3. The Agent Token travels in the Authorization header, not
// here, so a caller cannot smuggle a different agent identity past the one
// actually verified.
type exchangePrincipalTokenRequestBody struct {
	IDToken string `json:"id_token"`
}

// exchangePrincipalTokenResponseBody is the JSON response body on success.
type exchangePrincipalTokenResponseBody struct {
	PrincipalToken string    `json:"principal_token"`
	ExpiresAt      time.Time `json:"expires_at"`
}

// errorResponseBody is the JSON response body on failure. The message is
// always one of a small set of fixed, non-sensitive strings: see
// exchangePrincipalTokenHandler.ServeHTTP, which never passes an internal
// error's own text into it. Internal detail goes to the server log only.
type errorResponseBody struct {
	Error string `json:"error"`
}

// exchangePrincipalTokenHandler implements the ExchangePrincipalToken HTTP
// endpoint.
type exchangePrincipalTokenHandler struct {
	// reader is informer-backed, the same client AddIssuer and the other
	// controllers in this package read through: this handler runs on every
	// request, so a direct API read would put the apiserver in the hot path.
	reader client.Reader
	issuer *jwtissuer.Issuer
	// authorizer wraps reader. Held as its own field rather than constructed
	// per request, since nothing about it is request-scoped.
	authorizer *authorization.Authorizer
}

// newExchangePrincipalTokenHandler builds the handler. issuer must be
// non-nil: AddIssuer only ever constructs this handler once it already has a
// configured Issuer, so there is no opt-out path to thread through here.
func newExchangePrincipalTokenHandler(reader client.Reader, issuer *jwtissuer.Issuer) *exchangePrincipalTokenHandler {
	return &exchangePrincipalTokenHandler{
		reader:     reader,
		issuer:     issuer,
		authorizer: authorization.New(reader),
	}
}

// ServeHTTP implements the exchange: verify the Agent Token in the
// Authorization header, verify the ID Token in the body, authorize the
// exchange, and mint a Principal Token.
//
// Status codes follow proposal §7.6's Connect error codes, mapped to the
// nearest HTTP equivalent since this is plain HTTPS JSON rather than
// ConnectRPC: unauthenticated -> 401, permission_denied -> 403,
// invalid_argument -> 400, internal -> 500. failed_precondition maps to 412
// here (an AgentIdentity that exists but is not Ready), chosen over 409
// because nothing about this request conflicts with another request in
// flight; it is the server-side state that is not yet in the precondition the
// request needs. An AgentIdentity that does not exist at all is a distinct
// case the proposal's own table does not fold into failed_precondition
// either (its own not_found code), so it is reported as 404.
//
// No response body here ever carries an internal error's own text, a token,
// or any other sensitive value: every message is one of the fixed strings
// below, and the real error is logged server-side only, consistent with the
// proposal's §9 requirement never to write tokens to logs and never to return
// internal detail to the caller.
func (h *exchangePrincipalTokenHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ctx := r.Context()

	rawAgentToken, err := bearerToken(r.Header.Get("Authorization"))
	if err != nil {
		klog.V(4).InfoS("ExchangePrincipalToken rejected: missing or malformed Authorization header")
		writeError(w, http.StatusUnauthorized, "missing or malformed bearer token")
		return
	}

	agentClaims, err := h.issuer.VerifyAgentToken(rawAgentToken)
	if err != nil {
		klog.V(4).InfoS("ExchangePrincipalToken rejected: agent token did not verify", "err", err)
		writeError(w, http.StatusUnauthorized, "invalid agent token")
		return
	}
	namespace, agentIdentityName := agentClaims.Namespace, agentClaims.AgentIdentityName

	r.Body = http.MaxBytesReader(w, r.Body, maxExchangeRequestBodySize)
	var body exchangePrincipalTokenRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusBadRequest, "request body too large")
			return
		}
		klog.V(4).InfoS("ExchangePrincipalToken rejected: malformed request body", "err", err)
		writeError(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if body.IDToken == "" {
		writeError(w, http.StatusBadRequest, "id_token is required")
		return
	}

	identity := &securityv1alpha1.AgentIdentity{}
	identityKey := client.ObjectKey{Namespace: namespace, Name: agentIdentityName}
	if err := h.reader.Get(ctx, identityKey, identity); err != nil {
		if apierrors.IsNotFound(err) {
			klog.V(4).InfoS("ExchangePrincipalToken rejected: AgentIdentity not found",
				"namespace", namespace, "agentIdentity", agentIdentityName)
			writeError(w, http.StatusNotFound, "agent identity not found")
			return
		}
		klog.ErrorS(err, "ExchangePrincipalToken: get AgentIdentity failed",
			"namespace", namespace, "agentIdentity", agentIdentityName)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !isAgentIdentityReady(identity) {
		klog.V(4).InfoS("ExchangePrincipalToken rejected: AgentIdentity not Ready",
			"namespace", namespace, "agentIdentity", agentIdentityName)
		writeError(w, http.StatusPreconditionFailed, "agent identity is not ready")
		return
	}

	verifiedID, err := verifyIDToken(ctx, h.reader, namespace, identity, body.IDToken)
	if err != nil {
		klog.V(4).InfoS("ExchangePrincipalToken rejected: ID token did not verify",
			"namespace", namespace, "agentIdentity", agentIdentityName, "err", err)
		writeError(w, http.StatusUnauthorized, "invalid ID token")
		return
	}

	decision, err := h.authorizer.Authorize(ctx, authorization.Request{
		Namespace:     namespace,
		AgentIdentity: agentIdentityName,
		Action:        securityv1alpha1.ActionExchangePrincipalToken,
		Resource:      "AgentIdentity/" + agentIdentityName,
	})
	if err != nil {
		klog.ErrorS(err, "ExchangePrincipalToken: authorization check failed",
			"namespace", namespace, "agentIdentity", agentIdentityName)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !decision.Allowed {
		klog.V(4).InfoS("ExchangePrincipalToken denied", "namespace", namespace,
			"agentIdentity", agentIdentityName, "reason", decision.Reason)
		writeError(w, http.StatusForbidden, "not permitted")
		return
	}

	principalToken, expiresAt, err := h.issuer.IssuePrincipalToken(jwtissuer.PrincipalBinding{
		IDToken: body.IDToken,
		Issuer:  verifiedID.Issuer,
		Subject: verifiedID.Subject,
		Agent: jwtissuer.AgentBinding{
			Namespace:         namespace,
			AgentIdentityName: agentIdentityName,
			SandboxUID:        agentClaims.SandboxUID,
		},
	}, 0)
	if err != nil {
		klog.ErrorS(err, "ExchangePrincipalToken: issue principal token failed",
			"namespace", namespace, "agentIdentity", agentIdentityName)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	klog.V(4).InfoS("ExchangePrincipalToken succeeded",
		"namespace", namespace, "agentIdentity", agentIdentityName)
	writeJSONResponse(w, http.StatusOK, exchangePrincipalTokenResponseBody{
		PrincipalToken: principalToken,
		ExpiresAt:      expiresAt,
	})
}

// isAgentIdentityReady reports whether identity's Ready condition describes
// its current generation and is True. Mirrors isAuthConfigReady: a bare
// Ready=True is not enough, since a status write can lag the generation it
// claims to describe.
func isAgentIdentityReady(identity *securityv1alpha1.AgentIdentity) bool {
	if identity.Status.ObservedGeneration != identity.Generation {
		return false
	}
	ready := apiMeta.FindStatusCondition(identity.Status.Conditions, securityv1alpha1.ConditionReady)
	return ready != nil && ready.Status == metav1.ConditionTrue && ready.ObservedGeneration == identity.Generation
}

// bearerToken extracts the token from an "Authorization: Bearer <token>"
// header value.
func bearerToken(header string) (string, error) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", errors.New("missing bearer token")
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	if token == "" {
		return "", errors.New("empty bearer token")
	}
	return token, nil
}

// writeError writes a fixed, non-sensitive error message as the JSON body.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSONResponse(w, status, errorResponseBody{Error: message})
}

// writeJSONResponse writes body as the JSON response, after the status line
// is already decided.
func writeJSONResponse(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status and headers are already written, so there is nothing
		// useful left to tell the client. Matches jwtissuer's serveJSON,
		// which does the same for the same reason.
		klog.ErrorS(err, "ExchangePrincipalToken: failed to encode response body")
	}
}
