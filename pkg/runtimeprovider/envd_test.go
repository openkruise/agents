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
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openkruise/agents/pkg/utils/runtime"
	runtimeconfig "github.com/openkruise/agents/pkg/utils/runtime/config"
	"github.com/openkruise/agents/proto/envd/filesystem"
)

// fakeEnvdRuntime is a minimal envdRuntime double. A double is used here
// (rather than a real envd sidecar) because envdProvider's job is pure
// translation between runtimeprovider's protocol-neutral types and envd's
// wire types; the translation is what these tests verify, not envd itself.
type fakeEnvdRuntime struct {
	initOpts  runtimeconfig.InitRuntimeOptions
	initErr   error
	runReq    runtime.RunCommandRequest
	runResult runtime.RunCommandResult
	runErr    error
	writeReq  runtime.WriteFileRequest
	writeErr  error
}

func (f *fakeEnvdRuntime) Init() runtime.InitAPI             { return fakeInitAPI{f} }
func (f *fakeEnvdRuntime) Process() runtime.ProcessAPI       { return fakeProcessAPI{f} }
func (f *fakeEnvdRuntime) Filesystem() runtime.FilesystemAPI { return fakeFilesystemAPI{f} }

type fakeInitAPI struct{ f *fakeEnvdRuntime }

func (a fakeInitAPI) Init(_ context.Context, opts runtimeconfig.InitRuntimeOptions) error {
	a.f.initOpts = opts
	return a.f.initErr
}

type fakeProcessAPI struct{ f *fakeEnvdRuntime }

func (a fakeProcessAPI) Run(_ context.Context, req runtime.RunCommandRequest) (runtime.RunCommandResult, error) {
	a.f.runReq = req
	return a.f.runResult, a.f.runErr
}

func (a fakeProcessAPI) Chmod(context.Context, string, string) error {
	return errors.New("not implemented in fake")
}

type fakeFilesystemAPI struct{ f *fakeEnvdRuntime }

func (a fakeFilesystemAPI) Write(_ context.Context, req runtime.WriteFileRequest) (runtime.WriteFileResult, error) {
	a.f.writeReq = req
	return runtime.WriteFileResult{StatusCode: 200}, a.f.writeErr
}

func (a fakeFilesystemAPI) ListDir(context.Context, runtime.ListDirRequest) ([]*filesystem.EntryInfo, error) {
	return nil, errors.New("not implemented in fake")
}

func (a fakeFilesystemAPI) Remove(context.Context, runtime.RemovePathRequest) error {
	return errors.New("not implemented in fake")
}

func TestEnvdProvider_Init(t *testing.T) {
	fake := &fakeEnvdRuntime{}
	p := &envdProvider{rt: fake}

	err := p.Init(t.Context(), InitOptions{
		EnvVars:     map[string]string{"FOO": "bar"},
		AccessToken: "tok",
		ReInit:      true,
	})
	require.NoError(t, err)
	assert.Equal(t, "tok", fake.initOpts.AccessToken)
	assert.Equal(t, map[string]string{"FOO": "bar"}, fake.initOpts.EnvVars)
	assert.True(t, fake.initOpts.ReInit)
}

func TestEnvdProvider_Exec(t *testing.T) {
	fake := &fakeEnvdRuntime{
		runResult: runtime.RunCommandResult{
			Stdout:   []string{"hello ", "world"},
			Stderr:   []string{"warn"},
			ExitCode: 0,
			Exited:   true,
		},
	}
	p := &envdProvider{rt: fake}

	result, err := p.Exec(t.Context(), ExecOptions{
		Cmd:  "echo",
		Args: []string{"hi"},
		Cwd:  "/work",
	})
	require.NoError(t, err)
	assert.Equal(t, "hello world", result.Stdout)
	assert.Equal(t, "warn", result.Stderr)
	assert.True(t, result.Exited)
	require.NotNil(t, fake.runReq.ProcessConfig.Cwd)
	assert.Equal(t, "/work", *fake.runReq.ProcessConfig.Cwd)
	assert.Equal(t, "echo", fake.runReq.ProcessConfig.Cmd)
}

func TestEnvdProvider_WriteFile(t *testing.T) {
	fake := &fakeEnvdRuntime{}
	p := &envdProvider{rt: fake}

	err := p.WriteFile(t.Context(), WriteFileOptions{
		Path:    "/tmp/a.txt",
		Content: strings.NewReader("payload"),
	})
	require.NoError(t, err)
	assert.Equal(t, "/tmp/a.txt", fake.writeReq.FilePath)
	assert.Equal(t, []byte("payload"), fake.writeReq.Content)
}

// ReadFile is deliberately unsupported by the envd control-plane client (see
// the comment on envdProvider.ReadFile); this pins that contract.
func TestEnvdProvider_ReadFile_Unsupported(t *testing.T) {
	p := &envdProvider{rt: &fakeEnvdRuntime{}}
	_, err := p.ReadFile(t.Context(), ReadFileOptions{Path: "/tmp/a.txt"})
	require.Error(t, err)
}
