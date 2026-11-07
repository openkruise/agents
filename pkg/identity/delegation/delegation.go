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

// Package delegation stores the two records a user-delegated credential needs:
// the short-lived OAuth session an authorization round trip runs through, and
// the long-lived delegation it produces.
//
// Both hold material that must not be readable by anything able to read the
// object they live in, so both are sealed before storage with the address they
// belong to bound into the seal. Section 8.2 fixes the shape: separate records
// for sessions and delegations, authenticated encryption before storage, and
// resourceVersion with compare-and-swap for the races between a callback, a
// refresh and an invalidation.
//
// The interfaces are here and the Kubernetes implementation sits beside them.
// Section 8.2 anticipates other backends, and what a second backend has to
// preserve is not the Secret layout but the concurrency semantics, which is why
// the CAS contract is stated on the interface rather than left to the adapter.
package delegation

import (
	"context"
	"errors"
	"time"
)

// SessionStatus is where an authorization round trip has reached.
//
// Section 6.4 fixes the transitions: only Pending may become Exchanging, and
// that move has to be atomic, because it is what stops a replayed callback
// calling the provider's token endpoint a second time.
type SessionStatus string

const (
	// StatusPending is a session whose callback has not arrived.
	StatusPending SessionStatus = "Pending"
	// StatusExchanging means a callback claimed this session and the code
	// exchange is in flight. A second callback carrying the same state finds
	// this and stops.
	StatusExchanging SessionStatus = "Exchanging"
	// StatusCompleted means the exchange succeeded and a delegation exists.
	StatusCompleted SessionStatus = "Completed"
	// StatusFailed means the user declined, the session expired, or the
	// exchange failed.
	StatusFailed SessionStatus = "Failed"
)

// DelegationState is whether a stored delegation can still be used.
type DelegationState string

const (
	// DelegationActive means the credential is usable, subject to its own
	// expiry and a fresh authorization check on every read.
	DelegationActive DelegationState = "Active"
	// DelegationNeedsReauthorization means the provider rejected the refresh,
	// typically with invalid_grant, so only the user can restore it. The record
	// is kept rather than deleted so a caller can tell "never authorized" from
	// "authorization withdrawn".
	DelegationNeedsReauthorization DelegationState = "NeedsReauthorization"
)

var (
	// ErrNotFound means no record exists at that address. It is distinct from a
	// read failure so a caller can tell "this user has not authorized" from
	// "the store is unavailable", which are different answers to give an agent.
	ErrNotFound = errors.New("delegation: record not found")

	// ErrConflict means another writer changed the record first. The caller
	// should re-read and decide again rather than retry blindly, because the
	// decision it made may no longer apply to what is now stored.
	ErrConflict = errors.New("delegation: record was modified concurrently")

	// ErrNotPending means a session was claimed by someone else, or has already
	// completed or failed. A replayed callback is the expected cause, and the
	// reasons are deliberately not distinguishable: a caller cannot act
	// differently on them, and saying which would tell an attacker whether
	// their guessed state ever existed.
	ErrNotPending = errors.New("delegation: session is not pending")

	// ErrExpired means the session's window closed before the callback arrived.
	ErrExpired = errors.New("delegation: session has expired")
)

// SessionKey addresses one authorization round trip.
//
// Section 6.4 stores only the digest of the state and looks a session up by it,
// so the digest is the key. The raw state never reaches storage, which is what
// stops a reader of the store completing a flow they merely observed.
type SessionKey struct {
	// Namespace the session belongs to.
	Namespace string
	// StateDigest is the stored form of the state the provider echoes back.
	StateDigest string
}

// Session is the in-flight half of a delegation.
//
// Section 6.4 lists what it must hold. Everything a callback must not be
// allowed to supply is here, because the callback restores those values from
// the session rather than reading them out of its own query parameters.
type Session struct {
	Key SessionKey

	// Verifier is the PKCE code verifier. It is sealed at rest and never
	// leaves this process in cleartext; only its challenge was published.
	Verifier string

	// The address this session will produce a delegation for, restored from
	// here on callback and never taken from the request.
	AgentIdentity      string
	SandboxName        string
	SandboxUID         string
	PrincipalID        string
	CredentialProvider string

	// Scopes requested for this round trip. A delegation may not end up with
	// more than these.
	Scopes []string

	// RedirectURI as registered with the provider, kept so the token exchange
	// sends back the identical value.
	RedirectURI string

	CreatedAt time.Time
	ExpiresAt time.Time
	Status    SessionStatus

	// Version is the store's opaque concurrency token. A caller passes back
	// what it read, and a write fails with ErrConflict if the record moved on.
	// It is never part of what is sealed.
	Version string
}

