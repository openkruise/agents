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

package runtimeprovider

import (
	"context"
	"fmt"
	"io"
	"strings"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils/runtime"
	runtimeconfig "github.com/openkruise/agents/pkg/utils/runtime/config"
	"github.com/openkruise/agents/proto/envd/process"
)

func init() {
	register(KindEnvd, newEnvdProvider)
}

// envdRuntime is the subset of runtime.Runtime this adapter depends on. It
// exists so tests can substitute a fake without standing up a real envd
// sidecar; pkg/utils/runtime.Runtime itself already satisfies it.
type envdRuntime interface {
	Init() runtime.InitAPI
	Process() runtime.ProcessAPI
	Filesystem() runtime.FilesystemAPI
}

// envdProvider adapts pkg/utils/runtime's envd client to the protocol-neutral
// Provider interface. It performs no protocol logic of its own: every call is
// a straight translation to/from the envd-specific request/result types.
type envdProvider struct {
	rt envdRuntime
}

// NewEnvdProvider wraps an already-constructed envd runtime.Runtime (bound to
// a specific Sandbox) as a Provider. This is the constructor callers use when
// they already build a runtime.Runtime through the existing
// pkg/utils/runtime machinery (e.g. TLS bundle resolution during claim/clone);
// NewProvider(KindEnvd, ...) is for the simpler case of talking to a sandbox
// by endpoint alone.
func NewEnvdProvider(rt runtime.Runtime) Provider {
	return &envdProvider{rt: rt}
}

// newEnvdProvider is the Factory registered for KindEnvd. It builds a
// runtime.Runtime bound to a minimal Sandbox shell carrying only enough
// addressing for runtime.GetRuntimeURL to resolve: a full "scheme://host:port"
// endpoint is stored as the runtime-URL annotation verbatim, while a bare host
// is stored as the pod IP and combined with the well-known runtime port, same
// as the sandbox-manager claim/clone path does for a freshly claimed pod.
func newEnvdProvider(endpoint string, opts ...Option) (Provider, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("runtimeprovider: envd: empty endpoint")
	}
	s := newSettings(opts)
	sbx := &agentsv1alpha1.Sandbox{}
	if strings.Contains(endpoint, "://") {
		sbx.Annotations = map[string]string{agentsv1alpha1.AnnotationRuntimeURL: endpoint}
	} else {
		sbx.Status.PodInfo.PodIP = endpoint
	}
	rtOpts := []runtime.Option{runtime.WithRequestTimeout(s.timeout)}
	return &envdProvider{rt: runtime.NewRuntime(sbx, rtOpts...)}, nil
}

func (p *envdProvider) Kind() Kind { return KindEnvd }

func (p *envdProvider) Init(ctx context.Context, opts InitOptions) error {
	return p.rt.Init().Init(ctx, runtimeconfig.InitRuntimeOptions{
		EnvVars:     opts.EnvVars,
		AccessToken: opts.AccessToken,
		ReInit:      opts.ReInit,
	})
}

func (p *envdProvider) Exec(ctx context.Context, opts ExecOptions) (ExecResult, error) {
	var cwd *string
	if opts.Cwd != "" {
		cwd = &opts.Cwd
	}
	result, err := p.rt.Process().Run(ctx, runtime.RunCommandRequest{
		ProcessConfig: &process.ProcessConfig{
			Cmd:  opts.Cmd,
			Args: opts.Args,
			Envs: opts.Envs,
			Cwd:  cwd,
		},
		AuthUser: opts.AuthUser,
	})
	if err != nil {
		return ExecResult{}, err
	}
	return ExecResult{
		Stdout:   strings.Join(result.Stdout, ""),
		Stderr:   strings.Join(result.Stderr, ""),
		ExitCode: result.ExitCode,
		Exited:   result.Exited,
	}, result.Error
}

func (p *envdProvider) WriteFile(ctx context.Context, opts WriteFileOptions) error {
	content, err := io.ReadAll(opts.Content)
	if err != nil {
		return fmt.Errorf("runtimeprovider: envd: read content: %w", err)
	}
	_, err = p.rt.Filesystem().Write(ctx, runtime.WriteFileRequest{
		FilePath: opts.Path,
		Content:  content,
		AuthUser: opts.AuthUser,
	})
	return err
}

// ReadFile is not exposed by the envd Filesystem service used elsewhere in
// this repository (only Write, ListDir, and Remove are); envd instead serves
// file content through the sandbox-gateway data plane. Returning an
// unsupported error here is honest about that gap rather than silently
// returning an empty file.
func (p *envdProvider) ReadFile(_ context.Context, opts ReadFileOptions) (ReadFileResult, error) {
	return ReadFileResult{}, fmt.Errorf("runtimeprovider: envd: ReadFile is not supported by the envd control-plane client; read %q through the sandbox-gateway data plane instead", opts.Path)
}
