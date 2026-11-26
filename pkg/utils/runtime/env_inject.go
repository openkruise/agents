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
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/runtime/config"
	"github.com/openkruise/agents/proto/envd/process"
)

var startupEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func ValidateStartupData(data config.StartupData) error {
	if len(data.EnvVars) > 32 {
		return fmt.Errorf("at most 32 startup environment variables are allowed")
	}
	const budget = 32 * 1024
	budgetError := func() error { return fmt.Errorf("startup data exceeds 32768 serialized script bytes") }
	// Even an empty argument needs a space and two quotes. Bound the count
	// before scanning arguments or allocating the generated script.
	if len(data.Command) > (budget-len("exec --\n"))/3 {
		return budgetError()
	}
	total := 0
	if len(data.Command) > 0 {
		total = len("exec --\n")
	}
	for name, value := range data.EnvVars {
		if len(name) > budget-total-len("export =''\n") {
			return budgetError()
		}
		if !startupEnvName.MatchString(name) {
			return fmt.Errorf("invalid startup environment variable name %q", name)
		}
		for _, prefix := range []string{"SANDBOX_", "ENVD_", "KRUISE_", "KUBERNETES_", "BASH", "_rwe_"} {
			if strings.HasPrefix(name, prefix) {
				return fmt.Errorf("startup environment variable %q is reserved", name)
			}
		}
		switch name {
		case "PATH", "HOME", "HOSTNAME", "PWD", "SHELL", "USER", "LOGNAME", "TERM", "UID", "EUID", "PPID", "SHELLOPTS", "IFS", "ENV", "DIRSTACK", "GROUPS", "FUNCNAME", "PIPESTATUS", "LINENO", "SECONDS", "RANDOM", "SRANDOM":
			return fmt.Errorf("startup environment variable %q is reserved", name)
		}
		if len(value) > 4*1024 || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("startup environment variable %q contains NUL or exceeds 4096 bytes", name)
		}
		// A single quote expands from one byte to four in a shell literal.
		total += len("export =''\n") + len(name) + len(value) + 3*strings.Count(value, "'")
		if total > budget {
			return budgetError()
		}
	}
	if len(data.Command) > 0 && data.Command[0] == "" {
		return fmt.Errorf("startup command executable must not be empty")
	}
	for _, arg := range data.Command {
		if len(arg) > budget-total-3 {
			return budgetError()
		}
		if strings.ContainsRune(arg, '\x00') {
			return fmt.Errorf("startup command contains NUL")
		}
		total += 3 + len(arg) + 3*strings.Count(arg, "'")
		if total > budget {
			return budgetError()
		}
	}
	return nil
}

func startupScripts(data config.StartupData) (string, string) {
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'" }
	names := make([]string, 0, len(data.EnvVars))
	for name := range data.EnvVars {
		names = append(names, name)
	}
	sort.Strings(names)
	var env strings.Builder
	for _, name := range names {
		fmt.Fprintf(&env, "export %s=%s\n", name, quote(data.EnvVars[name]))
	}
	var command strings.Builder
	if len(data.Command) > 0 {
		command.WriteString("exec --")
		for _, arg := range data.Command {
			command.WriteByte(' ')
			command.WriteString(quote(arg))
		}
		command.WriteByte('\n')
	}
	return env.String(), command.String()
}

// These scripts never receive claim env or command contents as process arguments.
// Only the native /files API carries those contents, inside a private directory.
const startupPublishedCheck = `
if [[ -e "$claim/.env_ready" || -L "$claim/.env_ready" ]]; then
    [[ -f "$claim/.env_ready" && ! -L "$claim/.env_ready" ]] || exit 1
    ready=$(cat -- "$claim/.env_ready")
    [[ "$ready" != "$uid" ]] || exit 0
fi
`

const startupPrepareScript = `set -euo pipefail
claim=$1
uid=$2
[[ -d "$claim" && ! -L "$claim" ]] || exit 1
` + startupPublishedCheck + `
umask 077
chmod -- 0700 "$claim"
mktemp -d "$claim/.startup-XXXXXXXXXX"
`

