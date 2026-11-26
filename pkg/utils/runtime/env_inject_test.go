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

package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/runtime/config"
	"github.com/openkruise/agents/proto/envd/process"
	"github.com/openkruise/agents/proto/envd/process/processconnect"
)

func TestDeliveryReleasesActualWrapper(t *testing.T) {
	for _, claimCommand := range []bool{false, true} {
		t.Run(fmt.Sprintf("claim-command=%t", claimCommand), func(t *testing.T) {
			dir := t.TempDir()
			box := newStartupDeliveryTestServer(t, dir, nil, nil)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			wrapper, err := filepath.Abs("../../agent-runtime/run_with_envs.sh")
			require.NoError(t, err)
			cmd := exec.CommandContext(ctx, "bash", wrapper, "--", "bash", "-c", `printf '%s' "template:$VALUE"`)
			cmd.Env = append(os.Environ(), "SANDBOX_ENV_DIR="+dir, "SANDBOX_POD_UID=pod-uid")
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			require.NoError(t, cmd.Start())
			t.Cleanup(func() {
				if cmd.ProcessState == nil {
					err := cmd.Process.Kill()
					if !errors.Is(err, os.ErrProcessDone) {
						assert.NoError(t, err)
					}
					assert.Error(t, cmd.Wait())
				}
			})
			box.Status.PodInfo.PodUID = "pod-uid"
			data := config.StartupData{EnvVars: map[string]string{"VALUE": "a b'c"}}
			want := "template:a b'c"
			if claimCommand {
				data.Command = []string{"bash", "-c", `printf '%s' "claim:$VALUE"`}
				want = "claim:a b'c"
			}
			require.NoError(t, InjectStartupData(ctx, box, data))
			require.NoError(t, cmd.Wait())
			assert.True(t, strings.HasSuffix(output.String(), want), "%s", output.String())
		})
	}
}

func TestStartupDataDoesNotChangeInitWireProtocol(t *testing.T) {
	for _, skip := range []bool{false, true} {
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			assert.Equal(t, "/init", r.URL.Path)
			assert.NotContains(t, body, "command")
			assert.NotContains(t, body, "skipInit")
			assert.Contains(t, body, "envVars")
			w.WriteHeader(http.StatusNoContent)
		}))
		box := sandboxWithURL(server.URL)
		request := config.InitRuntimeOptions{SkipInit: skip, EnvVars: map[string]string{"VALUE": "claim"}, Command: []string{"printf", "claim"}}
		_, err := InitRuntime(t.Context(), box, request, nil)
		require.NoError(t, err)
		if skip {
			assert.Zero(t, calls.Load())
		} else {
			assert.EqualValues(t, 1, calls.Load())
		}
		assert.Equal(t, []string{"printf", "claim"}, request.Command)
		server.Close()
	}
}

func TestValidateStartupData(t *testing.T) {
	for _, key := range []string{"A.B", "A-B", "1A", "PATH", "UID", "EUID", "BASH_ENV", "SHELLOPTS", "_rwe_cmd_script", "SANDBOX_POD_UID"} {
		t.Run(key, func(t *testing.T) {
			require.Error(t, ValidateStartupData(config.StartupData{EnvVars: map[string]string{key: "value"}}))
		})
	}
	envs := func(n int, value string) map[string]string {
		out := make(map[string]string, n)
		for i := 0; i < n; i++ {
			out[fmt.Sprintf("V%d", i)] = value
		}
		return out
	}
	emptyArgs := func(n int) []string {
		out := make([]string, n)
		out[0] = "x"
		return out
	}
	for _, tt := range []struct {
		name    string
		data    config.StartupData
		wantErr bool
	}{
		{name: "empty"},
		{name: "empty argument", data: config.StartupData{Command: []string{"printf", ""}}},
		{name: "32 variables", data: config.StartupData{EnvVars: envs(32, "")}},
		{name: "33 variables", data: config.StartupData{EnvVars: envs(33, "")}, wantErr: true},
		{name: "4096-byte value", data: config.StartupData{EnvVars: envs(1, strings.Repeat("x", 4096))}},
		{name: "4097-byte value", data: config.StartupData{EnvVars: envs(1, strings.Repeat("x", 4097))}, wantErr: true},
		{name: "env NUL", data: config.StartupData{EnvVars: envs(1, "nul\x00")}, wantErr: true},
		{name: "empty executable", data: config.StartupData{Command: []string{""}}, wantErr: true},
		{name: "command NUL", data: config.StartupData{Command: []string{"echo", "nul\x00"}}, wantErr: true},
		{name: "exact serialized command budget", data: config.StartupData{Command: []string{"x", strings.Repeat("z", 32768-15)}}},
		{name: "one serialized byte over", data: config.StartupData{Command: []string{"x", strings.Repeat("z", 32768-14)}}, wantErr: true},
		{name: "escaped env quotes", data: config.StartupData{EnvVars: envs(3, strings.Repeat("'", 4096))}, wantErr: true},
		{name: "escaped command quotes", data: config.StartupData{Command: []string{"x", strings.Repeat("'", 8192)}}, wantErr: true},
		{name: "env syntax overhead", data: config.StartupData{EnvVars: envs(8, strings.Repeat("x", 4096))}, wantErr: true},
		{name: "oversized name bounded before regexp", data: config.StartupData{EnvVars: map[string]string{strings.Repeat("A", 32768): ""}}, wantErr: true},
		{name: "empty args below budget", data: config.StartupData{Command: emptyArgs(10919)}},
		{name: "empty args exceed budget", data: config.StartupData{Command: emptyArgs(10920)}, wantErr: true},
		{name: "argument count preflight", data: config.StartupData{Command: emptyArgs(100000)}, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateStartupData(tt.data)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			env, command := startupScripts(tt.data)
			assert.LessOrEqual(t, len(env)+len(command), 32768)
		})
	}
}

