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

// Package authzcode holds the two secrets that protect one OAuth Authorization
// Code exchange.
//
// A third-party authorization round trip leaves the control plane entirely: the
// user is sent to the provider, approves there, and the browser comes back to a
// fixed callback carrying a code and a state. Nothing about that return journey
// is authenticated, so two separate values are what make it safe.
//
// State ties the callback to the session that started it. Without it anyone who
// can reach the callback could present a code of their own and have it
// exchanged against somebody else's session, which is CSRF against the
// delegation itself.
//
// The PKCE verifier ties the code redemption to the party that began the flow.
// It never leaves this process; only its challenge is published. An
// authorization code intercepted in the browser, in a redirect chain or in a
// log is then useless, because redeeming it requires a secret the interceptor
// never saw.
//
// Both are generated here and neither is stored here. Section 6.4 keeps the
// state as a digest rather than the value, so a reader of the session store
// cannot complete a flow they observed, and section 4 forbids a complete state
// or a verifier from reaching status, events or logs. The types below are built
// so the value and the stored form cannot be confused for one another.
package authzcode

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"time"
)

// entropyBytes is how much randomness backs a state and a verifier.
//
// 256 bits, which is past the point where guessing is the weakest link in
// either. RFC 7636 section 7.1 asks for at least 256 bits of entropy behind a
// verifier, and encoding 32 bytes as unpadded base64url yields 43 characters,
// the shortest length section 4.1 permits.
const entropyBytes = 32

// DefaultLifetime is how long an authorization round trip is allowed to take.
//
// It bounds a window in which a stolen state is still worth replaying, so it is
// a security parameter rather than a convenience. Ten minutes is enough for a
// user to read a consent screen and decide, and short enough that an abandoned
// session stops being interesting quickly.
const DefaultLifetime = 10 * time.Minute

// ErrStateMismatch means the state returned to the callback is not the one that
// started the session.
//
// It is deliberately indistinguishable from a state that was never issued: a
// caller cannot act differently on the two, and telling them apart would let
// somebody probe which states exist.
var ErrStateMismatch = errors.New("authzcode: state does not match the session")

// ErrExpired means the authorization window closed before the callback arrived.
var ErrExpired = errors.New("authzcode: authorization session has expired")

// State is the value sent to the provider and echoed back to the callback.
//
// It is a distinct type from StateDigest so the value cannot be stored where
// the digest belongs by passing the wrong argument. Keeping the value out of
// the store is the whole point: it means a reader of a session cannot complete
// a flow they merely observed.
type State string

// StateDigest is what a session stores in place of the state.
type StateDigest string

// Verifier is the PKCE code verifier. It stays in this process and goes to the
// provider only as a Challenge.
type Verifier string

// Challenge is the PKCE code challenge published to the provider.
type Challenge string

// ChallengeMethod is the only PKCE method this package implements.
//
// RFC 7636 defines "plain" as well, where the challenge is the verifier itself.
// That removes the protection entirely for any provider that logs or leaks the
// authorization request, so it is not offered. There is no option to select it.
const ChallengeMethod = "S256"

// NewState returns a fresh state and the digest to store alongside the session.
//
// Both are returned together because they must not be derived separately: a
// caller that generated a state and then hashed something else would produce a
// session no callback could ever satisfy, and the failure would look like an
// attack rather than a bug.
func NewState() (State, StateDigest, error) {
	value, err := randomValue()
	if err != nil {
		return "", "", fmt.Errorf("authzcode: generate state: %w", err)
	}
	state := State(value)
	return state, state.Digest(), nil
}

// Digest returns the stored form of a state.
func (s State) Digest() StateDigest {
	sum := sha256.Sum256([]byte(s))
	return StateDigest(base64.RawURLEncoding.EncodeToString(sum[:]))
}

// Matches reports whether a state returned to the callback is the one this
// digest was made from.
//
// The comparison is constant time. A byte-by-byte comparison that returned
// early would let a caller recover a digest one byte at a time by measuring how
// long the rejection took, and a digest is enough to confirm a guessed state.
func (d StateDigest) Matches(returned State) bool {
	// Both empty cases are rejected before any comparison, and the returned
	// side is the one that matters. The digest of an empty state is a perfectly
	// well formed digest, so a session holding one would otherwise be satisfied
	// by a callback that simply omitted its state parameter, which is the
	// easiest request an attacker can make. An empty stored digest is rejected
	// for the mirror reason: an unpopulated session must not be satisfiable.
	if d == "" || returned == "" {
		return false
	}
	candidate := returned.Digest()
	return subtle.ConstantTimeCompare([]byte(d), []byte(candidate)) == 1
}

// NewVerifier returns a fresh PKCE verifier and its challenge.
func NewVerifier() (Verifier, Challenge, error) {
	value, err := randomValue()
	if err != nil {
		return "", "", fmt.Errorf("authzcode: generate verifier: %w", err)
	}
	verifier := Verifier(value)
	return verifier, verifier.Challenge(), nil
}

