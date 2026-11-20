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

package authzcode

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"
)

// unreserved is the character set RFC 7636 section 4.1 restricts a verifier to.
// A state travels in a query parameter, so the same set keeps it from needing
// escaping.
var unreserved = regexp.MustCompile(`^[A-Za-z0-9\-._~]+$`)

// TestNewStateAndVerifierAreUnguessable covers the one property both values
// rest on. Everything else here is structure; if these repeat or are short,
// nothing above them helps.
func TestNewStateAndVerifierAreUnguessable(t *testing.T) {
	const samples = 4096

	t.Run("states do not repeat", func(t *testing.T) {
		seen := make(map[State]int, samples)
		for i := 0; i < samples; i++ {
			state, digest, err := NewState()
			if err != nil {
				t.Fatalf("NewState %d: %v", i, err)
			}
			if previous, repeated := seen[state]; repeated {
				t.Fatalf("state %d repeats the value from state %d", i, previous)
			}
			seen[state] = i
			// The digest must belong to the state it came back with, or a
			// session could never be satisfied by its own callback.
			if !digest.Matches(state) {
				t.Fatalf("state %d does not match the digest returned with it", i)
			}
		}
	})

	t.Run("verifiers do not repeat", func(t *testing.T) {
		seen := make(map[Verifier]int, samples)
		for i := 0; i < samples; i++ {
			verifier, challenge, err := NewVerifier()
			if err != nil {
				t.Fatalf("NewVerifier %d: %v", i, err)
			}
			if previous, repeated := seen[verifier]; repeated {
				t.Fatalf("verifier %d repeats the value from verifier %d", i, previous)
			}
			seen[verifier] = i
			if challenge != verifier.Challenge() {
				t.Fatalf("verifier %d does not produce the challenge returned with it", i)
			}
		}
	})
}

// TestGeneratedValuesMeetTheWireRequirements covers the encoding both values
// need to survive a URL, and the length RFC 7636 section 4.1 requires.
func TestGeneratedValuesMeetTheWireRequirements(t *testing.T) {
	state, digest, err := NewState()
	if err != nil {
		t.Fatalf("NewState: %v", err)
	}
	verifier, challenge, err := NewVerifier()
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	cases := []struct {
		name  string
		value string
	}{
		{"state", string(state)},
		{"state digest", string(digest)},
		{"verifier", string(verifier)},
		{"challenge", string(challenge)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !unreserved.MatchString(tc.value) {
				t.Errorf("%q contains characters outside the unreserved set", tc.value)
			}
			if strings.Contains(tc.value, "=") {
				t.Errorf("%q carries base64 padding, which has to be escaped in a URL", tc.value)
			}
			// 43 characters is unpadded base64url over 32 bytes, and the
			// shortest verifier RFC 7636 permits.
			if len(tc.value) != 43 {
				t.Errorf("%q is %d characters, want 43", tc.value, len(tc.value))
			}
		})
	}
}

// TestChallengeFollowsRFC7636 derives the challenge independently rather than
// against the implementation.
//
// The trap it guards is hashing the random bytes rather than the ASCII of the
// encoded verifier. Both produce a well formed challenge, and the wrong one
// only fails at a real provider's token endpoint.
func TestChallengeFollowsRFC7636(t *testing.T) {
	verifier, challenge, err := NewVerifier()
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	sum := sha256.Sum256([]byte(string(verifier)))
	want := Challenge(base64.RawURLEncoding.EncodeToString(sum[:]))
	if challenge != want {
		t.Errorf("challenge is %q, want BASE64URL(SHA256(ASCII(verifier))) = %q", challenge, want)
	}

	// The appendix A worked example, which pins the derivation against a value
	// from the RFC itself rather than against anything in this package.
	const (
		exampleVerifier  = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
		exampleChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	)
	if got := Verifier(exampleVerifier).Challenge(); got != exampleChallenge {
		t.Errorf("RFC 7636 appendix A: got %q, want %q", got, exampleChallenge)
	}

	if ChallengeMethod != "S256" {
		t.Errorf("challenge method is %q, want S256", ChallengeMethod)
	}
}

// TestStateIsStoredOnlyAsADigest is the §6.4 requirement: a reader of the
// session store must not be able to complete a flow they only observed.
func TestStateIsStoredOnlyAsADigest(t *testing.T) {
	state, digest, err := NewState()
	if err != nil {
		t.Fatalf("NewState: %v", err)
	}

	if string(digest) == string(state) {
		t.Fatal("the stored digest is the state itself")
	}
	if strings.Contains(string(digest), string(state)) {
		t.Fatal("the stored digest contains the state")
	}
	// And the digest must not be reversible into something that matches: a
	// caller presenting the digest where the state belongs must be rejected.
	if digest.Matches(State(digest)) {
		t.Error("presenting the digest as the state was accepted")
	}
}

