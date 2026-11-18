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

package jwtauth

import (
	"errors"
	"fmt"

	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/identity/oidc"
)

// ErrVerifierUnavailable distinguishes missing JWT capability from an invalid
// credential. Serving traffic must not fall back to another authentication mode.
var ErrVerifierUnavailable = errors.New("JWT verifier is unavailable")

// ErrServingIncarnation identifies a validly signed token for an obsolete UID.
var ErrServingIncarnation = errors.New("Serving incarnation does not match")

// VerifyServingToken authenticates against trusted Serving state, without
// selecting or waking a member. Callers must resolve the ID and UID from resource
// observations, never from headers or unverified claims.
func VerifyServingToken(verifier oidc.Verifier, rawJWT, servingID, servingUID string) error {
	if servingUID == "" {
		return errors.New("Serving UID is required")
	}
	subject, err := identity.ServingTokenSubject(servingID)
	if err != nil {
		return err
	}
	if verifier == nil {
		return ErrVerifierUnavailable
	}
	claims, err := verifier.Verify(rawJWT)
	if err != nil {
		return fmt.Errorf("verify Serving token: %w", err)
	}
	// Sandbox tokens use a different binding contract. Reject mixed bindings
	// so one credential cannot authorize both resource kinds.
	if claims == nil || claims.Sandbox != nil || claims.Subject != subject ||
		claims.Metadata["servingId"] != servingID || claims.Metadata["servingUid"] == "" {
		return errors.New("traffic access token does not match Serving")
	}
	if claims.Metadata["servingUid"] != servingUID {
		return fmt.Errorf("traffic access token does not match Serving: %w", ErrServingIncarnation)
	}
	return nil
}
