package mountfinder

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type MockMountReader struct {
	lines []string
	err   error
}

func (m *MockMountReader) ReadMounts() ([]string, error) {
	return m.lines, m.err
}

// withMockReader swaps newSystemMountReader for the duration of the test.
func withMockReader(t *testing.T, reader MountReader) {
	t.Helper()
	orig := newSystemMountReader
	newSystemMountReader = func() MountReader { return reader }
	t.Cleanup(func() { newSystemMountReader = orig })
}

// withMockMountCmd swaps execMountCommand for the duration of the test.
func withMockMountCmd(t *testing.T, output []byte, err error) {
	t.Helper()
	orig := execMountCommand
	execMountCommand = func() ([]byte, error) { return output, err }
	t.Cleanup(func() { execMountCommand = orig })
}

// TestFindMountPathByName tests the findMountPathByName function
func TestFindMountPathByNameWithReader(t *testing.T) {

	t.Run("Successfully find mount path", func(t *testing.T) {
		mockReader := &MockMountReader{
			lines: []string{
				"/dev/sda1 / ext4 rw,relatime 0 0",
				"/dev/sda2 /run/csi/mount-root ext4 rw,relatime 0 0",
				"/dev/sda3 /home ext4 rw,relatime 0 0",
			},
			err: nil,
		}

		result, err := findMountPathByNameWithReader(mockReader, "mount-root")
		if err != nil {
			t.Errorf("Expected no error, but got: %v", err)
			return
		}

		expected := "/run/csi/mount-root"
		if result != expected {
			t.Errorf("Expected result: %s, but got: %s", expected, result)
		}
	})

	t.Run("Mount path not found", func(t *testing.T) {
		mockReader := &MockMountReader{
			lines: []string{
				"/dev/sda1 / ext4 rw,relatime 0 0",
				"/dev/sda3 /home ext4 rw,relatime 0 0",
			},
			err: nil,
		}

		result, err := findMountPathByNameWithReader(mockReader, "nonexistent_mount")
		if err == nil {
			t.Error("Expected error when mount path not found, but got nil")
			return
		}

		expectedMsg := "mount point containing 'nonexistent_mount' not found"
		if err.Error() != expectedMsg {
			t.Errorf("Expected error message: %s, but got: %s", expectedMsg, err.Error())
		}

		if result != "" {
			t.Errorf("Expected empty result when mount not found, but got: %s", result)
		}
	})

	t.Run("Reader returns error", func(t *testing.T) {
		mockReader := &MockMountReader{
			lines: nil,
			err:   fmt.Errorf("failed to read mounts"),
		}

		result, err := findMountPathByNameWithReader(mockReader, "any_mount")
		if err == nil {
			t.Error("Expected error when reader fails, but got nil")
			return
		}

		if err.Error() != "failed to read mounts" {
			t.Errorf("Expected error from reader, but got: %v", err)
		}

		if result != "" {
			t.Errorf("Expected empty result when reader fails, but got: %s", result)
		}
	})
}

// TestFindMountPathWithMountCmd tests the findMountPathWithMountCmd function
func TestFindMountPathWithMountCmd(t *testing.T) {
	tests := []struct {
		name        string
		output      string
		execErr     error
		mountName   string
		wantPath    string
		expectError string
	}{
		{
			name: "matching line returns first mount point",
			output: "/dev/sda1 on / type ext4 (rw,relatime)\n" +
				"/dev/sda2 on /run/csi/mount-root type ext4 (rw,relatime)\n",
			mountName: "mount-root",
			wantPath:  "/run/csi/mount-root",
		},
		{
			name:      "first matching line wins",
			output:    "/dev/sda1 on /a-mount type ext4 (rw)\n/dev/sda2 on /b-mount type ext4 (rw)\n",
			mountName: "mount",
			wantPath:  "/a-mount",
		},
		{
			name:        "exec error is propagated",
			execErr:     errors.New("mount: command not available"),
			mountName:   "any",
			expectError: "mount: command not available",
		},
		{
			name:        "no matching line returns not found",
			output:      "/dev/sda1 on / type ext4 (rw,relatime)\n",
			mountName:   "nonexistent_mount_point",
			expectError: "mount point 'nonexistent_mount_point' not found",
		},
		{
			name:        "line containing name but missing ' on ' separator is skipped",
			output:      "weird-mount-root-line-without-separator\n",
			mountName:   "mount-root",
			expectError: "mount point 'mount-root' not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withMockMountCmd(t, []byte(tt.output), tt.execErr)

			got, err := findMountPathWithMountCmd(tt.mountName)
			if tt.expectError == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got != tt.wantPath {
					t.Errorf("want path %q, got %q", tt.wantPath, got)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.expectError)
			}
			if !strings.Contains(err.Error(), tt.expectError) {
				t.Errorf("want error containing %q, got %q", tt.expectError, err.Error())
			}
			if got != "" {
				t.Errorf("want empty result on error, got %q", got)
			}
		})
	}
}

