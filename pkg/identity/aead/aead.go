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

// Package aead encrypts the records the identity provider persists.
//
// OAuth sessions and user delegations hold third-party access tokens, refresh
// tokens, PKCE verifiers and external account identifiers. They are kept in
// Kubernetes Secrets, whose contents are base64 but not encrypted: anything
// that can read the Secret can read the values. So the records are sealed
// before they are stored.
//
// The root key they are sealed under is read from a file mount and never from
// the API server. Those records live in Secrets in sandbox-system, so a root
// key held in a Secret there would be reachable by the same RBAC grant that
// reaches the ciphertext, and encrypting under it would protect nothing.
//
// Authenticated encryption is what makes a sealed record tamper-evident as well
// as unreadable, and the associated data is what binds a record to the one place
// it belongs. Without that binding, a reader who can write Secrets could move a
// valid ciphertext from one user's delegation to another's and the plaintext
// would still decrypt.
package aead

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	// SchemaVersion is the layout version of a sealed record. It is stored with
	// every record so a future change to the envelope can be recognised rather
	// than guessed at from a decode failure.
	SchemaVersion = 1

	// AlgorithmAESGCM names the only algorithm this package implements. It is
	// recorded per record so a later migration can tell what a record was sealed
	// with instead of assuming.
	AlgorithmAESGCM = "AES-256-GCM"

	// KeySize is the required root key length. AES-256 is not negotiable here:
	// accepting a shorter key would silently select AES-128 or AES-192 from the
	// same call.
	KeySize = 32
)

var (
	// ErrUnknownKey means the record was sealed under a key this keyring does
	// not hold, which is what a prematurely dropped retained key looks like.
	ErrUnknownKey = errors.New("aead: record was sealed under an unknown key")

	// ErrUnsupportedSchema means the record predates or postdates this code.
	ErrUnsupportedSchema = errors.New("aead: unsupported record schema version")

	// ErrUnsupportedAlgorithm means the record names an algorithm this package
	// cannot open.
	ErrUnsupportedAlgorithm = errors.New("aead: unsupported record algorithm")

	// ErrNotAuthentic means the record did not authenticate: it was altered, it
	// was sealed under different associated data, or the key is wrong. These are
	// deliberately indistinguishable, since telling them apart would tell an
	// attacker which part of a forgery to fix.
	ErrNotAuthentic = errors.New("aead: record failed authentication")
)

// Key is a root encryption key and the ID published alongside every record it
// seals. The ID must stay stable for as long as records sealed under it can
// still be read.
type Key struct {
	ID     string
	Secret []byte
}

// Keyring seals with one key and opens with any key it holds.
//
// Retaining superseded keys is what makes rotation survivable: a new key starts
// sealing immediately while existing records still open under the old one. A
// retained key may only be dropped once nothing sealed under it remains, which
// for delegations means after the longest-lived one has been re-sealed or
// deleted.
type Keyring struct {
	active Key
	byID   map[string]Key
}

// record is the stored form. Field names are short because every sealed value
// carries them, and explicit because the envelope has to be self-describing:
// anything reading a record must learn which key and algorithm produced it
// without external context.
type record struct {
	Schema     int    `json:"v"`
	Algorithm  string `json:"alg"`
	KeyID      string `json:"kid"`
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ct"`
}

// NewKeyring returns a Keyring that seals under active and opens under active
// or any retained key.
func NewKeyring(active Key, retained ...Key) (*Keyring, error) {
	keyring := &Keyring{byID: make(map[string]Key, 1+len(retained))}

	owned, err := keyring.add(active)
	if err != nil {
		return nil, fmt.Errorf("active key: %w", err)
	}
	// Assigned from what add stored rather than from the argument. The active
	// key is held separately from the lookup map, so taking it from the
	// argument would leave it aliasing the caller's slice while the map held a
	// copy, and sealing would break on a wipe that opening survived.
	keyring.active = owned

	for _, key := range retained {
		if _, err := keyring.add(key); err != nil {
			return nil, fmt.Errorf("retained key %q: %w", key.ID, err)
		}
	}
	return keyring, nil
}

// add validates key, stores a copy of it, and returns the copy it stored.
//
// The material is copied so the keyring owns it. A caller holding the only
// other reference is entitled to wipe its own buffer once the keyring is built,
// which is the careful thing to do with key material, and if the keyring
// aliased it that would turn every later Seal and Open into an authentication
// failure with nothing to point at.
//
// The copy is returned rather than only stored so a caller that needs to hold
// the key elsewhere takes it from here, instead of from an argument that the
// keyring does not own.
func (r *Keyring) add(key Key) (Key, error) {
	if key.ID == "" {
		return Key{}, errors.New("key ID must not be empty")
	}
	if len(key.Secret) != KeySize {
		return Key{}, fmt.Errorf("key must be %d bytes, got %d", KeySize, len(key.Secret))
	}
	if _, exists := r.byID[key.ID]; exists {
		return Key{}, errors.New("key ID is already present in the keyring")
	}
	owned := Key{ID: key.ID, Secret: bytes.Clone(key.Secret)}
	r.byID[key.ID] = owned
	return owned, nil
}

