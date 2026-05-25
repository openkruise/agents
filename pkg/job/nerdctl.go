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

type cmdOpt func(cmd *exec.Cmd)

func WithArgs(args ...string) cmdOpt {
	return func(cmd *exec.Cmd) {
		cmd.Args = append(cmd.Args, args...)
	}
}

func WithStdout(o io.Writer) cmdOpt {
	return func(cmd *exec.Cmd) {
		cmd.Stdout = o
	}
}

// NerdctlExec executes a nerdctl command with the given options.
func NerdctlExec(ctx context.Context, opts ...cmdOpt) error {
	presetArgs := []string{
		"--debug",
		"--namespace=k8s.io",
		fmt.Sprintf("--host=%s", Config().ContainerdSock()),
	}
	cmd := exec.Command("nerdctl", presetArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{}
	for _, opt := range opts {
		opt(cmd)
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
