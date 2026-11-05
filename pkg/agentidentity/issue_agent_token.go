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
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	securityv1alpha1 "github.com/openkruise/agents/api/security/v1alpha1"
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	identitypkg "github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/identity/agenttoken"
	"github.com/openkruise/agents/pkg/identity/jwtissuer"
)

// maxAgentTokenRequestBytes bounds the request body. The request is three short
// strings, so anything larger is a mistake or an attempt to exhaust memory.
const maxAgentTokenRequestBytes = 1 << 20

// newAgentTokenHandler serves agent token issuance for the control plane.
//
// The handler re-reads the Sandbox and its AgentIdentity from the API rather
// than trusting the request: the request only says which object to look at, and
// the UID is compared so a caller holding a stale view of a reused name cannot
// mint a token for the sandbox that took the name over.
func newAgentTokenHandler(issuer *jwtissuer.Issuer, reader client.Reader) http.Handler {
	return &agentTokenHandler{issuer: issuer, reader: reader}
}

type agentTokenHandler struct {
	issuer *jwtissuer.Issuer
	reader client.Reader
}

func (h *agentTokenHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := uuid.NewString()
	logger := klog.FromContext(r.Context()).WithValues("requestId", requestID)

	// Discovery and JWKS are public, but minting a credential is not. The
	// listener verifies any client certificate presented against the configured
	// CA; requiring one here keeps the public endpoints usable by verifiers that
	// hold no certificate.
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
		writeIssueError(w, http.StatusUnauthorized, agenttoken.Error{
			Code: agenttoken.CodeUnauthenticated, Message: "a verified client certificate is required", RequestID: requestID,
		})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeIssueError(w, http.StatusMethodNotAllowed, agenttoken.Error{
			Code: agenttoken.CodeMethodNotAllowed, Message: "only POST is supported", RequestID: requestID,
		})
		return
	}

	var req agenttoken.Request
	if err := decodeIssueRequest(w, r, &req); err != nil {
		writeIssueError(w, http.StatusBadRequest, agenttoken.Error{
			Code: agenttoken.CodeInvalidRequest, Message: err.Error(), RequestID: requestID,
		})
		return
	}
	if req.Namespace == "" || req.SandboxName == "" || req.SandboxUID == "" {
		writeIssueError(w, http.StatusBadRequest, agenttoken.Error{
			Code: agenttoken.CodeInvalidRequest, Message: "namespace, sandboxName and sandboxUid are required", RequestID: requestID,
		})
		return
	}

	resp, status, issueErr := h.issue(r, req, requestID, logger)
	if issueErr != nil {
		writeIssueError(w, status, *issueErr)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// issue performs the trusted lookups and mints the token. It returns an error
// body and status for every refusal so ServeHTTP only has to write it.
func (h *agentTokenHandler) issue(r *http.Request, req agenttoken.Request,
	requestID string, logger klog.Logger) (*agenttoken.Response, int, *agenttoken.Error) {
	ctx := r.Context()

	sbx := &agentsv1alpha1.Sandbox{}
	if err := h.reader.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: req.SandboxName}, sbx); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, http.StatusNotFound, &agenttoken.Error{
				Code: agenttoken.CodeSandboxNotFound, RequestID: requestID,
				Message: fmt.Sprintf("sandbox %s/%s not found", req.Namespace, req.SandboxName),
			}
		}
		logger.Error(err, "failed to read the sandbox for token issuance")
		return nil, http.StatusInternalServerError, internalError(requestID)
	}
	if string(sbx.UID) != req.SandboxUID {
		return nil, http.StatusConflict, &agenttoken.Error{
			Code: agenttoken.CodeSandboxUIDMismatch, RequestID: requestID,
			Message: "the sandbox UID differs from the one in the request",
		}
	}

	agentName := sbx.GetAnnotations()[identitypkg.AnnotationAgentName]
	if agentName == "" {
		return nil, http.StatusPreconditionFailed, &agenttoken.Error{
			Code: agenttoken.CodeAgentIdentityNotSelected, RequestID: requestID,
			Message: fmt.Sprintf("sandbox %s/%s does not select an AgentIdentity via annotation %q",
				sbx.Namespace, sbx.Name, identitypkg.AnnotationAgentName),
		}
	}

	if status, issueErr := h.requireIdentityReady(ctx, sbx.Namespace, agentName, requestID, logger); issueErr != nil {
		return nil, status, issueErr
	}

	rawToken, expiry, err := h.issuer.IssueAgentToken(jwtissuer.AgentBinding{
		Namespace:         sbx.Namespace,
		AgentIdentityName: agentName,
		SandboxUID:        string(sbx.UID),
	}, 0)
	if err != nil {
		logger.Error(err, "failed to sign the agent token", "namespace", sbx.Namespace, "sandbox", sbx.Name)
		return nil, http.StatusInternalServerError, internalError(requestID)
	}

	logger.Info("agent token issued", "namespace", sbx.Namespace, "sandbox", sbx.Name,
		"agentIdentity", agentName, "expiresAt", expiry.UTC())
	return &agenttoken.Response{
		AgentToken: rawToken,
		ExpiresAt:  expiry.UTC().Format(time.RFC3339),
		RequestID:  requestID,
	}, http.StatusOK, nil
}