func TestStartupScriptsPreserveValuesAndArgv(t *testing.T) {
	value := "a b'c\n$(exit 9); $HOME `false`"
	data := config.StartupData{
		EnvVars: map[string]string{"VALUE": value},
		Command: []string{"bash", "-c", `printf '%s\000' "$VALUE" "$@"`, "_", value, ""},
	}
	require.NoError(t, ValidateStartupData(data))
	env, command := startupScripts(data)
	require.True(t, strings.HasPrefix(command, "exec -- "))
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "env.sh"), []byte(env), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cmd.sh"), []byte(command), 0o600))
	cmd := exec.CommandContext(t.Context(), "bash", "-c", `set -a; . "$1"; . "$2"`, "_", filepath.Join(dir, "env.sh"), filepath.Join(dir, "cmd.sh"))
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Equal(t, value+"\x00"+value+"\x00\x00", string(out))
}

func TestInjectStartupData(t *testing.T) {
	data := config.StartupData{EnvVars: map[string]string{"VALUE": "quoted ' private value"}, Command: []string{"printf", "%s", "private-command"}}
	for _, tt := range []struct {
		name string
		data config.StartupData
		uid  *int64
		fail string
	}{
		{name: "root", data: data, uid: ptr.To[int64](0)},
		{name: "root image default", data: data},
		{name: "empty clears stale optional files"},
		{name: "env only leaves command absent", data: config.StartupData{EnvVars: data.EnvVars}},
		{name: "command only clears stale env", data: config.StartupData{Command: data.Command}},
		{name: "prepare failure", data: data, fail: "prepare"},
		{name: "env write failure", data: data, fail: "env.sh"},
		{name: "command write failure", data: data, fail: "cmd.sh"},
		{name: "signal write failure", data: data, fail: ".env_ready"},
		{name: "chmod failure before publish", data: data, fail: "chmod"},
		{name: "env commit failure", data: data, fail: "mv-env.sh"},
		{name: "command commit failure", data: data, fail: "mv-cmd.sh"},
		{name: "signal commit failure", data: data, fail: "mv-.env_ready"},
		{name: "signal response lost", data: data, fail: "lost"},
		{name: "cleanup failure is returned", data: data, fail: "cleanup"},
		{name: "write and cleanup errors both preserved", data: data, fail: "write-and-cleanup"},
		{name: "cancellation still cleans attempt", data: data, fail: "cancel"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.Chmod(dir, 0o755))
			for _, name := range []string{"env.sh", "cmd.sh", ".env_ready"} {
				require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("old-unpublished"), 0o600))
			}
			other := filepath.Join(dir, ".startup-OTHER00000")
			require.NoError(t, os.Mkdir(other, 0o700))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var fault atomic.Bool
			fault.Store(true)
			var writes, commits, cleanups atomic.Int32
			box := newStartupDeliveryTestServer(t, dir, func(ctx context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
				cfg := req.Msg.GetProcess()
				assert.NotContains(t, strings.Join(cfg.Args, " "), "private value")
				assert.NotContains(t, strings.Join(cfg.Args, " "), "private-command")
				prefix := ""
				switch cfg.Args[1] {
				case startupPrepareScript:
					if fault.Load() && tt.fail == "prepare" {
						prefix = "mktemp() { return 40; }\n"
					}
				case startupCommitScript:
					commits.Add(1)
					// Observe every rename, not just permissions after publication.
					prefix = `mv() {
    for file in "$stage"/* "$stage/.env_ready"; do
        [[ -f "$file" ]] || continue
        mode=$(ls -ld -- "$file") || return 50
        [[ "${mode:0:10}" == "-rw-------" ]] || return 50
    done
    [[ "$(cat -- "$claim/.env_ready")" != "$uid" ]] || return 51
    command mv "$@"
}
`
					if fault.Load() && tt.fail == "chmod" {
						prefix += "chmod() { return 41; }\n"
					}
					if fault.Load() && strings.HasPrefix(tt.fail, "mv-") {
						prefix = fmt.Sprintf("mv() { [[ \"${!#}\" != \"$claim/%s\" ]] || return 42; command mv \"$@\"; }\n", strings.TrimPrefix(tt.fail, "mv-"))
					}
				case startupCleanupScript:
					cleanups.Add(1)
					assert.NoError(t, ctx.Err())
					if fault.Load() && (tt.fail == "cleanup" || tt.fail == "write-and-cleanup") {
						prefix = "rm() { return 43; }\n"
					}
				default:
					t.Errorf("unexpected script")
				}
				stdout, stderr, code, err := executeStartupTestScript(t, ctx, cfg, dir, prefix)
				if err != nil {
					return err
				}
				if cfg.Args[1] == startupCommitScript && fault.Load() && tt.fail == "lost" {
					assert.Zero(t, code)
					return connect.NewError(connect.CodeUnavailable, errors.New("terminal response lost"))
				}
				return sendStartupTestResult(stream, stdout, stderr, code, true)
			}, func(name string) error {
				writes.Add(1)
				if fault.Load() && tt.fail == "cancel" {
					cancel()
					return errors.New("cancelled write")
				}
				if fault.Load() && (tt.fail == name || tt.fail == "write-and-cleanup") {
					return errors.New("write failed")
				}
				return nil
			})
			box.Spec.Template = &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", SecurityContext: &corev1.SecurityContext{RunAsUser: tt.uid}}}}}
			err := InjectStartupData(ctx, box, tt.data)
			if tt.fail != "" {
				require.Error(t, err)
				if commits.Load() > 0 {
					assert.ErrorIs(t, err, ErrStartupPublicationUncertain)
				} else {
					assert.NotErrorIs(t, err, ErrStartupPublicationUncertain)
				}
				if tt.fail == "cancel" {
					assert.ErrorIs(t, err, context.Canceled)
				}
				if tt.fail == "write-and-cleanup" {
					assert.ErrorContains(t, err, "write staged env.sh")
					assert.ErrorContains(t, err, "cleanup startup delivery")
				}
				if tt.fail != "lost" && tt.fail != "cleanup" {
					body, readErr := os.ReadFile(filepath.Join(dir, ".env_ready"))
					require.NoError(t, readErr)
					assert.Equal(t, "old-unpublished", string(body))
				}
			} else {
				require.NoError(t, err)
			}
			if tt.fail == "chmod" {
				for _, name := range []string{"env.sh", "cmd.sh"} {
					body, readErr := os.ReadFile(filepath.Join(dir, name))
					require.NoError(t, readErr)
					assert.Equal(t, "old-unpublished", string(body))
				}
			}
			fault.Store(false)
			// Retry an unpublished partial attempt with no optional files, proving
			// that stale env/cmd files cannot leak into the next successful delivery.
			wantData := tt.data
			if tt.fail != "" && tt.fail != "lost" && tt.fail != "cleanup" {
				wantData = config.StartupData{}
			}
			require.NoError(t, InjectStartupData(t.Context(), box, wantData))
			env, command := startupScripts(wantData)
			for name, want := range map[string]string{"env.sh": env, "cmd.sh": command, ".env_ready": "new-pod-uid"} {
				body, readErr := os.ReadFile(filepath.Join(dir, name))
				if want == "" {
					require.ErrorIs(t, readErr, os.ErrNotExist)
					continue
				}
				require.NoError(t, readErr)
				assert.Equal(t, want, string(body))
				info, statErr := os.Stat(filepath.Join(dir, name))
				require.NoError(t, statErr)
				assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
			}
			beforeWrites, beforeCommits, beforeCleanups := writes.Load(), commits.Load(), cleanups.Load()
			before, statErr := os.Stat(filepath.Join(dir, ".env_ready"))
			require.NoError(t, statErr)
			require.NoError(t, InjectStartupData(t.Context(), box, config.StartupData{EnvVars: map[string]string{"OTHER": "must-not-rewrite"}, Command: []string{"false"}}))
			after, statErr := os.Stat(filepath.Join(dir, ".env_ready"))
			require.NoError(t, statErr)
			assert.True(t, os.SameFile(before, after))
			assert.Equal(t, before.ModTime(), after.ModTime())
			assert.Equal(t, beforeWrites, writes.Load())
			assert.Equal(t, beforeCommits, commits.Load())
			assert.Equal(t, beforeCleanups, cleanups.Load())
			entries, readErr := os.ReadDir(dir)
			require.NoError(t, readErr)
			retainedAttempts := 0
			for _, entry := range entries {
				if entry.Name() == ".startup-publish.lock" {
					continue // The lock inode must remain stable across attempts.
				}
				if strings.HasPrefix(entry.Name(), ".startup-") && entry.Name() != filepath.Base(other) {
					retainedAttempts++
					assert.True(t, tt.fail == "lost" || tt.fail == "cleanup" || tt.fail == "write-and-cleanup", "unexpected retained attempt: %s", entry.Name())
					info, err := entry.Info()
					require.NoError(t, err)
					assert.True(t, info.IsDir())
					assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
				}
			}
			if tt.fail == "lost" {
				assert.EqualValues(t, 0, cleanups.Load(), "a missing terminal event must not authorize stage removal")
				assert.Equal(t, 1, retainedAttempts)
			}
			_, statErr = os.Stat(other)
			require.NoError(t, statErr, "must not clean another attempt")
		})
	}
	for _, box := range []*agentsv1alpha1.Sandbox{nil, {}} {
		require.ErrorContains(t, InjectStartupData(t.Context(), box, data), "pod UID")
	}
	assert.Equal(t, "/mnt/envd/claim", utils.SandboxEnvClaimDir)
}