// TestValidate covers the callback check, including the ordering that stops an
// expired session confirming a guessed state.
func TestValidate(t *testing.T) {
	now := time.Now()
	state, digest, err := NewState()
	if err != nil {
		t.Fatalf("NewState: %v", err)
	}
	other, _, err := NewState()
	if err != nil {
		t.Fatalf("NewState: %v", err)
	}

	tests := []struct {
		name      string
		stored    StateDigest
		returned  State
		expiresAt time.Time
		wantErr   error
	}{
		{
			name:      "the matching state inside the window",
			stored:    digest,
			returned:  state,
			expiresAt: now.Add(time.Minute),
		},
		{
			name:      "a different state",
			stored:    digest,
			returned:  other,
			expiresAt: now.Add(time.Minute),
			wantErr:   ErrStateMismatch,
		},
		{
			name:      "an empty state",
			stored:    digest,
			returned:  "",
			expiresAt: now.Add(time.Minute),
			wantErr:   ErrStateMismatch,
		},
		{
			// A session with no stored digest must never be satisfiable, or an
			// unpopulated record would accept anything.
			name:      "no stored digest",
			stored:    "",
			returned:  state,
			expiresAt: now.Add(time.Minute),
			wantErr:   ErrStateMismatch,
		},
		{
			name:      "an empty state against no stored digest",
			stored:    "",
			returned:  "",
			expiresAt: now.Add(time.Minute),
			wantErr:   ErrStateMismatch,
		},
		{
			// Expiry is reported before the state is compared. Answering "the
			// state was right but you are too late" confirms a guess.
			name:      "the matching state after the window closed",
			stored:    digest,
			returned:  state,
			expiresAt: now.Add(-time.Second),
			wantErr:   ErrExpired,
		},
		{
			name:      "a wrong state after the window closed is reported as expired",
			stored:    digest,
			returned:  other,
			expiresAt: now.Add(-time.Second),
			wantErr:   ErrExpired,
		},
		{
			// The boundary belongs to the expired side: a session whose expiry
			// is exactly now has no time left.
			name:      "expiring exactly now",
			stored:    digest,
			returned:  state,
			expiresAt: now,
			wantErr:   ErrExpired,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.stored, tt.returned, tt.expiresAt, now)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("got %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestExpiry covers the fallback that stops a caller with no policy creating a
// session that never closes.
func TestExpiry(t *testing.T) {
	issuedAt := time.Now()

	tests := []struct {
		name     string
		lifetime time.Duration
		want     time.Duration
	}{
		{"an explicit lifetime", 2 * time.Minute, 2 * time.Minute},
		{"zero falls back to the default", 0, DefaultLifetime},
		{"a negative lifetime falls back too", -time.Hour, DefaultLifetime},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Expiry(issuedAt, tt.lifetime).Sub(issuedAt); got != tt.want {
				t.Errorf("expiry is %s after issue, want %s", got, tt.want)
			}
		})
	}
}

// TestSecretsRedactThemselvesWhenFormatted covers the §4 requirement that a
// complete state or verifier must not reach status, events or logs. The likely
// way one does is a caller formatting a struct that holds it, so the types
// redact rather than relying on every caller remembering.
func TestSecretsRedactThemselvesWhenFormatted(t *testing.T) {
	state, digest, err := NewState()
	if err != nil {
		t.Fatalf("NewState: %v", err)
	}
	verifier, challenge, err := NewVerifier()
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	t.Run("a state does not appear in formatted output", func(t *testing.T) {
		for _, format := range []string{"%v", "%s", "%+v"} {
			rendered := fmt.Sprintf(format, state)
			if strings.Contains(rendered, string(state)) {
				t.Errorf("%s rendered the state: %s", format, rendered)
			}
		}
		// Inside a struct, which is how it actually leaks.
		type session struct {
			State    State
			Verifier Verifier
		}
		rendered := fmt.Sprintf("%+v", session{State: state, Verifier: verifier})
		if strings.Contains(rendered, string(state)) {
			t.Errorf("a struct holding the state rendered it: %s", rendered)
		}
		if strings.Contains(rendered, string(verifier)) {
			t.Errorf("a struct holding the verifier rendered it: %s", rendered)
		}
	})

	t.Run("a verifier does not appear in formatted output", func(t *testing.T) {
		for _, format := range []string{"%v", "%s", "%+v"} {
			rendered := fmt.Sprintf(format, verifier)
			if strings.Contains(rendered, string(verifier)) {
				t.Errorf("%s rendered the verifier: %s", format, rendered)
			}
		}
	})

	t.Run("the digest and challenge are not redacted", func(t *testing.T) {
		// Both are safe to publish, and a session is identified by its digest,
		// so redacting them would make a real incident undebuggable.
		if got := fmt.Sprintf("%s", digest); got != string(digest) {
			t.Errorf("the digest was redacted: %s", got)
		}
		if got := fmt.Sprintf("%s", challenge); got != string(challenge) {
			t.Errorf("the challenge was redacted: %s", got)
		}
	})
}

// TestValidateErrorsCarryNoSecrets covers the failure paths specifically, since
// those are the ones that reach a log line or a condition message.
func TestValidateErrorsCarryNoSecrets(t *testing.T) {
	now := time.Now()
	state, digest, err := NewState()
	if err != nil {
		t.Fatalf("NewState: %v", err)
	}

	failures := []error{
		Validate(digest, state, now.Add(-time.Second), now),
		Validate(digest, State("guessed"), now.Add(time.Minute), now),
	}

	for _, failure := range failures {
		if failure == nil {
			t.Fatal("expected a failure")
		}
		message := failure.Error()
		if strings.Contains(message, string(state)) {
			t.Errorf("the error carries the state: %s", message)
		}
		if strings.Contains(message, string(digest)) {
			t.Errorf("the error carries the stored digest: %s", message)
		}
	}
}

// failingReader stands in for a CSPRNG that cannot be read.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, errors.New("entropy source unavailable")
}

