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

// TestTheDocumentedRotationProcedure walks the three stages the guide gives,
// with real files, and checks the claim each stage rests on.
//
// This is the part of the guide an operator cannot safely get wrong: a key
// removed too early makes every record sealed under it permanently unreadable,
// which for a delegation means the user has to authorize again. The guide also
// promises the first two stages are reversible, which is only worth saying if
// it is true.
func TestTheDocumentedRotationProcedure(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl is not on PATH")
	}

	dir := t.TempDir()
	write := func(name string, content []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	randomKey := func() []byte {
		t.Helper()
		out, err := exec.Command("openssl", "rand", "32").Output()
		if err != nil {
			t.Fatalf("openssl rand 32: %v", err)
		}
		return out
	}
	const address = "sandbox-system"

	// Before: one key, sealing a delegation that must survive the rotation.
	write("2026-05", randomKey())
	write("active", []byte("2026-05"))

	before, err := loadEncryptionKeys(dir)
	if err != nil {
		t.Fatalf("load before rotation: %v", err)
	}
	legacy, err := before.Seal([]byte("sealed-before-rotation"), address, "alice", "github")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// Stage 1: add the new key file. active still names the old key.
	write("2026-11", randomKey())

	staged, err := loadEncryptionKeys(dir)
	if err != nil {
		t.Fatalf("load after stage 1: %v", err)
	}
	if got := staged.ActiveKeyID(); got != "2026-05" {
		t.Errorf("stage 1 changed the active key to %q; adding a file must change nothing", got)
	}
	if _, err := staged.Open(legacy, address, "alice", "github"); err != nil {
		t.Fatalf("stage 1 broke an existing record: %v", err)
	}

	// Stage 2: repoint active. New records seal under the new key, old records
	// still open because the old file is still present.
	write("active", []byte("2026-11"))

	rotated, err := loadEncryptionKeys(dir)
	if err != nil {
		t.Fatalf("load after stage 2: %v", err)
	}
	if got := rotated.ActiveKeyID(); got != "2026-11" {
		t.Errorf("stage 2 left the active key at %q", got)
	}
	if _, err := rotated.Open(legacy, address, "alice", "github"); err != nil {
		t.Fatalf("stage 2 broke a record sealed before the rotation: %v", err)
	}
	current, err := rotated.Seal([]byte("sealed-after-rotation"), address, "alice", "github")
	if err != nil {
		t.Fatalf("Seal after rotation: %v", err)
	}

	// The guide says stage 2 is reversible. Check that rather than assert it.
	write("active", []byte("2026-05"))
	reverted, err := loadEncryptionKeys(dir)
	if err != nil {
		t.Fatalf("load after reverting stage 2: %v", err)
	}
	if got := reverted.ActiveKeyID(); got != "2026-05" {
		t.Errorf("reverting stage 2 left the active key at %q", got)
	}
	if _, err := reverted.Open(current, address, "alice", "github"); err != nil {
		t.Errorf("reverting stage 2 orphaned a record sealed under the new key: %v", err)
	}
	write("active", []byte("2026-11"))

	// Stage 3 done too early: remove the old file while a record still needs
	// it. This has to fail, because the warning in the guide is only worth
	// printing if the consequence is real.
	if err := os.Remove(filepath.Join(dir, "2026-05")); err != nil {
		t.Fatalf("remove the old key: %v", err)
	}
	dropped, err := loadEncryptionKeys(dir)
	if err != nil {
		t.Fatalf("load after stage 3: %v", err)
	}
	if _, err := dropped.Open(legacy, address, "alice", "github"); err == nil {
		t.Error("a record sealed under the dropped key still opened, so the warning is wrong")
	}
	// And the records that did not need it are unaffected.
	if _, err := dropped.Open(current, address, "alice", "github"); err != nil {
		t.Errorf("stage 3 broke a record sealed under the active key: %v", err)
	}
}