func startupTestPath(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("flock"); err == nil {
		return os.Getenv("PATH")
	}
	for _, dir := range []string{"/opt/homebrew/opt/util-linux/bin", "/usr/local/opt/util-linux/bin", "/opt/homebrew/bin", "/usr/local/bin"} {
		if info, err := os.Stat(filepath.Join(dir, "flock")); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return dir + string(os.PathListSeparator) + os.Getenv("PATH")
		}
	}
	if goruntime.GOOS != "linux" {
		t.Skip("flock is unavailable on this platform; run real publication tests on Linux")
	}
	t.Fatal("real startup publication tests require flock")
	return ""
}

// Use the real static bash scripts with positional paths mapped to a local
// claim directory. Prefixes inject coreutils faults without replacing scripts.
func executeStartupTestScript(t *testing.T, ctx context.Context, cfg *process.ProcessConfig, dir, prefix string) (string, string, int32, error) {
	t.Helper()
	if !assert.Equal(t, "bash", cfg.Cmd) || !assert.GreaterOrEqual(t, len(cfg.Args), 4) {
		return "", "", 0, errors.New("invalid process config")
	}
	args := append([]string(nil), cfg.Args...)
	args[1] = prefix + args[1]
	for i := 3; i < len(args); i++ {
		if strings.HasPrefix(args[i], utils.SandboxEnvClaimDir) {
			args[i] = dir + strings.TrimPrefix(args[i], utils.SandboxEnvClaimDir)
		}
	}
	cmd := exec.CommandContext(ctx, cfg.Cmd, args...)
	cmd.Env = append(os.Environ(), "PATH="+startupTestPath(t))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	var code int32
	if errors.As(err, &exitErr) {
		code = int32(exitErr.ExitCode())
		t.Logf("test script exited with code %d: %s", code, stderr.String())
		err = nil
	}
	return strings.ReplaceAll(stdout.String(), dir, utils.SandboxEnvClaimDir), stderr.String(), code, err
}

