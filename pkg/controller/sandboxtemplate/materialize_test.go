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

package sandboxtemplate

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"

	api "github.com/openkruise/agents/api/v1alpha1"
)

func TestBuildCOPYCommand(t *testing.T) {
	for _, tt := range []struct {
		name   string
		source string
		dest   string
		user   string
		mode   *int32
		want   string
	}{
		{name: "file", source: "file.txt", dest: "/app/file.txt", want: "sourcePrefix='/file.txt'"},
		{name: "directory", source: "tree", dest: "/app/", want: "sourcePrefix='/tree'"},
		{name: "root glob", source: "*.py", dest: "/app", want: "sourcePrefix=''"},
		{name: "nested glob", source: "tree/*.py", dest: "/app", want: "sourcePrefix='/tree'"},
		{name: "explicit zero permissions", source: ".", dest: "/app", mode: ptr.To(int32(0)), want: "permissions='0000'"},
		{name: "permissions", source: "file", dest: "/app", mode: ptr.To(int32(0755)), want: "permissions='0755'"},
		{name: "shell quoting", source: "a' $() ; file", dest: "/app/a' $() ; file", user: "user' $() ;", want: "'\\''"},
		{name: "explicit group", source: "file", dest: "/app/file", user: "user:group", want: "owner='user:group'"},
		{name: "bare user resolves its primary group", source: "file", dest: "/app/file", user: "app", want: "owner='app'"},
		{name: "default owner stays root", source: "file", dest: "/app/file", want: "owner='root'"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			command, err := materializeCommand(&api.TemplateBuildCopy{FilesHash: strings.Repeat("a", 64), Src: tt.source, Dest: tt.dest, User: tt.user, Mode: tt.mode})
			require.NoError(t, err)
			require.Equal(t, "root", command.User)
			require.Equal(t, "/", command.Cwd)
			require.Contains(t, command.Command, tt.want)
			require.Contains(t, command.Command, "tar -xzf")
			require.Contains(t, command.Command, "--keep-directory-symlink --no-overwrite-dir")
			check := exec.Command("/bin/bash", "-n")
			check.Stdin = strings.NewReader(command.Command)
			output, err := check.CombinedOutput()
			require.NoError(t, err, string(output))
		})
	}
}

func TestBuildCOPYCleanup(t *testing.T) {
	for _, tt := range []struct {
		name       string
		chmodFails bool
		exitCode   int
	}{
		{name: "successful command"},
		{name: "failed command", exitCode: 7},
		{name: "permission reset fails", chmodFails: true},
		{name: "command and permission reset fail", chmodFails: true, exitCode: 7},
	} {
		t.Run(tt.name, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "copy")
			require.NoError(t, os.Mkdir(directory, 0755))
			require.NoError(t, os.WriteFile(filepath.Join(directory, "file"), []byte("content"), 0644))
			command, err := materializeCommand(&api.TemplateBuildCopy{FilesHash: strings.Repeat("a", 64), Src: "file", Dest: "/app/file"})
			require.NoError(t, err)
			prefix, _, found := strings.Cut(command.Command, "mkdir -p -- \"$temp/unpack\"\n")
			require.True(t, found)
			prefix = strings.Replace(prefix, "temp=$(mktemp -d /tmp/template-copy.XXXXXXXX)", "temp="+shellQuote(directory), 1)
			// BSD chmod does not accept GNU's post-mode option terminator.
			prefix = strings.Replace(prefix, "chmod -R u+rwx -- ", "chmod -R u+rwx ", 1)
			if tt.chmodFails {
				prefix = "chmod() { return 1; }\n" + prefix
			}
			output, err := exec.CommandContext(t.Context(), "/bin/bash", "-c", fmt.Sprintf("%s\nexit %d\n", prefix, tt.exitCode)).CombinedOutput()
			if tt.exitCode == 0 {
				require.NoError(t, err, string(output))
			} else {
				var exitError *exec.ExitError
				require.ErrorAs(t, err, &exitError)
				require.Equal(t, tt.exitCode, exitError.ExitCode(), string(output))
			}
			_, err = os.Stat(directory)
			require.ErrorIs(t, err, os.ErrNotExist)
			if tt.chmodFails {
				require.Contains(t, string(output), "COPY cleanup could not reset temporary directory permissions")
			} else {
				require.Empty(t, string(output))
			}
		})
	}
}

func TestBuildCOPYRejectsInvalidInput(t *testing.T) {
	for _, tt := range []struct {
		name   string
		source string
		dest   string
		hash   string
	}{
		{name: "absolute source", source: "/file", dest: "/app", hash: strings.Repeat("a", 64)},
		{name: "escaped source", source: "../file", dest: "/app", hash: strings.Repeat("a", 64)},
		{name: "relative destination", source: "file", dest: "app", hash: strings.Repeat("a", 64)},
		{name: "bad archive key", source: "file", dest: "/app", hash: "bad"},
		{name: "nonhex archive key", source: "file", dest: "/app", hash: strings.Repeat("g", 64)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := materializeCommand(&api.TemplateBuildCopy{FilesHash: tt.hash, Src: tt.source, Dest: tt.dest})
			require.Error(t, err)
		})
	}
}

func gnuTarBinary() string {
	for _, name := range []string{"gtar", "tar"} {
		candidate, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		version, err := exec.Command(candidate, "--version").Output()
		if err == nil && strings.Contains(string(version), "GNU tar") {
			return candidate
		}
	}
	return ""
}