const startupCommitScript = `set -euo pipefail
claim=$1
uid=$2
stage=$3
has_env=$4
has_command=$5
[[ -d "$claim" && ! -L "$claim" ]] || exit 1
[[ "$stage" == "$claim/"* && "${stage##*/}" =~ ^\.startup-[A-Za-z0-9]{10}$ ]] || exit 1
[[ -d "$stage" && ! -L "$stage" ]] || exit 1
` + startupPublishedCheck + `
# The kernel releases this advisory lock even after SIGKILL. Never unlink it:
# replacing the inode would let two publishers hold different locks.
umask 077
lock="$claim/.startup-publish.lock"
[[ ! -L "$lock" && ( ! -e "$lock" || -f "$lock" ) ]] || exit 1
exec 9>>"$lock"
command -v flock >/dev/null || exit 127
flock -n 9 || { status=$?; [[ "$status" != 1 ]] || exit 75; exit "$status"; }
trap 'exit 143' TERM
trap 'exit 130' INT
` + startupPublishedCheck + `
files=("$stage/.env_ready")
case "$has_env" in 1) files+=("$stage/env.sh");; 0);; *) exit 1;; esac
case "$has_command" in 1) files+=("$stage/cmd.sh");; 0);; *) exit 1;; esac
for file in "${files[@]}"; do
    [[ -f "$file" && ! -L "$file" ]] || exit 1
done
signal=$(cat -- "$stage/.env_ready")
[[ "$signal" == "$uid" ]] || exit 1
# Never allow mv to interpret a destination as a directory.
for name in env.sh cmd.sh .env_ready; do
    [[ ! -d "$claim/$name" ]] || exit 1
done
# /files may create 0644 files. Their 0700 parent hides that permission window;
# chmod all files before the first rename exposes them in the claim directory.
chmod -- 0600 "${files[@]}"
if [[ "$has_env" == 1 ]]; then
    mv -f -- "$stage/env.sh" "$claim/env.sh"
else
    rm -f -- "$claim/env.sh"
fi
if [[ "$has_command" == 1 ]]; then
    mv -f -- "$stage/cmd.sh" "$claim/cmd.sh"
else
    # An absent command must stay absent, including after an unpublished retry.
    rm -f -- "$claim/cmd.sh"
fi
mv -f -- "$stage/.env_ready" "$claim/.env_ready"
`

const startupCleanupScript = `set -euo pipefail
claim=$1
stage=$2
[[ "$stage" == "$claim/"* && "${stage##*/}" =~ ^\.startup-[A-Za-z0-9]{10}$ ]] || exit 1
rm -rf -- "$stage"
`

var startupStagePath = regexp.MustCompile(`^/mnt/envd/claim/\.startup-[A-Za-z0-9]{10}$`)

var ErrStartupPublicationUncertain = errors.New("startup publication outcome is uncertain")

const startupPublicationLockBusyExitCode = 75

type startupDeliveryError struct {
	message string
	err     error
}

func (e *startupDeliveryError) Error() string {
	var apiErr *APIError
	if errors.As(e.err, &apiErr) {
		return fmt.Sprintf("%s (HTTP %d)", e.message, apiErr.StatusCode)
	}
	if errors.Is(e.err, context.DeadlineExceeded) {
		return e.message + ": deadline exceeded"
	}
	if errors.Is(e.err, context.Canceled) {
		return e.message + ": context canceled"
	}
	var rpcErr *connect.Error
	if errors.As(e.err, &rpcErr) {
		return e.message + ": " + rpcErr.Code().String()
	}
	return e.message
}

func (e *startupDeliveryError) Unwrap() error {
	return e.err
}

type startupPublicationError struct {
	err          error
	exited       bool
	exitCode     int32
	ambiguousEnd bool
}

func (e *startupPublicationError) Error() string {
	return fmt.Sprintf("%s: %v", ErrStartupPublicationUncertain, e.err)
}

func (e *startupPublicationError) Unwrap() error {
	return e.err
}

func (e *startupPublicationError) Is(target error) bool {
	return target == ErrStartupPublicationUncertain
}

// StartupDataPublished reads only the native /files publication signal. An exact
// current PodUID proves the original post-processes reached their final publish;
// absence is not permission to inject the incomplete claim's startup data.
func StartupDataPublished(ctx context.Context, box *agentsv1alpha1.Sandbox, opts ...Option) (published bool, err error) {
	if box == nil || box.Status.PodInfo.PodUID == "" {
		return false, nil
	}
	r := NewRuntime(box, opts...).(*runtimeClient)
	uid := string(box.Status.PodInfo.PodUID)
	body, err := r.Filesystem().ReadFile(ctx, ReadFileRequest{
		FilePath: utils.SandboxEnvClaimDir + "/.env_ready", Username: defaultRuntimeFilesUsername,
		AuthUser: defaultRuntimeFilesUsername, Timeout: min(r.timeout, 5*time.Second), MaxBytes: int64(len(uid)),
	})
	if errors.Is(err, ErrRuntimePathNotFound) || errors.Is(err, ErrRuntimeFileTooLarge) {
		return false, nil
	}
	if err != nil {
		return false, &startupDeliveryError{message: "read startup publication signal failed", err: err}
	}
	return string(body) == uid, nil
}

func runStartupScript(ctx context.Context, box *agentsv1alpha1.Sandbox, phase, script string, args []string, opts ...Option) (RunCommandResult, error) {
	result, err := RunCommandWithRuntime(ctx, RunCmdFuncArgs{
		Sbx:           box,
		ProcessConfig: &process.ProcessConfig{Cmd: "bash", Args: append([]string{"-c", script, "startup-delivery"}, args...)},
		Timeout:       6 * time.Second, ProcessTimeout: 5 * time.Second,
		AuthUser:      defaultRuntimeFilesUsername,
		OutputOptions: ProcessOutputOptions{MaxBytes: 4096, SuppressResultLog: true},
	}, opts...)
	if err != nil {
		klog.FromContext(ctx).V(2).Info("Startup process returned an error", "phase", phase, "exited", result.Exited, "exitCode", result.ExitCode, "hasProcessError", result.Error != nil)
		return result, &startupDeliveryError{
			message: fmt.Sprintf("%s startup delivery: exited=%t exitCode=%d", phase, result.Exited, result.ExitCode),
			err:     errors.Join(err, ctx.Err()),
		}
	}
	if !result.Exited || result.ExitCode != 0 {
		// Do not include process output: an external protocol result is untrusted.
		return result, fmt.Errorf("%s startup delivery: exited=%t exitCode=%d", phase, result.Exited, result.ExitCode)
	}
	return result, nil
}