// shortReader delivers one byte and then ends, which is the partial read
// io.ReadFull turns into ErrUnexpectedEOF.
//
// A reader that merely returns fewer bytes per call without ending is not a
// failure at all, since io.ReadFull keeps asking until the buffer is full, so
// modelling the short read needs the EOF.
type shortReader struct{ done bool }

func (r *shortReader) Read(p []byte) (int, error) {
	if r.done || len(p) == 0 {
		return 0, io.EOF
	}
	r.done = true
	p[0] = 1
	return 1, nil
}

// TestGenerationFailsClosedWithoutEntropy covers the branch that cannot happen
// in production and matters most if it ever does.
//
// A generator that returned an empty value with a nil error would be worse than
// one that returned an error: the digest of an empty state is a perfectly valid
// digest, so an empty callback state would match it and every session would
// accept every callback. So the test asserts no usable value escapes, not just
// that an error comes back.
func TestGenerationFailsClosedWithoutEntropy(t *testing.T) {
	readers := []struct {
		name   string
		reader func() interface{ Read([]byte) (int, error) }
	}{
		{"a reader that errors", func() interface{ Read([]byte) (int, error) } { return failingReader{} }},
		{"a reader that comes up short", func() interface{ Read([]byte) (int, error) } { return &shortReader{} }},
	}

	for _, tc := range readers {
		t.Run(tc.name, func(t *testing.T) {
			original := randomReader
			randomReader = tc.reader()
			t.Cleanup(func() { randomReader = original })

			state, digest, err := NewState()
			if err == nil {
				t.Fatal("NewState succeeded without entropy")
			}
			if state != "" || digest != "" {
				t.Errorf("NewState returned state %q digest %q alongside the error", string(state), digest)
			}

			verifier, challenge, err := NewVerifier()
			if err == nil {
				t.Fatal("NewVerifier succeeded without entropy")
			}
			if verifier != "" || challenge != "" {
				t.Errorf("NewVerifier returned verifier %q challenge %q alongside the error",
					string(verifier), challenge)
			}
		})
	}
}

// TestAnEmptyStateNeverValidates is the consequence the test above protects
// against, asserted directly: whatever goes wrong upstream, a session must not
// be satisfiable by an empty callback state.
func TestAnEmptyStateNeverValidates(t *testing.T) {
	now := time.Now()
	window := now.Add(time.Minute)

	// The digest of an empty state is a well formed digest, which is precisely
	// why this has to be rejected on the stored side rather than by luck.
	emptyDigest := State("").Digest()
	if emptyDigest == "" {
		t.Fatal("hashing an empty state produced an empty digest")
	}

	if err := Validate(emptyDigest, State(""), window, now); !errors.Is(err, ErrStateMismatch) {
		t.Errorf("an empty state validated against the digest of an empty state: %v", err)
	}
}