func sendStartupTestResult(stream *connect.ServerStream[process.StartResponse], stdout, stderr string, code int32, exited bool) error {
	for _, event := range []*process.ProcessEvent_DataEvent{
		{Output: &process.ProcessEvent_DataEvent_Stdout{Stdout: []byte(stdout)}},
		{Output: &process.ProcessEvent_DataEvent_Stderr{Stderr: []byte(stderr)}},
	} {
		if err := stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_Data{Data: event}}}); err != nil {
			return err
		}
	}
	return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{Exited: exited, ExitCode: code}}}})
}

func newStartupDeliveryTestServer(t *testing.T, dir string, startFn func(context.Context, *connect.Request[process.StartRequest], *connect.ServerStream[process.StartResponse]) error, writeFn func(string) error) *agentsv1alpha1.Sandbox {
	t.Helper()
	_ = startupTestPath(t)
	var mu sync.Mutex
	handler := &mockProcessHandler{startFn: func(ctx context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
		mu.Lock()
		defer mu.Unlock()
		assert.Equal(t, basicAuthHeader("root"), req.Header().Get("Authorization"))
		assert.Equal(t, "test-token", req.Header().Get(accessTokenHeader))
		assert.Equal(t, "5000", req.Header().Get("Connect-Timeout-Ms"))
		if startFn != nil {
			return startFn(ctx, req, stream)
		}
		stdout, stderr, code, err := executeStartupTestScript(t, ctx, req.Msg.GetProcess(), dir, "")
		if err != nil {
			return err
		}
		return sendStartupTestResult(stream, stdout, stderr, code, true)
	}}
	mux := http.NewServeMux()
	path, processHandler := processconnect.NewProcessHandler(handler)
	mux.Handle(path, processHandler)
	mux.HandleFunc("/files", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		assert.Equal(t, "root", r.URL.Query().Get("username"))
		assert.Equal(t, basicAuthHeader("root"), r.Header.Get("Authorization"))
		assert.Equal(t, "test-token", r.Header.Get(accessTokenHeader))
		remote := r.URL.Query().Get("path")
		if !assert.True(t, strings.HasPrefix(remote, utils.SandboxEnvClaimDir+"/.startup-")) {
			http.Error(w, "write outside attempt", http.StatusBadRequest)
			return
		}
		local := dir + strings.TrimPrefix(remote, utils.SandboxEnvClaimDir)
		parent, err := os.Stat(filepath.Dir(local))
		if err != nil {
			t.Error(err)
			http.Error(w, "missing stage", 400)
			return
		}
		assert.Equal(t, os.FileMode(0o700), parent.Mode().Perm())
		claim, err := os.Stat(dir)
		if err != nil {
			t.Error(err)
			http.Error(w, "missing claim", 400)
			return
		}
		assert.Equal(t, os.FileMode(0o700), claim.Mode().Perm())
		if writeFn != nil {
			if err := writeFn(filepath.Base(local)); err != nil {
				http.Error(w, err.Error(), http.StatusForbidden)
				return
			}
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			t.Error(err)
			http.Error(w, "invalid file", 400)
			return
		}
		body, readErr := io.ReadAll(file)
		err = errors.Join(readErr, file.Close())
		if err == nil {
			err = os.WriteFile(local, body, 0o644)
		}
		// Model envd's os.Create defaults independently of the test runner's umask.
		if err == nil {
			err = os.Chmod(local, 0o644)
		}
		if err != nil {
			t.Error(err)
			http.Error(w, "write failed", 400)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	box := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		agentsv1alpha1.AnnotationRuntimeURL:         server.URL,
		agentsv1alpha1.AnnotationRuntimeAccessToken: "test-token",
	}}}
	box.Status.PodInfo.PodUID = "new-pod-uid"
	return box
}

