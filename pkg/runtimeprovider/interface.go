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

// Package runtimeprovider defines a protocol-neutral abstraction over the
// in-sandbox execution daemon (command execution, file I/O, and runtime
// initialization) so that a Sandbox is not hard-wired to a single daemon
// protocol.
//
// Two protocols are known today:
//   - envd: the E2B-compatible daemon already spoken by
//     [github.com/openkruise/agents/pkg/utils/runtime], reached over
//     connect-RPC (proto/envd) plus a JSON /init handshake.
//   - execd: the OpenSandbox-compatible daemon, reached over a plain JSON/SSE
//     REST API (see specs/execd-api.yaml in the OpenSandbox project).
//
// A Provider is bound to one sandbox at construction time, mirroring
// pkg/utils/runtime.Runtime. Callers select an implementation once (per
// sandbox, at claim/clone time) via NewProvider and then depend only on this
// package's types, never on envd- or execd-specific wire types. This keeps the
// selection a construction-time concern rather than something every call site
// has to branch on.
package runtimeprovider

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// Kind identifies which concrete daemon protocol a Provider speaks.
type Kind string

const (
	// KindEnvd selects the E2B-compatible envd daemon.
	KindEnvd Kind = "envd"
	// KindExecd selects the OpenSandbox-compatible execd daemon.
	KindExecd Kind = "execd"
)

// DefaultKind is used when a sandbox does not request a runtime explicitly.
// envd remains the default because it is the runtime every existing template
// ships today; execd is opt-in until templates carry it.
const DefaultKind = KindEnvd

// ErrUnknownKind is returned by NewProvider when Kind names a protocol with no
// registered factory.
var ErrUnknownKind = errors.New("runtimeprovider: unknown runtime kind")

// InitOptions carries the data the daemon needs to accept the initial
// handshake: environment variables materialized into the sandbox process tree
// and the access token that authenticates subsequent Exec/file calls.
type InitOptions struct {
	EnvVars     map[string]string
	AccessToken string
	// ReInit indicates a re-handshake against an already-initialized daemon
	// (e.g. after resume). Implementations must treat "already initialized" as
	// success rather than an error when ReInit is set.
	ReInit bool
}

// ExecOptions describes a single command execution.
type ExecOptions struct {
	// Cmd is the executable to run. It is not passed through a shell, so a
	// caller needing shell semantics (pipes, globs) must invoke a shell
	// explicitly, e.g. Cmd: "/bin/sh", Args: []string{"-c", "..."}.
	Cmd  string
	Args []string
	Envs map[string]string
	// Cwd is the working directory for the command. Empty means the daemon's
	// default (typically the sandbox user's home directory).
	Cwd string
	// AuthUser is the OS user identity the daemon should run the command as.
	// Empty means the daemon's default user.
	AuthUser string
}

// ExecResult is the outcome of a completed Exec call. Providers only return
// this type after the command has exited (or the context timed out); neither
// implementation currently exposes incremental streaming through this
// interface, since no caller of the interface needs it yet.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int32
	// Exited is false when the command timed out or was interrupted before it
	// reported an exit code.
	Exited bool
}

// WriteFileOptions describes a single file write.
type WriteFileOptions struct {
	// Path is the absolute path inside the sandbox to write.
	Path string
	// Content is read fully and uploaded as the new file body, unconditionally
	// overwriting any pre-existing file at Path.
	Content io.Reader
	// AuthUser is the OS user the daemon should use when writing the file.
	AuthUser string
}

// ReadFileOptions describes a single file read.
type ReadFileOptions struct {
	// Path is the absolute path inside the sandbox to read.
	Path     string
	AuthUser string
}

// ReadFileResult carries a file's content. Callers must Close it.
type ReadFileResult struct {
	Content io.ReadCloser
	// Size is the payload size in bytes when known, else -1.
	Size int64
}

// Provider abstracts command execution, file I/O, and runtime initialization
// against one sandbox's in-pod execution daemon, independent of which daemon
// protocol (envd, execd, ...) actually backs it.
type Provider interface {
	// Kind reports which protocol this Provider speaks, e.g. for logging.
	Kind() Kind
	// Init performs the daemon handshake described by opts.
	Init(ctx context.Context, opts InitOptions) error
	// Exec runs a command to completion inside the sandbox and returns its
	// output and exit status.
	Exec(ctx context.Context, opts ExecOptions) (ExecResult, error)
	// WriteFile writes a file inside the sandbox.
	WriteFile(ctx context.Context, opts WriteFileOptions) error
	// ReadFile reads a file from inside the sandbox.
	ReadFile(ctx context.Context, opts ReadFileOptions) (ReadFileResult, error)
}

// Factory builds a Provider bound to the given endpoint. endpoint is the
// scheme-less host:port (or host) at which the daemon is reachable; how it is
// resolved (plaintext, TLS, pinned dial IP) is the caller's concern, mirroring
// pkg/utils/runtime's transport resolution, and is passed in fully resolved.
type Factory func(endpoint string, opts ...Option) (Provider, error)

// registry maps a Kind to the Factory that builds it. Populated by the envd
// and execd sub-files' init() functions so importing this package alone (with
// neither daemon package) yields an empty, safely-failing registry.
var registry = map[Kind]Factory{}

// register is called from each implementation's init() function.
func register(kind Kind, f Factory) {
	registry[kind] = f
}

// NewProvider builds the Provider registered for kind. An empty kind selects
// DefaultKind so per-sandbox configuration can omit the field and still get
// envd, matching every template shipped before execd existed.
func NewProvider(kind Kind, endpoint string, opts ...Option) (Provider, error) {
	if kind == "" {
		kind = DefaultKind
	}
	factory, ok := registry[kind]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}
	return factory(endpoint, opts...)
}