func writeCopyArchive(t *testing.T, archive string) {
	t.Helper()
	file, err := os.Create(archive)
	require.NoError(t, err)
	compressed := gzip.NewWriter(file)
	writer := tar.NewWriter(compressed)
	require.NoError(t, writer.WriteHeader(&tar.Header{Name: "tree/file", Mode: 0644, Size: 7}))
	_, err = writer.Write([]byte("content"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	require.NoError(t, compressed.Close())
	require.NoError(t, file.Close())
}

func TestBuildCOPYFilesystem(t *testing.T) {
	binary := gnuTarBinary()
	if binary == "" {
		if runtime.GOOS == "linux" {
			t.Fatal("GNU tar is required by the runtime COPY script")
		}
		t.Skip("COPY filesystem verification requires GNU tar")
	}
	for _, tt := range []struct {
		name   string
		source string
		mode   *int32
	}{
		{name: "file rename", source: "tree/file"},
		{name: "directory merge", source: "tree"},
		{name: "symlink", source: "tree/link"},
		{name: "zero mode", source: "tree/file", mode: ptr.To(int32(0))},
		{name: "missing source", source: "missing"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			archive := filepath.Join(root, "archive.tar.gz")
			file, err := os.Create(archive)
			require.NoError(t, err)
			compressed := gzip.NewWriter(file)
			writer := tar.NewWriter(compressed)
			for _, header := range []tar.Header{{Name: "tree", Typeflag: tar.TypeDir, Mode: 0755}, {Name: "tree/file", Mode: 0644, Size: 7}, {Name: "tree/link", Typeflag: tar.TypeSymlink, Linkname: "file", Mode: 0777}} {
				require.NoError(t, writer.WriteHeader(&header))
				if header.Size > 0 {
					_, err := writer.Write([]byte("content"))
					require.NoError(t, err)
				}
			}
			require.NoError(t, writer.Close())
			require.NoError(t, compressed.Close())
			require.NoError(t, file.Close())
			destination := filepath.Join(root, "destination ' quoted")
			if tt.name == "directory merge" {
				require.NoError(t, os.Mkdir(destination, 0755))
				require.NoError(t, os.WriteFile(filepath.Join(destination, "existing"), []byte("preserved"), 0644))
			}
			hash := strings.Repeat("a", 64)
			command, err := materializeCommand(&api.TemplateBuildCopy{FilesHash: hash, Src: tt.source, Dest: destination, User: fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), Mode: tt.mode})
			require.NoError(t, err)
			command.Command = strings.Replace(command.Command, shellQuote("/blobs/"+hash+".tar.gz"), shellQuote(archive), 1)
			command.Command = strings.ReplaceAll(command.Command, "tar -", shellQuote(binary)+" -")
			output, err := exec.CommandContext(t.Context(), "/bin/bash", "-c", command.Command).CombinedOutput()
			if tt.name == "missing source" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err, string(output))
			switch tt.name {
			case "directory merge":
				data, err := os.ReadFile(filepath.Join(destination, "existing"))
				require.NoError(t, err)
				require.Equal(t, "preserved", string(data))
				data, err = os.ReadFile(filepath.Join(destination, "file"))
				require.NoError(t, err)
				require.Equal(t, "content", string(data))
			case "symlink":
				target, err := os.Readlink(destination)
				require.NoError(t, err)
				require.Equal(t, "file", target)
			case "zero mode":
				info, err := os.Stat(destination)
				require.NoError(t, err)
				require.Zero(t, info.Mode().Perm())
			default:
				data, err := os.ReadFile(destination)
				require.NoError(t, err)
				require.Equal(t, "content", string(data))
			}
		})
	}
}

func TestBuildCOPYOwnerNormalization(t *testing.T) {
	binary := gnuTarBinary()
	if binary == "" {
		if runtime.GOOS == "linux" {
			t.Fatal("GNU tar is required by the runtime COPY script")
		}
		t.Skip("COPY owner verification requires GNU tar")
	}
	for _, tt := range []struct {
		user     string
		expected string
	}{
		{user: "1000", expected: "1000:1000"},
		{user: "1000:", expected: "1000:1000"},
		{user: "app", expected: "app:543"},
		{user: "app:", expected: "app:543"},
		{user: "app:group", expected: "app:group"},
		{user: "1000:1001", expected: "1000:1001"},
	} {
		t.Run(tt.user, func(t *testing.T) {
			root := t.TempDir()
			archive := filepath.Join(root, "archive.tar.gz")
			writeCopyArchive(t, archive)
			destination := filepath.Join(root, "destination")
			hash := strings.Repeat("a", 64)
			command, err := materializeCommand(&api.TemplateBuildCopy{FilesHash: hash, Src: "tree/file", Dest: destination, User: tt.user})
			require.NoError(t, err)
			command.Command = strings.Replace(command.Command, shellQuote("/blobs/"+hash+".tar.gz"), shellQuote(archive), 1)
			command.Command = strings.ReplaceAll(command.Command, "tar -", shellQuote(binary)+" -")
			// chown and id are stubbed so ownership resolution is asserted
			// without privileges: the numeric path must not consult id, and
			// the named path resolves through the stubbed primary group.
			command.Command = "chown() { printf 'CHOWN %s\\n' \"$*\"; }\nid() { printf '543\\n'; }\n" + command.Command
			output, err := exec.CommandContext(t.Context(), "/bin/bash", "-c", command.Command).CombinedOutput()
			require.NoError(t, err, string(output))
			require.Contains(t, string(output), "CHOWN -- "+tt.expected+" ")
		})
	}
}