func TestStartupDeliveryProcessProtocol(t *testing.T) {
	for _, phase := range []string{"prepare", "commit", "cleanup"} {
		for _, protocol := range []string{"nonzero", "not exited", "missing end", "transport error", "invalid path", "traversal path", "multiple paths", "oversized output"} {
			pathFault := strings.Contains(protocol, "path") || protocol == "oversized output"
			if phase != "prepare" && pathFault {
				continue
			}
			t.Run(phase+"/"+protocol, func(t *testing.T) {
				dir := t.TempDir()
				other := filepath.Join(dir, ".startup-OTHER00000")
				require.NoError(t, os.Mkdir(other, 0o700))
				var writes, cleanups atomic.Int32
				script := map[string]string{"prepare": startupPrepareScript, "commit": startupCommitScript, "cleanup": startupCleanupScript}[phase]
				box := newStartupDeliveryTestServer(t, dir, func(ctx context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
					cfg := req.Msg.GetProcess()
					if cfg.Args[1] == startupCleanupScript {
						cleanups.Add(1)
					}
					var stdout, stderr string
					var code int32
					if cfg.Args[1] != script || phase != "commit" {
						var err error
						stdout, stderr, code, err = executeStartupTestScript(t, ctx, cfg, dir, "")
						if err != nil {
							return err
						}
					}
					exited := true
					if cfg.Args[1] == script {
						stderr = "untrusted-private-output"
						switch protocol {
						case "nonzero":
							code = 71
						case "not exited":
							exited = false
						case "invalid path":
							stdout = "/tmp/untrusted-private-output\n"
						case "traversal path":
							stdout = utils.SandboxEnvClaimDir + "/../.startup-OTHER00000\n"
						case "multiple paths":
							stdout += utils.SandboxEnvClaimDir + "/.startup-OTHER00000\n"
						case "oversized output":
							stdout = strings.Repeat("x", 5000)
						case "missing end":
							return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_Data{Data: &process.ProcessEvent_DataEvent{Output: &process.ProcessEvent_DataEvent_Stdout{Stdout: []byte(stdout)}}}}})
						}
					}
					if err := sendStartupTestResult(stream, stdout, stderr, code, exited); err != nil {
						return err
					}
					if cfg.Args[1] == script && protocol == "transport error" {
						return connect.NewError(connect.CodeUnavailable, errors.New("interrupted terminal response"))
					}
					return nil
				}, func(string) error { writes.Add(1); return nil })
				err := InjectStartupData(t.Context(), box, config.StartupData{EnvVars: map[string]string{"VALUE": "private"}})
				require.ErrorContains(t, err, phase)
				assert.NotContains(t, err.Error(), "untrusted-private-output")
				if phase == "prepare" {
					assert.Zero(t, writes.Load())
					if pathFault {
						assert.Zero(t, cleanups.Load())
					} else {
						assert.EqualValues(t, 1, cleanups.Load())
					}
				}
				if phase == "commit" {
					_, statErr := os.Stat(filepath.Join(dir, ".env_ready"))
					require.ErrorIs(t, statErr, os.ErrNotExist)
				}
				_, statErr := os.Stat(other)
				require.NoError(t, statErr)
			})
		}
	}
}

