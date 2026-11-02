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

package aead

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func mustKey(t *testing.T, id string) Key {
	t.Helper()
	key, err := NewKey(id)
	if err != nil {
		t.Fatalf("NewKey(%q): %v", id, err)
	}
	return key
}

func mustKeyring(t *testing.T, active Key, retained ...Key) *Keyring {
	t.Helper()
	keyring, err := NewKeyring(active, retained...)
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	return keyring
}

// delegationContext is the shape §8.1 isolates a delegation by, used here as
// the associated data a record is bound to.
func delegationContext(namespace, principal, agent, provider string) []string {
	return []string{namespace, principal, agent, provider}
}

func TestSealOpenRoundTrip(t *testing.T) {
	keyring := mustKeyring(t, mustKey(t, "2026-10"))
	context := delegationContext("team-a", "principal-1", "sample-agent", "github-3lo")
	plaintext := []byte("gho_exampleaccesstoken")

	stored, err := keyring.Seal(plaintext, context...)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// The stored form must not contain the plaintext anywhere. This is the
	// "base64 is not encryption" check: a record that merely encoded the value
	// would pass a round trip and fail here.
	if bytes.Contains(stored, plaintext) {
		t.Fatal("the sealed record contains the plaintext")
	}

	opened, err := keyring.Open(stored, context...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(opened, plaintext) {
		t.Errorf("round trip: got %q, want %q", opened, plaintext)
	}
}

// TestSealedRecordIsSelfDescribing covers what §8.2 requires every record to
// carry, so a later migration can read a record without external context.
func TestSealedRecordIsSelfDescribing(t *testing.T) {
	keyring := mustKeyring(t, mustKey(t, "2026-10"))
	stored, err := keyring.Seal([]byte("secret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	var sealed record
	if err := json.Unmarshal(stored, &sealed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sealed.Schema != SchemaVersion {
		t.Errorf("schema: got %d, want %d", sealed.Schema, SchemaVersion)
	}
	if sealed.Algorithm != AlgorithmAESGCM {
		t.Errorf("algorithm: got %q, want %q", sealed.Algorithm, AlgorithmAESGCM)
	}
	if sealed.KeyID != "2026-10" {
		t.Errorf("key ID: got %q, want %q", sealed.KeyID, "2026-10")
	}
	if len(sealed.Nonce) == 0 {
		t.Error("nonce is absent")
	}
	if len(sealed.Ciphertext) == 0 {
		t.Error("ciphertext is absent")
	}
}

// TestNonceIsNeverReused is the property GCM depends on most. Reuse under one
// key breaks confidentiality and integrity for every message, not just the
// repeated one.
func TestNonceIsNeverReused(t *testing.T) {
	keyring := mustKeyring(t, mustKey(t, "2026-10"))
	const iterations = 256

	seen := make(map[string]struct{}, iterations)
	ciphertexts := make(map[string]struct{}, iterations)
	for i := 0; i < iterations; i++ {
		stored, err := keyring.Seal([]byte("the same plaintext every time"))
		if err != nil {
			t.Fatalf("Seal %d: %v", i, err)
		}
		var sealed record
		if err := json.Unmarshal(stored, &sealed); err != nil {
			t.Fatalf("decode %d: %v", i, err)
		}
		if _, repeated := seen[string(sealed.Nonce)]; repeated {
			t.Fatalf("nonce repeated after %d seals", i)
		}
		seen[string(sealed.Nonce)] = struct{}{}
		ciphertexts[string(sealed.Ciphertext)] = struct{}{}
	}

	// Identical plaintext must not produce identical ciphertext, or a reader
	// could tell that two delegations hold the same token.
	if len(ciphertexts) != iterations {
		t.Errorf("identical plaintext produced %d distinct ciphertexts, want %d",
			len(ciphertexts), iterations)
	}
}

// TestOpenRejectsTamperingAndMisbinding is the reason this uses AEAD rather
// than plain encryption. Each case is something a party who can write Secrets
// but not read the root key might try.
func TestOpenRejectsTamperingAndMisbinding(t *testing.T) {
	context := delegationContext("team-a", "principal-1", "sample-agent", "github-3lo")

	tests := []struct {
		name string
		// corrupt mutates the stored record, returning what Open should reject.
		corrupt func(t *testing.T, keyring *Keyring, stored []byte) []byte
		// openContext is what Open is called with; nil means the original.
		openContext []string
		wantErr     error
	}{
		{
			name: "a flipped ciphertext bit",
			corrupt: func(_ *testing.T, _ *Keyring, stored []byte) []byte {
				var sealed record
				_ = json.Unmarshal(stored, &sealed)
				sealed.Ciphertext[0] ^= 0x01
				out, _ := json.Marshal(sealed)
				return out
			},
			wantErr: ErrNotAuthentic,
		},
		{
			name: "a flipped nonce bit",
			corrupt: func(_ *testing.T, _ *Keyring, stored []byte) []byte {
				var sealed record
				_ = json.Unmarshal(stored, &sealed)
				sealed.Nonce[0] ^= 0x01
				out, _ := json.Marshal(sealed)
				return out
			},
			wantErr: ErrNotAuthentic,
		},
		{
			name: "a truncated ciphertext",
			corrupt: func(_ *testing.T, _ *Keyring, stored []byte) []byte {
				var sealed record
				_ = json.Unmarshal(stored, &sealed)
				sealed.Ciphertext = sealed.Ciphertext[:len(sealed.Ciphertext)-1]
				out, _ := json.Marshal(sealed)
				return out
			},
			wantErr: ErrNotAuthentic,
		},
		{
			name: "a nonce of the wrong length",
			corrupt: func(_ *testing.T, _ *Keyring, stored []byte) []byte {
				var sealed record
				_ = json.Unmarshal(stored, &sealed)
				sealed.Nonce = append(sealed.Nonce, 0x00)
				out, _ := json.Marshal(sealed)
				return out
			},
			wantErr: ErrNotAuthentic,
		},
		{
			// The attack the associated data exists to stop: a valid record moved
			// to a different user's delegation.
			name:        "a record replayed against another principal",
			corrupt:     func(_ *testing.T, _ *Keyring, stored []byte) []byte { return stored },
			openContext: delegationContext("team-a", "principal-2", "sample-agent", "github-3lo"),
			wantErr:     ErrNotAuthentic,
		},
		{
			name:        "a record replayed against another namespace",
			corrupt:     func(_ *testing.T, _ *Keyring, stored []byte) []byte { return stored },
			openContext: delegationContext("team-b", "principal-1", "sample-agent", "github-3lo"),
			wantErr:     ErrNotAuthentic,
		},
		{
			name:        "a record replayed against another agent identity",
			corrupt:     func(_ *testing.T, _ *Keyring, stored []byte) []byte { return stored },
			openContext: delegationContext("team-a", "principal-1", "other-agent", "github-3lo"),
			wantErr:     ErrNotAuthentic,
		},
		{
			name:        "a record replayed against another provider",
			corrupt:     func(_ *testing.T, _ *Keyring, stored []byte) []byte { return stored },
			openContext: delegationContext("team-a", "principal-1", "sample-agent", "gitlab"),
			wantErr:     ErrNotAuthentic,
		},
		{
			name:        "a record opened with no context at all",
			corrupt:     func(_ *testing.T, _ *Keyring, stored []byte) []byte { return stored },
			openContext: []string{},
			wantErr:     ErrNotAuthentic,
		},
		{
			name: "a record naming a key the keyring does not hold",
			corrupt: func(_ *testing.T, _ *Keyring, stored []byte) []byte {
				var sealed record
				_ = json.Unmarshal(stored, &sealed)
				sealed.KeyID = "a-key-that-was-dropped"
				out, _ := json.Marshal(sealed)
				return out
			},
			wantErr: ErrUnknownKey,
		},
		{
			name: "a record from a future schema",
			corrupt: func(_ *testing.T, _ *Keyring, stored []byte) []byte {
				var sealed record
				_ = json.Unmarshal(stored, &sealed)
				sealed.Schema = SchemaVersion + 1
				out, _ := json.Marshal(sealed)
				return out
			},
			wantErr: ErrUnsupportedSchema,
		},
		{
			name: "a record naming another algorithm",
			corrupt: func(_ *testing.T, _ *Keyring, stored []byte) []byte {
				var sealed record
				_ = json.Unmarshal(stored, &sealed)
				sealed.Algorithm = "AES-128-CBC"
				out, _ := json.Marshal(sealed)
				return out
			},
			wantErr: ErrUnsupportedAlgorithm,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keyring := mustKeyring(t, mustKey(t, "2026-10"))
			stored, err := keyring.Seal([]byte("gho_exampleaccesstoken"), context...)
			if err != nil {
				t.Fatalf("Seal: %v", err)
			}

			openContext := context
			if tt.openContext != nil {
				openContext = tt.openContext
			}

			opened, err := keyring.Open(tt.corrupt(t, keyring, stored), openContext...)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Open error: got %v, want %v", err, tt.wantErr)
			}
			if opened != nil {
				t.Error("a rejected record must not yield plaintext")
			}
		})
	}
}

// TestRotationKeepsOlderRecordsReadable covers why retained keys exist: a new
// key must start sealing without making existing records unreadable.
func TestRotationKeepsOlderRecordsReadable(t *testing.T) {
	previous := mustKey(t, "2026-04")
	current := mustKey(t, "2026-10")
	context := delegationContext("team-a", "principal-1", "sample-agent", "github-3lo")

	before := mustKeyring(t, previous)
	old, err := before.Seal([]byte("sealed under the previous key"), context...)
	if err != nil {
		t.Fatalf("Seal under the previous key: %v", err)
	}

	after := mustKeyring(t, current, previous)
	if got := after.ActiveKeyID(); got != "2026-10" {
		t.Errorf("active key: got %q, want %q", got, "2026-10")
	}

	opened, err := after.Open(old, context...)
	if err != nil {
		t.Fatalf("a retained key must still open its records: %v", err)
	}
	if string(opened) != "sealed under the previous key" {
		t.Errorf("round trip after rotation: got %q", opened)
	}

	// New records seal under the current key, so re-sealing is what eventually
	// lets the previous key be dropped.
	fresh, err := after.Seal([]byte("sealed under the current key"), context...)
	if err != nil {
		t.Fatalf("Seal under the current key: %v", err)
	}
	var sealed record
	if err := json.Unmarshal(fresh, &sealed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sealed.KeyID != "2026-10" {
		t.Errorf("new records must use the active key, got %q", sealed.KeyID)
	}

	// Dropping the previous key is what makes its records unreadable, which is
	// the lever for invalidating everything sealed under it at once.
	dropped := mustKeyring(t, current)
	if _, err := dropped.Open(old, context...); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("after dropping the previous key: got %v, want %v", err, ErrUnknownKey)
	}
}

// TestContextPartsAreUnambiguous covers the length-prefixing. Concatenating the
// parts would make these two contexts identical, so a record addressed to one
// delegation would authenticate against another.
func TestContextPartsAreUnambiguous(t *testing.T) {
	keyring := mustKeyring(t, mustKey(t, "2026-10"))

	stored, err := keyring.Seal([]byte("token"), "ab", "c")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := keyring.Open(stored, "a", "bc"); !errors.Is(err, ErrNotAuthentic) {
		t.Errorf(`Open with {"a","bc"} a record sealed with {"ab","c"}: got %v, want %v`,
			err, ErrNotAuthentic)
	}
	if _, err := keyring.Open(stored, "ab", "c"); err != nil {
		t.Errorf("Open with the original context: %v", err)
	}
}

// TestKeyringOwnsItsKeyMaterial covers the keyring keeping a copy rather than a
// reference to the caller's key material.
//
// Wiping a key buffer once it has been handed over is the careful thing for a
// caller to do. If the keyring aliased that buffer, doing so would turn every
// later Seal and Open into an authentication failure, with nothing in the error
// to suggest the key had been zeroed underneath.
func TestKeyringOwnsItsKeyMaterial(t *testing.T) {
	secret := bytes.Repeat([]byte{0x11}, KeySize)
	keyring, err := NewKeyring(Key{ID: "2026-10", Secret: secret})
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}

	stored, err := keyring.Seal([]byte("token"), "team-a")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	for i := range secret {
		secret[i] = 0
	}

	// Sealing has to keep working too: the active key is held separately from
	// the lookup map, so a copy in one and a reference in the other would pass
	// an Open-only check.
	resealed, err := keyring.Seal([]byte("token"), "team-a")
	if err != nil {
		t.Fatalf("Seal after the caller wiped its key: %v", err)
	}
	if _, err := keyring.Open(resealed, "team-a"); err != nil {
		t.Fatalf("Open a record sealed after the caller wiped its key: %v", err)
	}

	opened, err := keyring.Open(stored, "team-a")
	if err != nil {
		t.Fatalf("Open after the caller wiped its key: %v", err)
	}
	if string(opened) != "token" {
		t.Errorf("Open returned %q, want %q", opened, "token")
	}
}

func TestNewKeyringRejectsBadKeys(t *testing.T) {
	good := mustKey(t, "2026-10")

	tests := []struct {
		name     string
		active   Key
		retained []Key
		wantErr  string
	}{
		{
			name:    "an empty key ID",
			active:  Key{ID: "", Secret: good.Secret},
			wantErr: "key ID must not be empty",
		},
		{
			name:    "a short key",
			active:  Key{ID: "weak", Secret: make([]byte, 16)},
			wantErr: "key must be 32 bytes, got 16",
		},
		{
			name:    "a nil key",
			active:  Key{ID: "absent"},
			wantErr: "key must be 32 bytes, got 0",
		},
		{
			name:     "a duplicate key ID",
			active:   good,
			retained: []Key{{ID: good.ID, Secret: make([]byte, KeySize)}},
			wantErr:  "already present in the keyring",
		},
		{
			name:     "a retained key that is too short",
			active:   good,
			retained: []Key{{ID: "2026-04", Secret: make([]byte, 31)}},
			wantErr:  "key must be 32 bytes, got 31",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keyring, err := NewKeyring(tt.active, tt.retained...)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error: got %q, want it to contain %q", err, tt.wantErr)
			}
			if keyring != nil {
				t.Error("a rejected keyring must not be returned")
			}
		})
	}
}

func TestNewKeyGeneratesDistinctKeys(t *testing.T) {
	first := mustKey(t, "a")
	second := mustKey(t, "b")
	if len(first.Secret) != KeySize {
		t.Fatalf("key length: got %d, want %d", len(first.Secret), KeySize)
	}
	if bytes.Equal(first.Secret, second.Secret) {
		t.Error("two generated keys share material")
	}
	if _, err := NewKey(""); err == nil {
		t.Error("expected an empty key ID to be rejected")
	}
}