// Expired reports whether the authorization window has closed.
func (s *Session) Expired(now time.Time) bool {
	return !now.Before(s.ExpiresAt)
}

// String redacts the session.
//
// Section 4 forbids a complete state or a PKCE verifier from reaching status,
// events or logs, and the likeliest way one gets there is a caller formatting
// a struct that holds it. The digest is safe and identifies the session, so it
// is the one thing kept.
func (s Session) String() string {
	return "Session{" + s.Key.Namespace + "/" + s.Key.StateDigest + " " + string(s.Status) + "}"
}

// DelegationKey addresses one stored delegation.
//
// Section 8.1 fixes these four parts. AgentIdentity is in the key because two
// agents acting for the same user against the same provider must not share a
// delegation: a user authorizing one agent has not authorized the other.
type DelegationKey struct {
	Namespace          string
	PrincipalID        string
	AgentIdentity      string
	CredentialProvider string
}

// Delegation is a user's standing authorization for one agent and one provider.
type Delegation struct {
	Key DelegationKey

	// AccessToken and RefreshToken are sealed at rest. The refresh token never
	// leaves the identity provider; an agent only ever receives a short-lived
	// access token derived from it.
	AccessToken  string
	RefreshToken string

	// ExternalAccountID identifies the user at the provider. Sealed because it
	// is a user identifier, and section 8.2 lists it alongside the tokens.
	ExternalAccountID string

	// Scopes actually granted, which may be narrower than those requested.
	Scopes []string

	// AccessTokenExpiresAt is when the access token stops working. A refresh is
	// due before this, not after.
	AccessTokenExpiresAt time.Time

	State     DelegationState
	UpdatedAt time.Time

	// Version is the store's opaque concurrency token, as on Session.
	Version string
}

// String redacts the delegation, for the same reason Session does.
func (d Delegation) String() string {
	return "Delegation{" + d.Key.Namespace + "/" + d.Key.AgentIdentity +
		"/" + d.Key.CredentialProvider + " " + string(d.State) + "}"
}

// SessionStore holds authorization sessions.
//
// The atomicity of BeginExchange is the reason this is an interface rather than
// a map: a second backend may keep sessions anywhere, but it must make that one
// transition indivisible or a replayed callback redeems the code twice.
type SessionStore interface {
	// Create stores a new Pending session. It fails with ErrConflict if a
	// session already exists at that address, so a repeated state cannot
	// silently overwrite one already in flight.
	Create(ctx context.Context, session *Session) error

	// Get returns the session at key, or ErrNotFound.
	Get(ctx context.Context, key SessionKey) (*Session, error)

	// BeginExchange atomically moves a Pending session to Exchanging and
	// returns it as it now stands.
	//
	// It returns ErrNotPending when the session was already claimed, completed
	// or failed, and ErrExpired when its window closed. Expiry is reported
	// before the status so a late callback carrying a correct state learns
	// nothing about whether that state was ever pending.
	//
	// This is the single-use enforcement point: a caller may contact the
	// provider's token endpoint only after this returns nil.
	BeginExchange(ctx context.Context, key SessionKey, now time.Time) (*Session, error)

	// Finish records the terminal status of a session being exchanged. Only
	// Completed and Failed are accepted.
	Finish(ctx context.Context, session *Session, status SessionStatus) error

	// Delete removes a session. Absence is not an error, because a caller
	// cleaning up after a failure should not have to care whether something
	// else got there first.
	Delete(ctx context.Context, key SessionKey) error
}

// DelegationStore holds standing user authorizations.
type DelegationStore interface {
	// Get returns the delegation at key, or ErrNotFound.
	Get(ctx context.Context, key DelegationKey) (*Delegation, error)

	// Put writes a delegation. An empty Version creates; a non-empty one
	// updates only if the stored record still carries that version, failing
	// with ErrConflict otherwise.
	//
	// This is what serialises a callback completing, a refresh rotating the
	// tokens and an invalidation clearing them, which section 4.4 names as the
	// races that matter.
	Put(ctx context.Context, record *Delegation) error

	// Delete removes a delegation, which is what withdrawing an authorization
	// means. Absence is not an error.
	Delete(ctx context.Context, key DelegationKey) error
}
