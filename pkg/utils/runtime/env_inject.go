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
	"fmt"
	"regexp"
	"sort"
	"strings"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/runtime/config"
)

var startupEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func ValidateStartupData(data config.StartupData) error {
	if len(data.EnvVars) > 32 {
		return fmt.Errorf("at most 32 startup environment variables are allowed")
	}
	total := 0
	for name, value := range data.EnvVars {
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
		if strings.ContainsRune(value, '\x00') || len(value) > 4*1024 {
			return fmt.Errorf("startup environment variable %q contains NUL or exceeds 4096 bytes", name)
		}
		total += len(name) + len(value)
	}
	if len(data.Command) > 0 && data.Command[0] == "" {
		return fmt.Errorf("startup command executable must not be empty")
	}
	for _, arg := range data.Command {
		if strings.ContainsRune(arg, '\x00') {
			return fmt.Errorf("startup command contains NUL")
		}
		total += len(arg)
	}
	if total > 32*1024 {
		return fmt.Errorf("startup data exceeds 32768 bytes")
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

func InjectStartupData(ctx context.Context, box *agentsv1alpha1.Sandbox, data config.StartupData, opts ...Option) error {
	if err := ValidateStartupData(data); err != nil {
		return err
	}
	if box.Status.PodInfo.PodUID == "" {
		return fmt.Errorf("sandbox has no pod UID for startup delivery")
	}
	env, command := startupScripts(data)
	mode := "0600"
	for _, file := range []struct{ name, content string }{{"env.sh", env}, {"cmd.sh", command}} {
		if file.content == "" {
			continue
		}
		path := utils.SandboxEnvClaimDir + "/" + file.name
		if _, err := WriteFileWithRuntime(ctx, WriteFileArgs{
			Sbx: box, FilePath: path, Content: []byte(file.content), Username: defaultRuntimeFilesUsername, AuthUser: defaultRuntimeFilesUsername,
		}, opts...); err != nil {
			return fmt.Errorf("write %s: %w", file.name, err)
		}
		if err := ChmodFileOnRuntime(ctx, box, path, mode, opts...); err != nil {
			return fmt.Errorf("set %s permissions: %w", file.name, err)
		}
	}
	// An absent claim command must leave cmd.sh absent; the wrapper rejects an empty file.
	_, err := WriteFileWithRuntime(ctx, WriteFileArgs{
		Sbx: box, FilePath: utils.SandboxEnvClaimDir + "/.env_ready",
		Content: []byte(box.Status.PodInfo.PodUID), Username: defaultRuntimeFilesUsername, AuthUser: defaultRuntimeFilesUsername,
	}, opts...)
	if err != nil {
		return fmt.Errorf("write startup signal: %w", err)
	}
	return nil
}
