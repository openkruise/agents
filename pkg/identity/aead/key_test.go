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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadKeyringFromDir(t *testing.T) {
	current := mustKey(t, "2026-10")
	previous := mustKey(t, "2026-04")

	tests := []struct {
		name         string
		data         map[string][]byte
		wantActiveID string
		wantErr      string
	}{
		{
			name: "one active key",
			data: map[string][]byte{
				ActiveKeyName: []byte("2026-10"),
				"2026-10":     current.Secret,
			},
			wantActiveID: "2026-10",
		},
		{
			name: "an active key alongside a retained one",
			data: map[string][]byte{
				ActiveKeyName: []byte("2026-10"),
				"2026-10":     current.Secret,
				"2026-04":     previous.Secret,
			},
			wantActiveID: "2026-10",
		},
		{
			name:    "an empty directory",
			data:    map[string][]byte{},
			wantErr: "root key directory is empty",
		},
		{
			name:    "no active file",
			data:    map[string][]byte{"2026-10": current.Secret},
			wantErr: "has no \"active\" file",
		},
		{
			name: "an active file naming nothing",
			data: map[string][]byte{
				ActiveKeyName: []byte(""),
				"2026-10":     current.Secret,
			},
			wantErr: "has no \"active\" file",
		},
		{
			// Would otherwise treat the pointer as its own key material.
			name: "an active file naming itself",
			data: map[string][]byte{
				ActiveKeyName: []byte(ActiveKeyName),
			},
			wantErr: "cannot name itself",
		},
		{
			name: "an active key with no material",
			data: map[string][]byte{
				ActiveKeyName: []byte("2026-11"),
				"2026-10":     current.Secret,
			},
			wantErr: "active key \"2026-11\" has no file",
		},
		{
			name: "a retained key of the wrong length",
			data: map[string][]byte{
				ActiveKeyName: []byte("2026-10"),
				"2026-10":     current.Secret,
				"2026-04":     make([]byte, 16),
			},
			wantErr: "retained key \"2026-04\"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keyring, err := LoadKeyringFromDir(writeKeyDir(t, tt.data))

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error: got %q, want it to contain %q", err, tt.wantErr)
				}
				if keyring != nil {
					t.Error("a rejected key directory must not produce a keyring")
				}
				return
			}

			if err != nil {
				t.Fatalf("LoadKeyringFromDir: %v", err)
			}
			if got := keyring.ActiveKeyID(); got != tt.wantActiveID {
				t.Errorf("active key: got %q, want %q", got, tt.wantActiveID)
			}
		})
	}
}

// TestLoadKeyringFromDirSurvivesRotation walks the two-step rotation the key
// layout is designed for: add the new key, then repoint active at it, with the
// previous key left in place so existing records still open.
func TestLoadKeyringFromDirSurvivesRotation(t *testing.T) {
	previous := mustKey(t, "2026-04")
	current := mustKey(t, "2026-10")
	context := delegationContext("team-a", "principal-1", "sample-agent", "github-3lo")

	before := map[string][]byte{
		ActiveKeyName: []byte("2026-04"),
		"2026-04":     previous.Secret,
	}
	original, err := LoadKeyringFromDir(writeKeyDir(t, before))
	if err != nil {
		t.Fatalf("before rotation: %v", err)
	}
	stored, err := original.Seal([]byte("gho_token"), context...)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// Step one: the new key is added but not yet active, so nothing changes.
	staged := map[string][]byte{
		ActiveKeyName: []byte("2026-04"),
		"2026-04":     previous.Secret,
		"2026-10":     current.Secret,
	}
	stagedKeyring, err := LoadKeyringFromDir(writeKeyDir(t, staged))
	if err != nil {
		t.Fatalf("staged: %v", err)
	}
	if got := stagedKeyring.ActiveKeyID(); got != "2026-04" {
		t.Errorf("staging must not change the active key, got %q", got)
	}

	// Step two: active is repointed, and the old record still opens.
	rotated := map[string][]byte{
		ActiveKeyName: []byte("2026-10"),
		"2026-04":     previous.Secret,
		"2026-10":     current.Secret,
	}
	rotatedKeyring, err := LoadKeyringFromDir(writeKeyDir(t, rotated))
	if err != nil {
		t.Fatalf("rotated: %v", err)
	}
	if got := rotatedKeyring.ActiveKeyID(); got != "2026-10" {
		t.Errorf("active key after rotation: got %q, want %q", got, "2026-10")
	}
	opened, err := rotatedKeyring.Open(stored, context...)
	if err != nil {
		t.Fatalf("a record from before the rotation must still open: %v", err)
	}
	if string(opened) != "gho_token" {
		t.Errorf("round trip across rotation: got %q", opened)
	}
}

// writeKeyDir lays a key set out as files, the way a projected volume or a
// secret driver would present it.
func writeKeyDir(t *testing.T, data map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	for name, value := range data {
		if err := os.WriteFile(filepath.Join(dir, name), value, 0o600); err != nil {
			t.Fatalf("write key file %q: %v", name, err)
		}
	}
	return dir
}

// TestLoadKeyringFromDirIgnoresProjectedVolumeBookkeeping covers the layout a
// Kubernetes projected volume actually produces.
//
// kubelet writes the real data into a timestamped directory and presents each
// key as a symlink beside a "..data" link. Those entries are not keys, and a
// loader that treated them as keys would fail on every real mount while
// passing every test built from a flat directory.
func TestLoadKeyringFromDirIgnoresProjectedVolumeBookkeeping(t *testing.T) {
	current := mustKey(t, "2026-10")
	dir := t.TempDir()

	timestamped := filepath.Join(dir, "..2026_10_30_12_00_00.123456789")
	if err := os.MkdirAll(timestamped, 0o700); err != nil {
		t.Fatalf("create the timestamped directory: %v", err)
	}
	for name, value := range map[string][]byte{
		ActiveKeyName: []byte("2026-10"),
		"2026-10":     current.Secret,
	} {
		if err := os.WriteFile(filepath.Join(timestamped, name), value, 0o600); err != nil {
			t.Fatalf("write %q: %v", name, err)
		}
		// The visible entry. A real mount symlinks it; a plain copy exercises
		// the same loader path on platforms where symlinks need privileges.
		if err := os.WriteFile(filepath.Join(dir, name), value, 0o600); err != nil {
			t.Fatalf("write the visible %q: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "..data"), []byte("bookkeeping"), 0o600); err != nil {
		t.Fatalf("write the data link: %v", err)
	}

	keyring, err := LoadKeyringFromDir(dir)
	if err != nil {
		t.Fatalf("LoadKeyringFromDir over a projected layout: %v", err)
	}
	if got := keyring.ActiveKeyID(); got != "2026-10" {
		t.Errorf("active key: got %q, want %q", got, "2026-10")
	}

	// And it must actually work, not merely load.
	stored, err := keyring.Seal([]byte("token"), "team-a")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := keyring.Open(stored, "team-a"); err != nil {
		t.Fatalf("Open: %v", err)
	}
}

// TestLoadKeyringFromDirRejectsAMissingDirectory covers the misconfiguration an
// operator is most likely to hit: the mount is not there at all. Failing at
// startup is the point, since the alternative is discovering it on the first
// credential request.
func TestLoadKeyringFromDirRejectsAMissingDirectory(t *testing.T) {
	keyring, err := LoadKeyringFromDir(filepath.Join(t.TempDir(), "absent"))
	if err == nil {
		t.Fatal("a missing key directory produced a keyring")
	}
	if keyring != nil {
		t.Error("a keyring was returned alongside the error")
	}
}
