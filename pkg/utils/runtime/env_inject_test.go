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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
			var mu sync.Mutex
			handler := &mockProcessHandler{startFn: func(_ context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
				mu.Lock()
				defer mu.Unlock()
				path := filepath.Join(dir, filepath.Base(req.Msg.GetProcess().Args[1]))
				if err := os.Chmod(path, 0o600); err != nil {
					return err
				}
				return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{Exited: true}}}})
			}}
			mux := http.NewServeMux()
			path, processHandler := processconnect.NewProcessHandler(handler)
			mux.Handle(path, processHandler)
			mux.HandleFunc("/files", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				file, _, err := r.FormFile("file")
				if err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				defer file.Close()
				body, err := io.ReadAll(file)
				if err == nil {
					err = os.WriteFile(filepath.Join(dir, filepath.Base(r.URL.Query().Get("path"))), body, 0o600)
				}
				if err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			})
			server := httptest.NewServer(mux)
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", "../../agent-runtime/run_with_envs.sh", "--", "bash", "-c", `printf '%s' "template:$VALUE"`)
			cmd.Env = append(os.Environ(), "SANDBOX_ENV_DIR="+dir, "SANDBOX_POD_UID=pod-uid")
			out := filepath.Join(t.TempDir(), "output")
			output, err := os.Create(out)
			require.NoError(t, err)
			defer output.Close()
			cmd.Stdout, cmd.Stderr = output, output
			require.NoError(t, cmd.Start())
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			box := sandboxWithURL(server.URL)
			box.Status.PodInfo.PodUID = "pod-uid"
			data := config.StartupData{EnvVars: map[string]string{"VALUE": "a b'c"}}
			want := "template:a b'c"
			if claimCommand {
				data.Command = []string{"bash", "-c", `printf '%s' "claim:$VALUE"`}
				want = "claim:a b'c"
			}
			require.NoError(t, InjectStartupData(ctx, box, data))
			require.NoError(t, cmd.Wait())
			body, err := os.ReadFile(out)
			require.NoError(t, err)
			assert.True(t, strings.HasSuffix(string(body), want), "%s", body)
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
	for _, data := range []config.StartupData{
		{EnvVars: map[string]string{"VALUE": "nul\x00"}},
		{EnvVars: map[string]string{"VALUE": strings.Repeat("x", 4097)}},
		{Command: []string{""}}, {Command: []string{"echo", "nul\x00"}},
		{Command: []string{"echo", strings.Repeat("x", 32769)}},
	} {
		require.Error(t, ValidateStartupData(data))
	}
	require.NoError(t, ValidateStartupData(config.StartupData{}))
	require.NoError(t, ValidateStartupData(config.StartupData{Command: []string{"printf", ""}}))
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
	data := config.StartupData{EnvVars: map[string]string{"VALUE": "quoted ' value"}, Command: []string{"printf", "%s", "claim"}}
	for _, tt := range []struct {
		name string
		data config.StartupData
		uid  *int64
		mode string
		fail string
	}{
		{name: "root", data: data, uid: ptr.To[int64](0), mode: "0600"},
		{name: "root image default", data: data, mode: "0600"},
		{name: "empty claim only signals", mode: "0600"},
		{name: "env only leaves command absent", data: config.StartupData{EnvVars: data.EnvVars}, mode: "0600"},
		{name: "write failure never signals", data: data, mode: "0600", fail: "env.sh"},
		{name: "chmod failure never signals", data: data, mode: "0600", fail: "chmod"},
		{name: "command failure never signals", data: data, mode: "0600", fail: "cmd.sh"},
		{name: "signal failure is returned", data: data, mode: "0600", fail: ".env_ready"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			files := map[string]string{}
			var calls []string
			handler := &mockProcessHandler{startFn: func(_ context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
				mu.Lock()
				defer mu.Unlock()
				cfg := req.Msg.GetProcess()
				assert.Equal(t, "chmod", cfg.Cmd)
				assert.Equal(t, tt.mode, cfg.Args[0])
				assert.Equal(t, basicAuthHeader("root"), req.Header().Get("Authorization"))
				calls = append(calls, "chmod:"+filepath.Base(cfg.Args[1]))
				code := int32(0)
				if tt.fail == "chmod" {
					code = 1
				}
				return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_End{
					End: &process.ProcessEvent_EndEvent{Exited: true, ExitCode: code},
				}}})
			}}
			mux := http.NewServeMux()
			path, processHandler := processconnect.NewProcessHandler(handler)
			mux.Handle(path, processHandler)
			mux.HandleFunc("/files", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				name := filepath.Base(r.URL.Query().Get("path"))
				calls = append(calls, "write:"+name)
				assert.Equal(t, "root", r.URL.Query().Get("username"))
				assert.Equal(t, basicAuthHeader("root"), r.Header.Get("Authorization"))
				assert.Equal(t, "test-token", r.Header.Get(accessTokenHeader))
				if tt.fail == name {
					http.Error(w, "write failed", http.StatusForbidden)
					return
				}
				file, _, err := r.FormFile("file")
				if err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				defer file.Close()
				body, err := io.ReadAll(file)
				if err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				files[name] = string(body)
				w.WriteHeader(http.StatusNoContent)
			})
			server := httptest.NewServer(mux)
			defer server.Close()
			box := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				agentsv1alpha1.AnnotationRuntimeURL:         server.URL,
				agentsv1alpha1.AnnotationRuntimeAccessToken: "test-token",
			}}}
			box.Status.PodInfo.PodUID = "new-pod-uid"
			box.Spec.Template = &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", SecurityContext: &corev1.SecurityContext{RunAsUser: tt.uid}}}}}
			err := InjectStartupData(t.Context(), box, tt.data)
			mu.Lock()
			defer mu.Unlock()
			if tt.fail != "" {
				require.Error(t, err)
				assert.NotContains(t, files, ".env_ready")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "new-pod-uid", files[".env_ready"])
			expected := []string{}
			if len(tt.data.EnvVars) > 0 {
				expected = append(expected, "write:env.sh", "chmod:env.sh")
			}
			if len(tt.data.Command) > 0 {
				expected = append(expected, "write:cmd.sh", "chmod:cmd.sh")
			} else {
				assert.NotContains(t, files, "cmd.sh")
			}
			expected = append(expected, "write:.env_ready")
			assert.Equal(t, expected, calls)
		})
	}
	require.ErrorContains(t, InjectStartupData(t.Context(), &agentsv1alpha1.Sandbox{}, data), "pod UID")
	assert.Equal(t, "/mnt/envd/claim", utils.SandboxEnvClaimDir)
}
