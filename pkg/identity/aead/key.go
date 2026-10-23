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
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sort"
)

// ActiveKeyName is the Secret key naming which root key seals new records.
// Its value is a key ID, not key material.
const ActiveKeyName = "active"

// KeyringFromSecret builds a Keyring from the data of the identity provider's
// root key Secret.
//
// Every entry except ActiveKeyName is a root key, with the Secret's key used as
// the key ID and its value as the key material. The ActiveKeyName entry names
// which of them seals new records. A rotation is therefore a two-step the
// operator can stage: add the new key and let it replicate, then repoint active
// at it, leaving the previous key present so existing records still open.
//
//	active:   "2026-10"
//	2026-10:  <32 random bytes>
//	2026-04:  <32 random bytes>   # retained, still opens older records
//
// The Secret holds raw bytes, so the values are the keys themselves rather than
// an encoding of them. Kubernetes base64s Secret data in transit and on the
// wire, which is not encryption and is not relied on here for anything.
func KeyringFromSecret(data map[string][]byte) (*Keyring, error) {
	if len(data) == 0 {
		return nil, errors.New("aead: root key Secret is empty")
	}

	activeID := string(data[ActiveKeyName])
	if activeID == "" {
		return nil, fmt.Errorf("aead: root key Secret has no %q entry naming the active key", ActiveKeyName)
	}
	if activeID == ActiveKeyName {
		return nil, fmt.Errorf("aead: %q cannot name itself as the active key", ActiveKeyName)
	}

	activeSecret, found := data[activeID]
	if !found {
		return nil, fmt.Errorf("aead: active key %q has no entry in the root key Secret", activeID)
	}

	// Sorted so a malformed retained key is always reported by the same name,
	// rather than whichever one map iteration reached first.
	retainedIDs := make([]string, 0, len(data))
	for id := range data {
		if id == ActiveKeyName || id == activeID {
			continue
		}
		retainedIDs = append(retainedIDs, id)
	}
	sort.Strings(retainedIDs)

	retained := make([]Key, 0, len(retainedIDs))
	for _, id := range retainedIDs {
		retained = append(retained, Key{ID: id, Secret: data[id]})
	}

	return NewKeyring(Key{ID: activeID, Secret: activeSecret}, retained...)
}

// NewKey returns a root key with freshly generated material, for operators
// creating or rotating the Secret.
//
// Errors carry no key material, since anything returned here can reach a log.
func NewKey(id string) (Key, error) {
	if id == "" {
		return Key{}, errors.New("aead: key ID must not be empty")
	}
	secret := make([]byte, KeySize)
	if _, err := io.ReadFull(rand.Reader, secret); err != nil {
		return Key{}, fmt.Errorf("aead: generate key: %w", err)
	}
	return Key{ID: id, Secret: secret}, nil
}