// Challenge returns the S256 challenge for this verifier.
//
// RFC 7636 section 4.2 hashes the ASCII of the verifier string, not the random
// bytes it was encoded from. Hashing the pre-encoding bytes would produce a
// challenge no provider agrees with, and the mismatch would only surface at the
// token endpoint of a real provider.
func (v Verifier) Challenge() Challenge {
	sum := sha256.Sum256([]byte(v))
	return Challenge(base64.RawURLEncoding.EncodeToString(sum[:]))
}

// Expiry is when a session created at issuedAt stops being usable.
//
// A lifetime of zero or less falls back to DefaultLifetime, so a caller with no
// per-request policy cannot accidentally create a session that never expires.
func Expiry(issuedAt time.Time, lifetime time.Duration) time.Time {
	if lifetime <= 0 {
		lifetime = DefaultLifetime
	}
	return issuedAt.Add(lifetime)
}

// Validate checks a callback's state against a session before its code is
// exchanged.
//
// Expiry is checked first. An expired session must not have its state compared
// at all, because answering "the state was right but you are too late" confirms
// a guessed state to whoever asked.
//
// Single use is not enforced here and cannot be: this package holds no state.
// The session store is what must transition a session out of Pending
// atomically, so two callbacks carrying the same code cannot both proceed.
// Passing this check is a necessary condition for exchanging a code, never a
// sufficient one.
func Validate(stored StateDigest, returned State, expiresAt, now time.Time) error {
	if !now.Before(expiresAt) {
		return ErrExpired
	}
	if !stored.Matches(returned) {
		return ErrStateMismatch
	}
	return nil
}

// randomReader is the entropy source. It is a variable only so a test can make
// it fail: there is no production reason to use anything but the system CSPRNG,
// and no option exposed to select one.
var randomReader io.Reader = rand.Reader

// randomValue reads entropyBytes from the system CSPRNG and encodes them as
// unpadded base64url.
//
// Unpadded because both values travel in URLs: the state in a query parameter
// and the challenge in the authorization request, and RFC 7636 section 4.1
// restricts a verifier to the unreserved character set, which excludes "=".
//
// A read failure is returned rather than worked around. There is no weaker
// source to fall back to that would still make either value unguessable.
func randomValue() (string, error) {
	buffer := make([]byte, entropyBytes)
	if _, err := io.ReadFull(randomReader, buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

// Redaction.
//
// Section 4 forbids a complete state or a verifier from reaching status, events
// or logs, and section 8.2 requires a verifier to be encrypted before it is
// persisted. The likeliest way either rule is broken is not malice but a caller
// printing or serialising a struct that happens to hold one, so the types
// defend themselves rather than relying on every caller to remember.
//
// Three paths are covered and one cannot be:
//
//   - fmt verbs that use the Stringer, such as %v and %s, and %+v on an
//     exported field, are handled by String.
//   - %#v does not call String, so GoString handles it separately.
//   - encoding/json and anything built on encoding.TextMarshaler are refused
//     outright rather than redacted. Writing "[redacted]" into storage would be
//     worse than failing: the record would look valid and the flow it belongs
//     to could never complete. A caller that genuinely needs to persist these
//     must seal them first and store the ciphertext.
//
// The case that cannot be fixed here is a struct holding one of these in an
// *unexported* field. fmt cannot call methods on unexported fields, so %v on
// such a struct prints the raw value whatever this type does. A struct that
// carries a State or a Verifier must therefore either export the field or
// implement String itself. There is a test asserting the exported-field form
// stays safe, but the unexported form is a contract for callers, not something
// this package can enforce.

// errNotSerialisable is returned rather than a redacted value, so an accidental
// marshal fails loudly at the call site instead of silently writing a record
// that can never be used.
var errNotSerialisable = errors.New(
	"authzcode: a state or verifier must not be serialised; seal it and store the ciphertext")

// String redacts a state for the fmt verbs that consult a Stringer.
func (s State) String() string { return "[redacted state]" }

// GoString redacts a state for %#v, which does not call String.
func (s State) GoString() string { return `authzcode.State("[redacted state]")` }

// MarshalJSON refuses to serialise a state. See the redaction note above.
func (s State) MarshalJSON() ([]byte, error) { return nil, errNotSerialisable }

// MarshalText refuses to serialise a state, which also covers a State used as
// a map key or passed to anything built on encoding.TextMarshaler.
func (s State) MarshalText() ([]byte, error) { return nil, errNotSerialisable }

// String redacts a verifier, for the same reason as State. A leaked verifier
// makes an intercepted authorization code redeemable again, which is exactly
// what PKCE exists to prevent.
func (v Verifier) String() string { return "[redacted verifier]" }

// GoString redacts a verifier for %#v.
func (v Verifier) GoString() string { return `authzcode.Verifier("[redacted verifier]")` }

// MarshalJSON refuses to serialise a verifier.
func (v Verifier) MarshalJSON() ([]byte, error) { return nil, errNotSerialisable }

// MarshalText refuses to serialise a verifier.
func (v Verifier) MarshalText() ([]byte, error) { return nil, errNotSerialisable }