// TestEveryRenderingPathIsRedacted walks the ways a secret can escape a type,
// not just the ones String covers.
//
// String alone handles %v, %s and %+v on an exported field. It does nothing
// for %#v, which bypasses the Stringer, and nothing for encoding/json, which
// would write the value straight into whatever the caller is persisting. §8.2
// requires a verifier to be encrypted before it is stored, so an accidental
// marshal is a storage bug as well as a logging one.
func TestEveryRenderingPathIsRedacted(t *testing.T) {
	const (
		rawState    = "STATE-VALUE-THAT-MUST-NOT-ESCAPE"
		rawVerifier = "VERIFIER-VALUE-THAT-MUST-NOT-ESCAPE"
	)
	state := State(rawState)
	verifier := Verifier(rawVerifier)

	// A struct written the way the contract requires: exported fields.
	type held struct {
		State    State
		Verifier Verifier
	}
	carrier := held{State: state, Verifier: verifier}

	t.Run("fmt verbs", func(t *testing.T) {
		cases := []struct {
			name     string
			rendered string
		}{
			{"%v on a state", fmt.Sprintf("%v", state)},
			{"%s on a state", fmt.Sprintf("%s", state)},
			{"%q on a state", fmt.Sprintf("%q", state)},
			{"%#v on a state", fmt.Sprintf("%#v", state)},
			{"%v on a verifier", fmt.Sprintf("%v", verifier)},
			{"%s on a verifier", fmt.Sprintf("%s", verifier)},
			{"%q on a verifier", fmt.Sprintf("%q", verifier)},
			{"%#v on a verifier", fmt.Sprintf("%#v", verifier)},
			{"%v on a struct", fmt.Sprintf("%v", carrier)},
			{"%+v on a struct", fmt.Sprintf("%+v", carrier)},
			{"%#v on a struct", fmt.Sprintf("%#v", carrier)},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if strings.Contains(tc.rendered, rawState) {
					t.Errorf("the state escaped: %s", tc.rendered)
				}
				if strings.Contains(tc.rendered, rawVerifier) {
					t.Errorf("the verifier escaped: %s", tc.rendered)
				}
			})
		}
	})

	t.Run("serialisation is refused rather than redacted", func(t *testing.T) {
		// Refused, because writing "[redacted]" into storage would produce a
		// record that looks valid and belongs to a flow that can never
		// complete.
		for _, tc := range []struct {
			name string
			run  func() ([]byte, error)
		}{
			{"a state", func() ([]byte, error) { return json.Marshal(state) }},
			{"a verifier", func() ([]byte, error) { return json.Marshal(verifier) }},
			{"a struct holding both", func() ([]byte, error) { return json.Marshal(carrier) }},
			{"a state as text", func() ([]byte, error) { return state.MarshalText() }},
			{"a verifier as text", func() ([]byte, error) { return verifier.MarshalText() }},
		} {
			t.Run(tc.name, func(t *testing.T) {
				encoded, err := tc.run()
				if err == nil {
					t.Fatalf("marshalling succeeded and produced %s", encoded)
				}
				// And the failure must not itself carry the secret.
				if strings.Contains(err.Error(), rawState) ||
					strings.Contains(err.Error(), rawVerifier) {
					t.Errorf("the error carries the secret: %v", err)
				}
			})
		}
	})

	t.Run("the safe values still render and serialise", func(t *testing.T) {
		// Redacting these too would make a real incident undebuggable and
		// stop a session being stored at all.
		digest := state.Digest()
		challenge := verifier.Challenge()

		if got := fmt.Sprintf("%v", digest); got != string(digest) {
			t.Errorf("the digest was redacted: %s", got)
		}
		if got := fmt.Sprintf("%#v", challenge); !strings.Contains(got, string(challenge)) {
			t.Errorf("the challenge was redacted under %%#v: %s", got)
		}
		encoded, err := json.Marshal(struct {
			Digest    StateDigest `json:"digest"`
			Challenge Challenge   `json:"challenge"`
		}{digest, challenge})
		if err != nil {
			t.Fatalf("the digest and challenge must stay serialisable: %v", err)
		}
		if !strings.Contains(string(encoded), string(digest)) {
			t.Errorf("the digest did not survive marshalling: %s", encoded)
		}
	})
}
