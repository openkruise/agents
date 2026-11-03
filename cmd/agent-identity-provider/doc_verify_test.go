package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestTheDocumentedCommandsProduceALoadableKeyDirectory runs the commands the
// encryption key guide gives an operator and loads the result.
//
// A guide is a set of claims. An operator following a command that produces a
// key the provider refuses has no way to tell whose fault it is, and the
// likeliest mistake here is a trailing newline, which echo adds and openssl
// rand does not.
func TestTheDocumentedCommandsProduceALoadableKeyDirectory(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl is not on PATH")
	}
	dir := t.TempDir()

	for _, keyID := range []string{"2026-11", "2026-05"} {
		out, err := exec.Command("openssl", "rand", "32").Output()
		if err != nil {
			t.Fatalf("openssl rand 32: %v", err)
		}
		if len(out) != 32 {
			t.Fatalf("openssl rand 32 produced %d bytes, want 32", len(out))
		}
		if err := os.WriteFile(filepath.Join(dir, keyID), out, 0o600); err != nil {
			t.Fatalf("write %s: %v", keyID, err)
		}
	}
	// printf, not echo: echo appends a newline and the key ID would not match
	// any file.
	if err := os.WriteFile(filepath.Join(dir, "active"), []byte("2026-11"), 0o600); err != nil {
		t.Fatalf("write active: %v", err)
	}

	keyring, err := loadEncryptionKeys(dir)
	if err != nil {
		t.Fatalf("the provider refused a directory built from the guide: %v", err)
	}
	if got := keyring.ActiveKeyID(); got != "2026-11" {
		t.Errorf("active key is %q, want 2026-11", got)
	}
	sealed, err := keyring.Seal([]byte("refresh-token"), "sandbox-system", "alice", "github")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := keyring.Open(sealed, "sandbox-system", "alice", "github"); err != nil {
		t.Fatalf("Open: %v", err)
	}
}

// TestATrailingNewlineIsRejected pins the troubleshooting row about echo. The
// failure has to be loud, because a 33-byte key silently truncated or padded
// would be far worse than one refused.
func TestATrailingNewlineIsRejected(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl is not on PATH")
	}
	dir := t.TempDir()

	out, err := exec.Command("openssl", "rand", "32").Output()
	if err != nil {
		t.Fatalf("openssl rand 32: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "2026-11"), append(out, '\n'), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	// What echo would have produced.
	if err := os.WriteFile(filepath.Join(dir, "active"), []byte("2026-11\n"), 0o600); err != nil {
		t.Fatalf("write active: %v", err)
	}

	if _, err := loadEncryptionKeys(dir); err == nil {
		t.Fatal("a key with a trailing newline was accepted")
	}
}
