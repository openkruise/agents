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

package job

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"

	"k8s.io/klog/v2"
)

// CmdOpt is a functional option for configuring an exec.Cmd.
type CmdOpt func(cmd *exec.Cmd)

// WithArgs appends additional arguments to the command.
func WithArgs(args ...string) CmdOpt {
	return func(cmd *exec.Cmd) {
		cmd.Args = append(cmd.Args, args...)
	}
}

// WithBinary overrides the binary path (for testing).
func WithBinary(bin string) CmdOpt {
	return func(cmd *exec.Cmd) {
		cmd.Path = bin
	}
}

// NerdctlExec executes a nerdctl command with the given options.
func NerdctlExec(ctx context.Context, opts ...CmdOpt) error {
	presetArgs := []string{
		"nerdctl",
		"--debug",
		"--namespace=k8s.io",
		fmt.Sprintf("--host=%s", Config().ContainerdSock()),
		fmt.Sprintf("--hosts-dir=%s", DefaultNerdctlHostsDir),
	}
	cmd := &exec.Cmd{
		Args:        presetArgs,
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
		SysProcAttr: &syscall.SysProcAttr{},
	}
	for _, opt := range opts {
		opt(cmd)
	}
	// Resolve binary path after opts (WithBinary may override cmd.Path)
	if cmd.Path == "" {
		p, err := exec.LookPath("nerdctl")
		if err != nil {
			return fmt.Errorf("nerdctl binary not found: %w", err)
		}
		cmd.Path = p
	}
	stdErrBuf := &bytes.Buffer{}
	cmd.Stderr = io.MultiWriter(cmd.Stderr, stdErrBuf)

	klog.InfoS("nerdctl CMD", "args", cmd.Args)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start cmd failed: %w", err)
	}

	exit := make(chan struct{})
	defer close(exit)
	go func() {
		select {
		case <-ctx.Done():
			if p := cmd.Process; p != nil {
				_ = p.Signal(syscall.SIGTERM)
			}
		case <-exit:
		}
	}()

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("nerdctl output: %q (%w)", stdErrBuf.String(), err)
	}
	return nil
}