func InjectStartupData(ctx context.Context, box *agentsv1alpha1.Sandbox, data config.StartupData, opts ...Option) (err error) {
	if err := ValidateStartupData(data); err != nil {
		return err
	}
	if box == nil || box.Status.PodInfo.PodUID == "" {
		return fmt.Errorf("sandbox has no pod UID for startup delivery")
	}
	uid := string(box.Status.PodInfo.PodUID)
	publicationAttempted := false
	var commitResult RunCommandResult
	result, prepareErr := runStartupScript(ctx, box, "prepare", startupPrepareScript, []string{utils.SandboxEnvClaimDir, uid}, opts...)
	output := strings.Join(result.Stdout, "")
	stage := strings.TrimSuffix(output, "\n")
	if output != "" && !startupStagePath.MatchString(stage) {
		return errors.Join(prepareErr, fmt.Errorf("prepare startup delivery returned an invalid staging path"))
	}
	if stage != "" {
		defer func() {
			// A lost stream can leave commit using this stage; only a terminal event permits cleanup.
			if !publicationAttempted || (commitResult.Exited && (commitResult.Error == nil || commitResult.ExitCode != 0)) {
				_, cleanupErr := runStartupScript(context.WithoutCancel(ctx), box, "cleanup", startupCleanupScript, []string{utils.SandboxEnvClaimDir, stage}, opts...)
				err = errors.Join(err, cleanupErr)
			}
			if publicationAttempted && err != nil {
				err = &startupPublicationError{err: err, exited: commitResult.Exited, exitCode: commitResult.ExitCode, ambiguousEnd: commitResult.Error != nil && commitResult.ExitCode == 0}
			}
		}()
	}
	if prepareErr != nil {
		return prepareErr
	}
	if stage == "" {
		// A matching published signal is immutable for this PodUID.
		return nil
	}
	env, command := startupScripts(data)
	hasEnv, hasCommand := "0", "0"
	if env != "" {
		hasEnv = "1"
	}
	if command != "" {
		hasCommand = "1"
	}
	for _, file := range []struct{ name, content string }{{"env.sh", env}, {"cmd.sh", command}, {".env_ready", uid}} {
		if file.content == "" {
			continue
		}
		if _, err := WriteFileWithRuntime(ctx, WriteFileArgs{
			Sbx: box, FilePath: stage + "/" + file.name, Content: []byte(file.content),
			Username: defaultRuntimeFilesUsername, AuthUser: defaultRuntimeFilesUsername, Permissions: 0o600,
		}, opts...); err != nil {
			return &startupDeliveryError{message: "write staged " + file.name + " failed", err: errors.Join(err, ctx.Err())}
		}
	}
	publicationAttempted = true
	commitResult, err = runStartupScript(ctx, box, "commit", startupCommitScript, []string{utils.SandboxEnvClaimDir, uid, stage, hasEnv, hasCommand}, opts...)
	return err
}

func DeliverStartupData(ctx context.Context, box *agentsv1alpha1.Sandbox, cli client.Client, opts ...Option) (bool, error) {
	if box.Spec.StartupMode != agentsv1alpha1.StartupModeDeferred {
		return false, nil
	}
	request, err := GetInitRuntimeRequest(box)
	if err != nil {
		return false, err
	}
	if request == nil {
		return false, fmt.Errorf("Deferred sandbox has no recorded startup data")
	}
	data := config.StartupData{EnvVars: request.EnvVars, Command: request.Command}
	for attempt := 0; ; attempt++ {
		err = InjectStartupData(ctx, box, data, opts...)
		if err == nil {
			return true, RecordStartupDelivered(ctx, box, cli)
		}
		var publicationErr *startupPublicationError
		if !errors.As(err, &publicationErr) {
			return false, err
		}
		published := publicationErr.exited && publicationErr.exitCode == 0 && !publicationErr.ambiguousEnd
		var verifyErr error
		if !published {
			published, verifyErr = StartupDataPublished(ctx, box, opts...)
		}
		if published {
			klog.FromContext(ctx).Info("Startup publication confirmed after response or cleanup failure", "sandbox", klog.KObj(box), "exited", publicationErr.exited, "exitCode", publicationErr.exitCode)
			return true, RecordStartupDelivered(ctx, box, cli)
		}
		if verifyErr != nil || !publicationErr.exited || publicationErr.ambiguousEnd || publicationErr.exitCode == startupPublicationLockBusyExitCode {
			return false, errors.Join(err, verifyErr)
		}
		if attempt == 2 {
			return false, publicationErr.err
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, errors.Join(publicationErr.err, ctx.Err())
		case <-timer.C:
		}
	}
}
