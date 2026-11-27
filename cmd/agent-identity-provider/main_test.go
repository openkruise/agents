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

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openkruise/agents/pkg/identity/aead"
)

// writeKeyDir lays a key set out the way a mounted volume presents it.
func writeKeyDir(t *testing.T, files map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	for name, value := range files {
		if err := os.WriteFile(filepath.Join(dir, name), value, 0o600); err != nil {
			t.Fatalf("write %q: %v", name, err)
		}
	}
	return dir
}

// TestLoadEncryptionKeys covers what an operator can get wrong in the mount,
// and the one case that must not be treated as an error.
//
// The failures matter because they decide whether the process starts. A
// malformed key directory has to stop it here, since the alternative is a
// provider that comes up healthy and then cannot serve the first delegation a
// user tries to establish.
func TestLoadEncryptionKeys(t *testing.T) {
	good := bytes.Repeat([]byte{0x11}, aead.KeySize)
	previous := bytes.Repeat([]byte{0x22}, aead.KeySize)

	tests := []struct {
		name string
		// files is nil when the flag is left empty rather than pointing at a
		// directory.
		files        map[string][]byte
		unset        bool
		wantKeyring  bool
		wantActiveID string
		wantErr      string
	}{
		{
			// Credential storage is opt-in, the same shape as the signing key.
			// A deployment that has not reached the credential flow must not
			// have to invent a key to start.
			name:  "no directory configured",
			unset: true,
		},
		{
			name: "one active key",
			files: map[string][]byte{
				aead.ActiveKeyName: []byte("2026-11"),
				"2026-11":          good,
			},
			wantKeyring:  true,
			wantActiveID: "2026-11",
		},
		{
			// Mid-rotation: the superseded key stays so records sealed under it
			// still open.
			name: "an active key alongside a retained one",
			files: map[string][]byte{
				aead.ActiveKeyName: []byte("2026-11"),
				"2026-11":          good,
				"2026-05":          previous,
			},
			wantKeyring:  true,
			wantActiveID: "2026-11",
		},
		{
			name: "no active file",
			files: map[string][]byte{
				"2026-11": good,
			},
			wantErr: "naming the active key",
		},
		{
			name: "active naming a key with no file",
			files: map[string][]byte{
				aead.ActiveKeyName: []byte("2026-12"),
				"2026-11":          good,
			},
			wantErr: "has no file",
		},
		{
			name: "a key of the wrong length",
			files: map[string][]byte{
				aead.ActiveKeyName: []byte("2026-11"),
				"2026-11":          []byte("short"),
			},
			wantErr: "must be 32 bytes",
		},
		{
			name:    "an empty directory",
			files:   map[string][]byte{},
			wantErr: "empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := ""
			if !tt.unset {
				dir = writeKeyDir(t, tt.files)
			}

			keyring, err := loadEncryptionKeys(dir)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error: got %q, want it to contain %q", err, tt.wantErr)
				}
				if keyring != nil {
					t.Error("a keyring was returned alongside the error")
				}
				return
			}

			if err != nil {
				t.Fatalf("loadEncryptionKeys: %v", err)
			}
			if !tt.wantKeyring {
				if keyring != nil {
					t.Error("a keyring was built even though no directory was configured")
				}
				return
			}
			if keyring == nil {
				t.Fatal("no keyring was built")
			}
			if got := keyring.ActiveKeyID(); got != tt.wantActiveID {
				t.Errorf("active key: got %q, want %q", got, tt.wantActiveID)
			}

			// Loading is not the point; sealing is. A keyring that loads but
			// cannot round trip would pass every check above.
			sealed, err := keyring.Seal([]byte("refresh-token"), "sandbox-system", "alice", "github")
			if err != nil {
				t.Fatalf("Seal: %v", err)
			}
			opened, err := keyring.Open(sealed, "sandbox-system", "alice", "github")
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if string(opened) != "refresh-token" {
				t.Errorf("round trip returned %q", opened)
			}
		})
	}
}

// TestLoadEncryptionKeysRejectsAMissingDirectory separates the two ways a mount
// can be absent. An empty flag means storage is switched off; a flag pointing
// at nothing means the mount did not arrive, which is a misconfiguration and
// has to stop the process.
func TestLoadEncryptionKeysRejectsAMissingDirectory(t *testing.T) {
	keyring, err := loadEncryptionKeys(filepath.Join(t.TempDir(), "never-mounted"))
	if err == nil {
		t.Fatal("a missing directory was treated as success")
	}
	if keyring != nil {
		t.Error("a keyring was returned alongside the error")
	}
}