// TestCheckMountPathExists tests the checkMountPathExists function
func TestCheckMountPathExists(t *testing.T) {
	// Create a temporary directory for testing
	tempDir := t.TempDir()

	t.Run("Directory exists", func(t *testing.T) {
		// Test an existing directory
		existingDir := filepath.Join(tempDir, "existing_dir")
		err := os.Mkdir(existingDir, 0755)
		if err != nil {
			t.Fatalf("Failed to create test directory: %v", err)
		}

		result := checkMountPathExists(existingDir)
		if !result {
			t.Errorf("Expected directory %s to exist, but checkMountPathExists returned false", existingDir)
		}
	})

	t.Run("Directory does not exist", func(t *testing.T) {
		// Test a non-existent directory
		nonexistentDir := filepath.Join(tempDir, "nonexistent_dir")
		result := checkMountPathExists(nonexistentDir)
		if result {
			t.Errorf("Expected directory %s to not exist, but checkMountPathExists returned true", nonexistentDir)
		}
	})

	t.Run("Path is a file, not directory", func(t *testing.T) {
		// Test a file path (not a directory)
		filePath := filepath.Join(tempDir, "test_file.txt")
		err := os.WriteFile(filePath, []byte("test content"), 0644)
		if err != nil {
			t.Fatalf("Failed to create test file: %v", err)
		}

		result := checkMountPathExists(filePath)
		if result {
			t.Errorf("Expected file path %s to not be treated as directory, but checkMountPathExists returned true", filePath)
		}
	})
}

// TestFindMountPath tests the FindMountPath function across
// all four control-flow branches: reader-success / reader-fail-cmd-success
// / reader-fail-cmd-fail / path-existence-check.
func TestFindMountPath(t *testing.T) {
	tempDir := t.TempDir()
	existingDir := filepath.Join(tempDir, "mount-root")
	if err := os.Mkdir(existingDir, 0o755); err != nil {
		t.Fatalf("setup mkdir: %v", err)
	}
	missingPath := filepath.Join(tempDir, "missing-path")

	tests := []struct {
		name         string
		readerLines  []string
		readerErr    error
		cmdOutput    string
		cmdErr       error
		mountName    string
		wantPath     string
		expectError  string
	}{
		{
			name: "reader succeeds and path exists",
			readerLines: []string{
				"/dev/sda1 / ext4 rw,relatime 0 0",
				fmt.Sprintf("/dev/sda2 %s ext4 rw,relatime 0 0", existingDir),
			},
			mountName: existingDir,
			wantPath:  existingDir,
		},
		{
			name: "reader succeeds but reported path is not accessible",
			readerLines: []string{
				fmt.Sprintf("/dev/sda1 %s ext4 rw,relatime 0 0", missingPath),
			},
			mountName:   missingPath,
			expectError: "is not accessible",
		},
		{
			name:      "reader fails, mount cmd succeeds",
			readerErr: errors.New("open /proc/mounts: no such file"),
			cmdOutput: fmt.Sprintf("/dev/sda1 on %s type ext4 (rw,relatime)\n", existingDir),
			mountName: "mount-root",
			wantPath:  existingDir,
		},
		{
			name:        "reader fails, mount cmd also fails",
			readerErr:   errors.New("open /proc/mounts: no such file"),
			cmdErr:      errors.New("mount: command not available"),
			mountName:   "any",
			expectError: "mount: command not available",
		},
		{
			name:        "reader fails, mount cmd returns no match",
			readerErr:   errors.New("open /proc/mounts: no such file"),
			cmdOutput:   "/dev/sda1 on / type ext4 (rw)\n",
			mountName:   "any",
			expectError: "mount point 'any' not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withMockReader(t, &MockMountReader{lines: tt.readerLines, err: tt.readerErr})
			withMockMountCmd(t, []byte(tt.cmdOutput), tt.cmdErr)

			got, err := FindMountPath(tt.mountName, false)
			if tt.expectError == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got != tt.wantPath {
					t.Errorf("want path %q, got %q", tt.wantPath, got)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.expectError)
			}
			if !strings.Contains(err.Error(), tt.expectError) {
				t.Errorf("want error containing %q, got %q", tt.expectError, err.Error())
			}
		})
	}
}

