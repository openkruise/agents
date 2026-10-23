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
	"strings"
	"testing"
)

func TestKeyringFromSecret(t *testing.T) {
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
			name:    "an empty Secret",
			data:    map[string][]byte{},
			wantErr: "root key Secret is empty",
		},
		{
			name:    "no active entry",
			data:    map[string][]byte{"2026-10": current.Secret},
			wantErr: "has no \"active\" entry",
		},
		{
			name: "an active entry naming nothing",
			data: map[string][]byte{
				ActiveKeyName: []byte(""),
				"2026-10":     current.Secret,
			},
			wantErr: "has no \"active\" entry",
		},
		{
			// Would otherwise treat the pointer as its own key material.
			name: "an active entry naming itself",
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
			wantErr: "active key \"2026-11\" has no entry",
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
			keyring, err := KeyringFromSecret(tt.data)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error: got %q, want it to contain %q", err, tt.wantErr)
				}
				if keyring != nil {
					t.Error("a rejected Secret must not produce a keyring")
				}
				return
			}

			if err != nil {
				t.Fatalf("KeyringFromSecret: %v", err)
			}
			if got := keyring.ActiveKeyID(); got != tt.wantActiveID {
				t.Errorf("active key: got %q, want %q", got, tt.wantActiveID)
			}
		})
	}
}

// TestKeyringFromSecretSurvivesRotation walks the two-step rotation the Secret
// layout is designed for: add the new key, then repoint active at it, with the
// previous key left in place so existing records still open.
func TestKeyringFromSecretSurvivesRotation(t *testing.T) {
	previous := mustKey(t, "2026-04")
	current := mustKey(t, "2026-10")
	context := delegationContext("team-a", "principal-1", "sample-agent", "github-3lo")

	before := map[string][]byte{
		ActiveKeyName: []byte("2026-04"),
		"2026-04":     previous.Secret,
	}
	original, err := KeyringFromSecret(before)
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
	stagedKeyring, err := KeyringFromSecret(staged)
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
	rotatedKeyring, err := KeyringFromSecret(rotated)
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