func TestStartupProcessDiagnosticRedaction(t *testing.T) {
	const private = "test-private-token test-private-env test-private-command"
	for _, phase := range []string{"prepare", "commit", "cleanup"} {
		for _, mode := range []string{"success", "nonzero", "ambiguous end", "transport"} {
			t.Run(phase+"/"+mode, func(t *testing.T) {
				handler := &mockProcessHandler{startFn: func(_ context.Context, _ *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
					if err := stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_Data{Data: &process.ProcessEvent_DataEvent{Output: &process.ProcessEvent_DataEvent_Stdout{Stdout: []byte(private)}}}}}); err != nil {
						return err
					}
					if err := stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_Data{Data: &process.ProcessEvent_DataEvent{Output: &process.ProcessEvent_DataEvent_Stderr{Stderr: []byte(private)}}}}}); err != nil {
						return err
					}
					if mode == "transport" {
						return connect.NewError(connect.CodeUnavailable, errors.New(private))
					}
					end := &process.ProcessEvent_EndEvent{Exited: true}
					if mode != "success" {
						end.Error = ptr.To(private)
						if mode == "nonzero" {
							end.ExitCode = 42
						}
					}
					return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_End{End: end}}})
				}}
				_, box := newMockRuntimeServer(t, handler)
				var output bytes.Buffer
				var mu sync.Mutex
				logger := funcr.NewJSON(func(record string) {
					mu.Lock()
					defer mu.Unlock()
					output.WriteString(record)
				}, funcr.Options{Verbosity: 5})
				ctx := klog.NewContext(t.Context(), logger)
				result, err := runStartupScript(ctx, box, phase, "true", nil)
				if mode == "success" {
					require.NoError(t, err)
				} else {
					require.ErrorContains(t, err, phase+" startup delivery")
					for _, rendered := range []string{err.Error(), fmt.Sprintf("%+v", err), (&startupPublicationError{err: err}).Error()} {
						for _, secret := range strings.Fields(private) {
							assert.NotContains(t, rendered, secret)
						}
					}
					if mode == "transport" {
						var rpcErr *connect.Error
						require.ErrorAs(t, err, &rpcErr)
						assert.Equal(t, connect.CodeUnavailable, rpcErr.Code())
					} else {
						assert.ErrorIs(t, err, result.Error)
						assert.Equal(t, mode == "nonzero", result.ExitCode == 42)
					}
				}
				assert.Equal(t, []string{private}, result.Stdout)
				assert.Equal(t, []string{private}, result.Stderr)
				mu.Lock()
				defer mu.Unlock()
				assert.Contains(t, output.String(), "all messages are received")
				for _, secret := range strings.Fields(private) {
					assert.NotContains(t, output.String(), secret)
				}
			})
		}
	}
}

func TestStartupFileFailureDiagnosticRedaction(t *testing.T) {
	const private = "test-private-response test-private-env test-private-command"
	const stage = utils.SandboxEnvClaimDir + "/.startup-TEST000000"
	handler := &mockProcessHandler{startFn: func(_ context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
		if req.Msg.Process.Args[1] == startupPrepareScript {
			if err := stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_Data{Data: &process.ProcessEvent_DataEvent{Output: &process.ProcessEvent_DataEvent_Stdout{Stdout: []byte(stage + "\n")}}}}}); err != nil {
				return err
			}
		}
		return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{Exited: true}}}})
	}}
	mux := http.NewServeMux()
	processPath, processHandler := processconnect.NewProcessHandler(handler)
	mux.Handle(processPath, processHandler)
	mux.HandleFunc("/files", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, private, http.StatusForbidden)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	box := sandboxWithURL(server.URL)
	box.Status.PodInfo.PodUID = "test-pod-uid"
	var output bytes.Buffer
	logger := funcr.NewJSON(func(record string) { output.WriteString(record) }, funcr.Options{Verbosity: 5})
	err := InjectStartupData(klog.NewContext(t.Context(), logger), box, config.StartupData{
		EnvVars: map[string]string{"VALUE": "test-private-env"}, Command: []string{"echo", "test-private-command"},
	})
	require.ErrorContains(t, err, "write staged env.sh")
	assert.NotErrorIs(t, err, ErrStartupPublicationUncertain)
	for _, secret := range strings.Fields(private) {
		assert.NotContains(t, err.Error(), secret)
		assert.NotContains(t, output.String(), secret)
	}
	assert.Contains(t, output.String(), "writing file to runtime")
}