// ActiveKeyID is the key new records are sealed under. Useful for deciding
// whether a record needs re-sealing after a rotation.
func (r *Keyring) ActiveKeyID() string {
	return r.active.ID
}

// Seal encrypts plaintext under the active key and returns the stored record.
//
// context identifies where the record belongs, for example its namespace,
// principal, agent identity and provider. It is authenticated but not
// encrypted, and Open must be given exactly the same values: that is what stops
// a ciphertext being moved to a different delegation and still decrypting.
// Passing no context is allowed but means a record is bound to nothing, so it
// should only be used where the record genuinely has no address.
func (r *Keyring) Seal(plaintext []byte, context ...string) ([]byte, error) {
	gcm, err := newGCM(r.active.Secret)
	if err != nil {
		return nil, err
	}

	// A nonce must never repeat under one key. GCM loses confidentiality and
	// integrity on reuse, not just the one message, so this is read from the
	// system CSPRNG and a failure is fatal to the call rather than retried with
	// anything weaker.
	//
	// Random 96-bit nonces carry a birthday bound rather than a hard guarantee,
	// which NIST SP 800-38D puts at 2^32 seals under one key. Nothing here
	// counts invocations, so that ceiling is a reason to rotate on a schedule
	// and not only in response to a suspected compromise.
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("aead: read nonce: %w", err)
	}

	sealed := record{
		Schema:     SchemaVersion,
		Algorithm:  AlgorithmAESGCM,
		KeyID:      r.active.ID,
		Nonce:      nonce,
		Ciphertext: gcm.Seal(nil, nonce, plaintext, associatedData(context)),
	}
	stored, err := json.Marshal(sealed)
	if err != nil {
		return nil, fmt.Errorf("aead: encode record: %w", err)
	}
	return stored, nil
}

// Open decrypts a record produced by Seal.
//
// context must match what was passed to Seal. A mismatch is reported as
// ErrNotAuthentic rather than as a distinct error, because a caller cannot act
// differently on it and an attacker should not learn which half was wrong.
func (r *Keyring) Open(stored []byte, context ...string) ([]byte, error) {
	var sealed record
	if err := json.Unmarshal(stored, &sealed); err != nil {
		return nil, fmt.Errorf("aead: decode record: %w", err)
	}
	if sealed.Schema != SchemaVersion {
		return nil, fmt.Errorf("%w: %d", ErrUnsupportedSchema, sealed.Schema)
	}
	if sealed.Algorithm != AlgorithmAESGCM {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedAlgorithm, sealed.Algorithm)
	}

	key, found := r.byID[sealed.KeyID]
	if !found {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKey, sealed.KeyID)
	}
	gcm, err := newGCM(key.Secret)
	if err != nil {
		return nil, err
	}
	if len(sealed.Nonce) != gcm.NonceSize() {
		return nil, fmt.Errorf("%w: nonce is %d bytes, expected %d",
			ErrNotAuthentic, len(sealed.Nonce), gcm.NonceSize())
	}

	plaintext, err := gcm.Open(nil, sealed.Nonce, sealed.Ciphertext, associatedData(context))
	if err != nil {
		// The underlying error is deliberately dropped: it distinguishes nothing
		// useful to a caller and anything it did distinguish would be a hint.
		return nil, ErrNotAuthentic
	}
	return plaintext, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aead: create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("aead: create GCM: %w", err)
	}
	return gcm, nil
}

// associatedData joins the context parts unambiguously.
//
// Each part is length-prefixed rather than concatenated or delimited. Plain
// concatenation would make {"ab", "c"} and {"a", "bc"} identical, so a record
// addressed to one delegation would authenticate against another; a delimiter
// would merely move the problem to whichever byte was chosen, since these parts
// are namespaces and provider names rather than values this package controls.
func associatedData(parts []string) []byte {
	if len(parts) == 0 {
		return nil
	}
	size := 0
	for _, part := range parts {
		size += 8 + len(part)
	}
	joined := make([]byte, 0, size)
	for _, part := range parts {
		joined = binary.BigEndian.AppendUint64(joined, uint64(len(part)))
		joined = append(joined, part...)
	}
	return joined
}