// TestSystemMountReaderReadMounts exercises the production SystemMountReader
// against a transient absence of /proc/mounts (always the case on macOS).
// On Linux CI the file exists, so we accept either branch but assert that
// it never panics and that, when it succeeds, every line is non-empty.
func TestSystemMountReaderReadMounts(t *testing.T) {
	reader := &SystemMountReader{}
	lines, err := reader.ReadMounts()
	if err != nil {
		// macOS path: /proc/mounts does not exist; the error must be
		// propagated and lines must be nil.
		if lines != nil {
			t.Errorf("expected nil lines on error, got %v", lines)
		}
		if !strings.Contains(err.Error(), "/proc/mounts") {
			t.Errorf("expected error to mention /proc/mounts, got %v", err)
		}
		return
	}
	// Linux path: every returned line must be a valid mount entry.
	for i, line := range lines {
		if line == "" {
			t.Errorf("line %d is empty: %q", i, line)
		}
	}
}

// TestSystemMountReaderReadMountsWithFixture redirects procMountsPath at a
// temp fixture so the success branch is exercised on every platform,
// including macOS where /proc/mounts is absent. It also covers the
// not-found error branch via a missing path.
func TestSystemMountReaderReadMountsWithFixture(t *testing.T) {
	orig := procMountsPath
	t.Cleanup(func() { procMountsPath = orig })

	tests := []struct {
		name        string
		content     string
		setup       func(t *testing.T) string // returns the path to point procMountsPath at
		wantLines   []string
		expectError string
	}{
		{
			name: "reads every newline-delimited entry",
			setup: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), "mounts")
				if err := os.WriteFile(p, []byte("a / ext4 rw 0 0\nb /home ext4 rw 0 0\n"), 0o600); err != nil {
					t.Fatalf("write fixture: %v", err)
				}
				return p
			},
			wantLines: []string{"a / ext4 rw 0 0", "b /home ext4 rw 0 0"},
		},
		{
			name: "empty file yields nil slice and no error",
			setup: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), "mounts")
				if err := os.WriteFile(p, nil, 0o600); err != nil {
					t.Fatalf("write fixture: %v", err)
				}
				return p
			},
			wantLines: nil,
		},
		{
			name: "missing path propagates open error",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "does-not-exist")
			},
			expectError: "no such file",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			procMountsPath = tt.setup(t)

			reader := &SystemMountReader{}
			lines, err := reader.ReadMounts()

			if tt.expectError == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if len(lines) != len(tt.wantLines) {
					t.Fatalf("want %d lines, got %d (%v)", len(tt.wantLines), len(lines), lines)
				}
				for i, want := range tt.wantLines {
					if lines[i] != want {
						t.Errorf("line %d: want %q, got %q", i, want, lines[i])
					}
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.expectError)
			}
			if !strings.Contains(err.Error(), tt.expectError) {
				t.Errorf("want error containing %q, got %q", tt.expectError, err.Error())
			}
		})
	}
}