func TestStartupDeliveryErrorPreservesClassification(t *testing.T) {
	const private = "test-private-error-content"
	for _, cause := range []error{
		errors.New(private), context.Canceled, context.DeadlineExceeded,
		&APIError{Path: private, StatusCode: 403, Message: private},
		connect.NewError(connect.CodeUnavailable, errors.New(private)),
	} {
		t.Run(fmt.Sprintf("%T/%v", cause, errors.Is(cause, context.Canceled)), func(t *testing.T) {
			err := &startupDeliveryError{message: "verify startup publication", err: cause}
			assert.ErrorIs(t, err, cause)
			assert.NotContains(t, err.Error(), private)
			assert.Contains(t, err.Error(), "verify startup publication")
			var apiErr *APIError
			if errors.As(cause, &apiErr) {
				assert.Contains(t, err.Error(), "HTTP 403")
			}
		})
	}
}

func TestStartupDataPublished(t *testing.T) {
	for _, tt := range []struct {
		name      string
		status    int
		body      string
		published bool
		cancel    bool
		wantErr   bool
	}{
		{name: "exact current generation", status: 200, body: "pod-uid", published: true},
		{name: "missing signal", status: 404},
		{name: "old generation", status: 200, body: "old-uid"},
		{name: "empty signal", status: 200},
		{name: "no whitespace normalization", status: 200, body: "pod-uid\n"},
		{name: "bounded oversized signal", status: 200, body: "pod-uid" + strings.Repeat("x", 10000)},
		{name: "forbidden", status: 403, body: "private-token", wantErr: true},
		{name: "server error", status: 500, body: "private-token", wantErr: true},
		{name: "cancelled", cancel: true, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, "/files", r.URL.Path)
				assert.Equal(t, utils.SandboxEnvClaimDir+"/.env_ready", r.URL.Query().Get("path"))
				assert.Equal(t, "root", r.URL.Query().Get("username"))
				assert.Equal(t, basicAuthHeader("root"), r.Header.Get("Authorization"))
				assert.Equal(t, "test-token", r.Header.Get(accessTokenHeader))
				w.WriteHeader(tt.status)
				_, err := io.WriteString(w, tt.body)
				assert.NoError(t, err)
			}))
			defer server.Close()
			box := sandboxWithURL(server.URL)
			box.Annotations[agentsv1alpha1.AnnotationRuntimeAccessToken] = "test-token"
			box.Status.PodInfo.PodUID = "pod-uid"
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tt.cancel {
				cancel()
			}
			published, err := StartupDataPublished(ctx, box)
			assert.Equal(t, tt.published, published)
			if tt.wantErr {
				require.Error(t, err)
				assert.NotContains(t, err.Error(), "private-token")
				if tt.cancel {
					assert.ErrorIs(t, err, context.Canceled)
				}
			} else {
				require.NoError(t, err)
			}
		})
	}
	for _, box := range []*agentsv1alpha1.Sandbox{nil, {}} {
		published, err := StartupDataPublished(t.Context(), box)
		require.NoError(t, err)
		assert.False(t, published)
	}
}

func TestStartupPublicationLockTarget(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(fmt.Sprintf("symlink=%t", symlink), func(t *testing.T) {
			dir := t.TempDir()
			box := newStartupDeliveryTestServer(t, dir, nil, nil)
			lock := filepath.Join(dir, ".startup-publish.lock")
			target := filepath.Join(t.TempDir(), "untouched")
			require.NoError(t, os.WriteFile(target, []byte("outside-claim"), 0o600))
			if symlink {
				require.NoError(t, os.Symlink(target, lock))
			} else {
				require.NoError(t, os.Mkdir(lock, 0o700))
			}
			require.ErrorContains(t, InjectStartupData(t.Context(), box, config.StartupData{}), "commit startup delivery")
			_, err := os.Stat(filepath.Join(dir, ".env_ready"))
			require.ErrorIs(t, err, os.ErrNotExist)
			body, err := os.ReadFile(target)
			require.NoError(t, err)
			assert.Equal(t, "outside-claim", string(body))
			info, err := os.Lstat(lock)
			require.NoError(t, err)
			if symlink {
				assert.NotZero(t, info.Mode()&os.ModeSymlink)
			} else {
				assert.True(t, info.IsDir())
			}
		})
	}
}

