/*
Copyright 2026 The Kruise Authors.

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

package mcp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/proto/envd/process"
)

// SandboxOperator defines the interface for sandbox operations
// This abstraction allows for dependency injection and easier testing
type SandboxOperator interface {
	ClaimSandbox(ctx context.Context, opts infra.ClaimSandboxOptions) (infra.Sandbox, error)
	GetClaimedSandbox(ctx context.Context, userID, sandboxID string) (infra.Sandbox, error)
}

// SandboxInfo contains basic sandbox information
type SandboxInfo struct {
	SandboxID   string
	AccessToken string
	State       string
}

// CommandResult represents the result of command execution
type CommandResult struct {
	PID      uint32
	Stdout   []string
	Stderr   []string
	ExitCode int32
	Exited   bool
	Error    error
}

// CreateSandbox creates a new sandbox for the user using the SandboxOperator interface
// sandboxTTL controls when the sandbox will be automatically reclaimed.
// If sandboxTTL is zero, no timeout will be applied.
func CreateSandbox(ctx context.Context, operator SandboxOperator, userID, sessionID, templateID string, sandboxTTL time.Duration) (*SandboxInfo, error) {
	log := klog.FromContext(ctx).WithValues("userID", userID, "templateID", templateID)

	accessToken := uuid.NewString()
	sbx, err := operator.ClaimSandbox(ctx, infra.ClaimSandboxOptions{
		User:     userID,
		Template: templateID,
		Modifier: func(sbx infra.Sandbox) {
			// Configure sandbox shutdown time based on sandboxTTL
			if sandboxTTL > 0 {
				now := time.Now()
				sbx.SetTimeout(infra.TimeoutOptions{
					ShutdownTime: now.Add(sandboxTTL),
				})
			}

			annotations := sbx.GetAnnotations()
			if annotations == nil {
				annotations = make(map[string]string)
			}
			annotations[v1alpha1.AnnotationMCPSessionID] = sessionID
			annotations[v1alpha1.AnnotationRuntimeAccessToken] = accessToken
			sbx.SetAnnotations(annotations)
		},
		InitRuntime: &config.InitRuntimeOptions{
			EnvVars:     models.EnvVars{},
			AccessToken: accessToken,
		},
	})
	if err != nil {
		log.Error(err, "failed to claim sandbox")
		return nil, fmt.Errorf("failed to claim sandbox: %w", err)
	}

	state, _ := sbx.GetState()
	log.Info("sandbox created", "sandboxID", sbx.GetSandboxID())
	return &SandboxInfo{
		SandboxID:   sbx.GetSandboxID(),
		AccessToken: accessToken,
		State:       state,
	}, nil
}

// GetSandbox retrieves sandbox information using the SandboxOperator interface
func GetSandbox(ctx context.Context, operator SandboxOperator, userID, sandboxID string) (*SandboxInfo, error) {
	sbx, err := operator.GetClaimedSandbox(ctx, userID, sandboxID)
	if err != nil {
		return nil, fmt.Errorf("sandbox not found: %w", err)
	}

	state, _ := sbx.GetState()
	return &SandboxInfo{
		SandboxID:   sbx.GetSandboxID(),
		AccessToken: sbx.GetAnnotations()[v1alpha1.AnnotationRuntimeAccessToken],
		State:       state,
	}, nil
}

// DeleteSandbox deletes a sandbox using the SandboxOperator interface
func DeleteSandbox(ctx context.Context, operator SandboxOperator, userID, sandboxID string) error {
	log := klog.FromContext(ctx).WithValues("userID", userID, "sandboxID", sandboxID)

	sbx, err := operator.GetClaimedSandbox(ctx, userID, sandboxID)
	if err != nil {
		log.Error(err, "failed to get sandbox")
		return fmt.Errorf("sandbox not found: %w", err)
	}

	if err := sbx.Kill(ctx); err != nil {
		log.Error(err, "failed to delete sandbox")
		return fmt.Errorf("failed to delete sandbox: %w", err)
	}

	log.Info("sandbox deleted successfully")
	return nil
}

// RequestToSandbox sends an HTTP request to the sandbox
func RequestToSandbox(ctx context.Context, operator SandboxOperator, userID, sandboxID string, method, path string, port int, body io.Reader, headers http.Header) (*http.Response, error) {
	sbx, err := operator.GetClaimedSandbox(ctx, userID, sandboxID)
	if err != nil {
		return nil, fmt.Errorf("sandbox not found: %w", err)
	}

	return sbx.Request(ctx, method, path, port, body, headers)
}

// ExecuteCommand executes a shell command in the sandbox via gRPC
// Aligned with E2B Commands.run() interface - commands are run via /bin/bash -l -c
func ExecuteCommand(ctx context.Context, operator SandboxOperator, userID, sandboxID string, cmd string, envs map[string]string, cwd *string, timeout time.Duration) (*CommandResult, error) {
	log := klog.FromContext(ctx).WithValues("userID", userID, "sandboxID", sandboxID, "cmd", cmd)

	sbx, err := operator.GetClaimedSandbox(ctx, userID, sandboxID)
	if err != nil {
		return nil, fmt.Errorf("sandbox not found: %w", err)
	}

	// Type assert to sandboxcr.Sandbox to access runCommandWithRuntime
	sbxImpl, ok := sbx.(*sandboxcr.Sandbox)
	if !ok {
		return nil, fmt.Errorf("unexpected sandbox implementation type")
	}

	// Build process config following E2B pattern:
	// Commands are run via /bin/bash -l -c "<cmd>"
	processConfig := &process.ProcessConfig{
		Cmd:  "/bin/bash",
		Args: []string{"-l", "-c", cmd},
		Envs: envs,
		Cwd:  cwd,
	}

	log.Info("executing command in sandbox")

	result, err := sbxImpl.RunCommandWithRuntime(ctx, processConfig, timeout)
	if err != nil {
		log.Error(err, "command execution failed")
		return nil, err
	}

	log.Info("command execution completed", "exitCode", result.ExitCode, "exited", result.Exited)

	// Convert to MCP CommandResult
	return &CommandResult{
		PID:      result.PID,
		Stdout:   result.Stdout,
		Stderr:   result.Stderr,
		ExitCode: result.ExitCode,
		Exited:   result.Exited,
		Error:    result.Error,
	}, result.Error
}
