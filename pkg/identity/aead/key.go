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
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ActiveKeyName is the file naming which root key seals new records. Its
// contents are a key ID, not key material.
const ActiveKeyName = "active"

// LoadKeyringFromDir builds a Keyring from a directory of key files.
//
// Each file is one root key, named by its key ID, holding the raw key bytes.
// The ActiveKeyName file names which of them seals new records, so a rotation
// is a two-step an operator can stage: add the new key file and let it reach
// every replica, then repoint active at it, leaving the previous file in place
// so existing records still open.
//
//	active    -> "2026-10"
//	2026-10   -> <32 random bytes>
//	2026-04   -> <32 random bytes>   # retained, still opens older records
//
// The keys are read from files rather than from a Secret this process reads
// through the API, and that is a security requirement rather than a
// convenience. Session and delegation records live in Secrets in
// sandbox-system; if the root key were a Secret there too, one RBAC grant over
// Secrets in that namespace would yield both the ciphertext and the key it is
// encrypted under, which is no encryption at all. Keeping the key out of the
// API server entirely is what separates the two.
//
// How the files arrive is deliberately out of scope. A projected volume, a CSI
// secret driver and a KMS sidecar all present a directory of files, and the
// identity provider is assumed to run somewhere that can be trusted with one.
//
// A directory rather than a single file because the keyring is plural by
// design: retaining the superseded key is the whole reason rotation does not
// orphan existing records.
func LoadKeyringFromDir(dir string) (*Keyring, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("aead: read root key directory: %w", err)
	}

	contents := make(map[string][]byte, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		// A projected volume keeps its real data in a timestamped directory and
		// presents every key as a symlink beside a "..data" link. Those
		// bookkeeping entries are not keys, and kubelet's own contract is that
		// consumers ignore anything prefixed with "..".
		if strings.HasPrefix(name, "..") {
			continue
		}
		if entry.IsDir() {
			continue
		}
		// #nosec G304 -- operator-configured key mount path
		value, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("aead: read root key %q: %w", name, err)
		}
		contents[name] = value
	}

	return keyringFromFiles(contents)
}

// keyringFromFiles builds a Keyring from file names mapped to their contents.
//
// Split from LoadKeyringFromDir so the validation can be exercised without a
// directory, and kept unexported because a caller handing this a map it built
// itself would be reintroducing the key source the comment above rules out.
func keyringFromFiles(data map[string][]byte) (*Keyring, error) {
	if len(data) == 0 {
		return nil, errors.New("aead: root key directory is empty")
	}

	activeID := string(data[ActiveKeyName])
	if activeID == "" {
		return nil, fmt.Errorf("aead: root key directory has no %q file naming the active key", ActiveKeyName)
	}
	if activeID == ActiveKeyName {
		return nil, fmt.Errorf("aead: %q cannot name itself as the active key", ActiveKeyName)
	}

	activeSecret, found := data[activeID]
	if !found {
		return nil, fmt.Errorf("aead: active key %q has no file in the root key directory", activeID)
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