// requireIdentityReady enforces the AgentIdentity contract: a sandbox may
// receive a token only while the identity it selects reports Ready for its
// current generation. Missing, deleting, stale and negative verdicts all fail
// closed; none of them falls back to another identity or an unsigned token.
func (h *agentTokenHandler) requireIdentityReady(ctx context.Context, namespace, name, requestID string,
	logger klog.Logger) (int, *agenttoken.Error) {
	agentIdentity := &securityv1alpha1.AgentIdentity{}
	if err := h.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, agentIdentity); err != nil {
		if apierrors.IsNotFound(err) {
			return http.StatusPreconditionFailed, &agenttoken.Error{
				Code: agenttoken.CodeAgentIdentityNotFound, RequestID: requestID,
				Message: fmt.Sprintf("AgentIdentity %s/%s not found", namespace, name),
			}
		}
		logger.Error(err, "failed to read the agent identity", "agentIdentity", fmt.Sprintf("%s/%s", namespace, name))
		return http.StatusInternalServerError, internalError(requestID)
	}
	if !agentIdentity.DeletionTimestamp.IsZero() {
		return http.StatusPreconditionFailed, identityNotReady(requestID, namespace, name, "it is being deleted")
	}

	ready := apiMeta.FindStatusCondition(agentIdentity.Status.Conditions, securityv1alpha1.ConditionReady)
	switch {
	case ready == nil:
		return http.StatusPreconditionFailed, identityNotReady(requestID, namespace, name, "it reports no Ready condition")
	case ready.ObservedGeneration != agentIdentity.Generation ||
		agentIdentity.Status.ObservedGeneration != agentIdentity.Generation:
		return http.StatusPreconditionFailed, identityNotReady(requestID, namespace, name,
			"its Ready verdict does not describe the current generation")
	case ready.Status != metav1.ConditionTrue:
		return http.StatusPreconditionFailed, identityNotReady(requestID, namespace, name,
			fmt.Sprintf("its Ready condition is %s", ready.Status))
	}
	return http.StatusOK, nil
}

func identityNotReady(requestID, namespace, name, reason string) *agenttoken.Error {
	return &agenttoken.Error{
		Code: agenttoken.CodeAgentIdentityNotReady, RequestID: requestID,
		Message: fmt.Sprintf("AgentIdentity %s/%s is not usable: %s", namespace, name, reason),
	}
}

func internalError(requestID string) *agenttoken.Error {
	return &agenttoken.Error{Code: agenttoken.CodeInternal, Message: "internal error", RequestID: requestID}
}

// decodeIssueRequest reads exactly one JSON object. Trailing content is
// rejected rather than silently ignored, so a malformed client cannot smuggle
// a second body past the first.
func decodeIssueRequest(w http.ResponseWriter, r *http.Request, into *agenttoken.Request) error {
	body := http.MaxBytesReader(w, r.Body, maxAgentTokenRequestBytes)
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	if decoder.More() {
		return fmt.Errorf("invalid request body: trailing content after the JSON object")
	}
	return nil
}

func writeIssueError(w http.ResponseWriter, status int, body agenttoken.Error) {
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The response is already partially written, so there is nothing useful
		// left to say to the client.
		return
	}
}