func TestStartupPublicationLock(t *testing.T) {
	for _, killed := range []bool{false, true} {
		t.Run(fmt.Sprintf("SIGKILL=%t", killed), func(t *testing.T) {
			dir := t.TempDir()
			box := newStartupDeliveryTestServer(t, dir, nil, nil)
			// An abandoned lock from the old mkdir protocol must be left alone.
			legacy := filepath.Join(dir, ".startup-publish-lock")
			require.NoError(t, os.Mkdir(legacy, 0o700))
			stage := filepath.Join(dir, ".startup-HOLDER0000")
			require.NoError(t, os.Mkdir(stage, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(stage, "env.sh"), []byte("holder-env"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(stage, ".env_ready"), []byte("new-pod-uid"), 0o600))
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			// Pause the real commit immediately after the kernel lock is acquired.
			// The pipe handshake avoids timing assumptions or a synthetic stale lock.
			prefix := "flock() { command flock \"$@\"; printf 'locked\\n'; read -r release; }\n"
			cmd := exec.CommandContext(ctx, "bash", "-c", prefix+startupCommitScript, "holder", dir, "new-pod-uid", stage, "1", "0")
			cmd.Env = append(os.Environ(), "PATH="+startupTestPath(t))
			stdin, err := cmd.StdinPipe()
			require.NoError(t, err)
			stdout, err := cmd.StdoutPipe()
			require.NoError(t, err)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			require.NoError(t, cmd.Start())
			t.Cleanup(func() {
				if err := stdin.Close(); !errors.Is(err, os.ErrClosed) {
					assert.NoError(t, err)
				}
				if cmd.ProcessState == nil {
					killErr := cmd.Process.Kill()
					if !errors.Is(killErr, os.ErrProcessDone) {
						assert.NoError(t, killErr)
					}
					assert.Error(t, cmd.Wait())
				}
			})
			locked := make(chan error, 1)
			go func() {
				line, err := bufio.NewReader(stdout).ReadString('\n')
				if err == nil && line != "locked\n" {
					err = fmt.Errorf("unexpected lock handshake %q", line)
				}
				locked <- err
			}()
			select {
			case err := <-locked:
				require.NoError(t, err)
			case <-ctx.Done():
				t.Fatal("lock holder did not acquire the kernel lock")
			}
			lockPath := filepath.Join(dir, ".startup-publish.lock")
			before, err := os.Stat(lockPath)
			require.NoError(t, err)
			data := config.StartupData{EnvVars: map[string]string{"VALUE": "retry"}}
			if killed {
				require.NoError(t, cmd.Process.Kill())
				require.Error(t, cmd.Wait(), "SIGKILL must release the held kernel lock")
				require.NoError(t, InjectStartupData(ctx, box, data))
			} else {
				start := time.Now()
				require.ErrorContains(t, InjectStartupData(ctx, box, data), "commit startup delivery")
				assert.Less(t, time.Since(start), 2*time.Second, "must not wait for or reclaim the active holder")
				_, err := os.Stat(filepath.Join(dir, ".env_ready"))
				require.ErrorIs(t, err, os.ErrNotExist)
				_, err = io.WriteString(stdin, "release\n")
				require.NoError(t, err)
				require.NoError(t, cmd.Wait(), stderr.String())
				require.NoError(t, InjectStartupData(ctx, box, data))
			}
			after, err := os.Stat(lockPath)
			require.NoError(t, err)
			assert.True(t, os.SameFile(before, after), "must never unlink or replace the lock inode")
			ready, err := os.ReadFile(filepath.Join(dir, ".env_ready"))
			require.NoError(t, err)
			assert.Equal(t, "new-pod-uid", string(ready))
			env, err := os.ReadFile(filepath.Join(dir, "env.sh"))
			require.NoError(t, err)
			want := "holder-env"
			if killed {
				want, _ = startupScripts(data)
			}
			assert.Equal(t, want, string(env), "first publication for the same Pod remains immutable")
			_, err = os.Stat(legacy)
			require.NoError(t, err)
			_, err = os.Stat(stage)
			require.NoError(t, err, "another attempt's staging directory must not be reclaimed")
		})
	}
}

func TestStartupCommitRechecksPublishedGeneration(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprintf("empty=%t", empty), func(t *testing.T) {
			dir := t.TempDir()
			var uploads atomic.Int32
			box := newStartupDeliveryTestServer(t, dir, func(ctx context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
				cfg := req.Msg.GetProcess()
				if cfg.Args[1] == startupCommitScript {
					// Another attempt won after prepare and before this commit.
					for name, content := range map[string]string{"env.sh": "winner-env", "cmd.sh": "winner-command", ".env_ready": "new-pod-uid"} {
						if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
							return err
						}
					}
				}
				stdout, stderr, code, err := executeStartupTestScript(t, ctx, cfg, dir, "")
				if err != nil {
					return err
				}
				return sendStartupTestResult(stream, stdout, stderr, code, true)
			}, func(string) error { uploads.Add(1); return nil })
			data := config.StartupData{EnvVars: map[string]string{"VALUE": "loser"}, Command: []string{"false"}}
			if empty {
				data = config.StartupData{}
			}
			require.NoError(t, InjectStartupData(t.Context(), box, data))
			for name, want := range map[string]string{"env.sh": "winner-env", "cmd.sh": "winner-command"} {
				body, err := os.ReadFile(filepath.Join(dir, name))
				require.NoError(t, err)
				assert.Equal(t, want, string(body))
			}
			before := uploads.Load()
			require.NoError(t, InjectStartupData(t.Context(), box, data))
			assert.Equal(t, before, uploads.Load())
		})
	}
}
