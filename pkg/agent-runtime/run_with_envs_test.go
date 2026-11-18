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

package agentruntime

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	testPodUID = "pod-uid-1"
	// blockWindow is how long a wait-mode run must stay alive to count as blocked.
	blockWindow = 1500 * time.Millisecond
)

// wrapperRun is one run_with_envs.sh process whose output goes to a file, so
// waiting for the process never depends on its children closing a pipe.
type wrapperRun struct {
	cmd     *exec.Cmd
	outPath string
	done    chan struct{}
}

func startWrapper(t *testing.T, dir string, env map[string]string, args ...string) *wrapperRun {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not available")
	}
	outPath := filepath.Join(t.TempDir(), "out")
	out, err := os.Create(outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()

	cmd := exec.Command(bash, append([]string{"run_with_envs.sh"}, args...)...)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"SANDBOX_ENV_DIR=" + dir,
		"SANDBOX_POD_UID=" + testPodUID,
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	run := &wrapperRun{cmd: cmd, outPath: outPath, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(run.done)
	}()
	t.Cleanup(func() {
		select {
		case <-run.done:
		default:
			_ = cmd.Process.Kill()
			<-run.done
		}
	})
	return run
}

func (r *wrapperRun) output(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(r.outPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRunWithEnvs(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		env    map[string]string
		envSh  string
		cmdSh  string
		signal string // .env_ready content; empty means not delivered

		wantBlocked bool
		wantCode    int
		wantSuffix  string
		wantAbsent  string
	}{
		{
			name: "probe succeeds before the signal",
			args: []string{"--probe", "--", "false"},
		},
		{
			name:     "probe with an empty argv fails",
			args:     []string{"--probe", "--"},
			wantCode: 1,
		},
		{
			name:       "probe runs the real check with the claim env after the signal",
			args:       []string{"--probe", "--", "bash", "-c", `printf %s "$FOO"`},
			envSh:      "export FOO='a b'\\''c'\n",
			signal:     testPodUID,
			wantSuffix: "a b'c",
		},
		{
			name:     "failing probe after the signal fails",
			args:     []string{"--probe", "--", "false"},
			signal:   testPodUID,
			wantCode: 1,
		},
		{
			name:        "waits until the signal is delivered",
			args:        []string{"--", "printf", "%s", "started"},
			wantBlocked: true,
		},
		{
			name:        "ignores a signal from another pod generation",
			args:        []string{"--", "printf", "%s", "started"},
			signal:      "other-pod-uid",
			wantBlocked: true,
		},
		{
			name:       "template command runs with the claim env",
			args:       []string{"--", "bash", "-c", `printf %s "$FOO"`},
			envSh:      "export FOO='a b'\\''c'\n",
			signal:     testPodUID,
			wantSuffix: "a b'c",
		},
		{
			name:       "claim command overrides the template command",
			args:       []string{"--", "printf", "%s", "template"},
			cmdSh:      "exec -- 'printf' '%s' 'claim'\n",
			signal:     testPodUID,
			wantSuffix: "claim",
			wantAbsent: "template\n",
		},
		{
			name:       "cmd.sh that returns fails instead of falling back to the template",
			args:       []string{"--", "printf", "%s", "template-ran"},
			cmdSh:      "true\n",
			signal:     testPodUID,
			wantCode:   1,
			wantAbsent: "template-ran\n",
		},
		{
			name:     "no command to run fails",
			args:     []string{"--"},
			signal:   testPodUID,
			wantCode: 1,
		},
		{
			name:     "argv starting with a dash is executed, not parsed as exec options",
			args:     []string{"--", "-l"},
			signal:   testPodUID,
			wantCode: 127,
		},
		{
			name: "template env reaches the app unchanged",
			args: []string{"--", "bash", "-c", `printf %s "$ENV_DIR|$CMD|$PROBE|$POD_UID"`},
			env: map[string]string{
				"ENV_DIR": "/data/app",
				"CMD":     "keep",
				"PROBE":   "liveness",
				"POD_UID": "mine",
			},
			signal:     testPodUID,
			wantSuffix: "/data/app|keep|liveness|mine",
		},
		{
			name:       "claim env cannot redirect the claim command or the argv",
			args:       []string{"--", "printf", "%s", "template"},
			envSh:      "export CMD='echo'\nexport CMD_SCRIPT='/nonexistent'\n",
			cmdSh:      "exec -- 'printf' '%s' 'claim'\n",
			signal:     testPodUID,
			wantSuffix: "claim",
		},
		{
			name:       "env.sh with a failing line fails startup",
			args:       []string{"--", "printf", "%s", "started"},
			envSh:      "export UID='1000'\nexport OK=1\n",
			signal:     testPodUID,
			wantCode:   1,
			wantAbsent: "started\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range map[string]string{"env.sh": tt.envSh, "cmd.sh": tt.cmdSh, ".env_ready": tt.signal} {
				if content == "" {
					continue
				}
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			run := startWrapper(t, dir, tt.env, tt.args...)
			select {
			case <-run.done:
				if tt.wantBlocked {
					t.Fatalf("wrapper exited, want it to keep waiting; output:\n%s", run.output(t))
				}
			case <-time.After(blockWindow):
				if !tt.wantBlocked {
					t.Fatalf("wrapper still running after %s; output:\n%s", blockWindow, run.output(t))
				}
				return
			}

			out := run.output(t)
			if code := run.cmd.ProcessState.ExitCode(); code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d; output:\n%s", code, tt.wantCode, out)
			}
			if !strings.HasSuffix(out, tt.wantSuffix) {
				t.Errorf("output does not end with %q:\n%s", tt.wantSuffix, out)
			}
			if tt.wantAbsent != "" && strings.Contains(out, tt.wantAbsent) {
				t.Errorf("output contains %q:\n%s", tt.wantAbsent, out)
			}
		})
	}
}

func TestRunWithEnvsExitsOnSIGTERMWhileWaiting(t *testing.T) {
	run := startWrapper(t, t.TempDir(), nil, "--", "printf", "%s", "started")

	// The wait message is printed after the traps are installed.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(run.output(t), "waiting for claim-time env") {
		if time.Now().After(deadline) {
			t.Fatalf("wrapper never started waiting; output:\n%s", run.output(t))
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := run.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-run.done:
	case <-time.After(time.Second):
		t.Fatal("wrapper did not exit promptly on SIGTERM")
	}
	if code := run.cmd.ProcessState.ExitCode(); code != 143 {
		t.Fatalf("exit code = %d, want 143 from the TERM trap", code)
	}
}
