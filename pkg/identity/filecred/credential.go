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

// Package filecred delivers an issued sandbox ID token into the sandbox as a
// credential file, and removes it again when the sandbox stops belonging to the
// claim it was issued for.
//
// It is the open-source implementation of the propagation half of the identity
// provider contract described in pkg/identity: SecurityTokenPropagator names
// WriteFileWithRuntime and ChmodFileOnRuntime as the delivery path, and
// SecurityTokenCleaner its counterpart. This package supplies both, defined
// together, so a deployment that opts into file credentials gets the write and
// the removal from one place.
//
// Nothing here registers itself. A deployment builds a Credential with the path
// and mode it wants and calls RegisterPropagator, which is the same posture the
// identity registry documents.
package filecred

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"time"

	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
	agentsruntime "github.com/openkruise/agents/pkg/utils/runtime"
)

const (
	// defaultMode is the file mode a credential gets when Config leaves it unset.
	// A token readable by other users in the sandbox is a credential leak inside
	// the sandbox itself, so the default is owner-only.
	defaultMode = "0600"
	// defaultAuthUser is the user identity the runtime resolves the write, chmod
	// and removal against when Config leaves it unset. It matches the reset
	// signal write in the recycle path, which also targets a root-owned
	// directory.
	defaultAuthUser = "root"
	// defaultTimeout bounds each individual runtime call.
	defaultTimeout = 10 * time.Second
)

// modePattern accepts three octal permission digits with an optional leading
// zero, the form ChmodFileOnRuntime passes through to `chmod`. Setuid, setgid
// and sticky bits are deliberately outside it: a credential file has no business
// carrying them.
var modePattern = regexp.MustCompile(`^0?[0-7]{3}$`)

// Config describes where a credential lives inside the sandbox and who owns it.
type Config struct {
	// Path is the absolute path of the credential file inside the sandbox. Its
	// parent directory must already exist, because the runtime files API does not
	// create one.
	Path string
	// Mode is the octal file mode applied after the write, for example "0600".
	// Empty means defaultMode.
	Mode string
	// AuthUser is the user identity the runtime resolves each call against.
	// Empty means defaultAuthUser.
	AuthUser string
	// Timeout bounds each individual runtime call. Zero means defaultTimeout.
	Timeout time.Duration
}

// Credential propagates and removes one credential file.
type Credential struct {
	cfg Config
	// fileMode is cfg.Mode parsed once, so the mode declared on the write and the
	// mode the chmod applies cannot drift apart.
	fileMode os.FileMode
}

// New validates cfg and returns the Credential it describes.
func New(cfg Config) (*Credential, error) {
	if !path.IsAbs(cfg.Path) {
		return nil, fmt.Errorf("credential path must be absolute, got %q", cfg.Path)
	}
	if path.Clean(cfg.Path) != cfg.Path {
		return nil, fmt.Errorf("credential path must be clean, got %q", cfg.Path)
	}
	if cfg.Mode == "" {
		cfg.Mode = defaultMode
	}
	if !modePattern.MatchString(cfg.Mode) {
		return nil, fmt.Errorf("credential mode must be three octal digits with an optional leading zero, got %q", cfg.Mode)
	}
	if cfg.AuthUser == "" {
		cfg.AuthUser = defaultAuthUser
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	// modePattern has already established the shape, so the three permission
	// digits convert without a second failure path.
	var fileMode os.FileMode
	for _, digit := range cfg.Mode[len(cfg.Mode)-3:] {
		fileMode = fileMode<<3 | os.FileMode(digit-'0')
	}
	return &Credential{cfg: cfg, fileMode: fileMode}, nil
}

// RegisterPropagator installs this credential's write half in the identity
// propagator registry. Like every other registration in pkg/identity it belongs
// in init() or startup wiring, because the registry is read concurrently
// afterwards and is not safe to modify at runtime.
//
// Cleanup has no matching registry to join yet. #786 adds one, and a
// RegisterCleaner beside this is a three line follow-up once it lands. Until
// then a deployment that wants removal calls Cleanup from its own lifecycle
// handling, which is why it is exported and tested here rather than kept
// private until the registry exists.
func (c *Credential) RegisterPropagator() {
	identity.RegisterSecurityTokenPropagator(c.Propagate)
}

// Propagate writes the issued token to the configured path and applies the
// configured mode. It satisfies identity.SecurityTokenPropagator.
//
// The mode needs the second call: WriteFileArgs.Permissions is documented as not
// transmitted today, so a caller that needs an exact mode has to follow the write
// with ChmodFileOnRuntime. A write whose chmod fails is reported as a failure and
// leaves the file in place, since the caller has to know the credential is not
// yet protected.
//
// rtOpts is forwarded to both calls unchanged, so the credential travels over the
// transport the caller resolved for this sandbox.
func (c *Credential) Propagate(ctx context.Context, sbx *agentsv1alpha1.Sandbox, tokenResp *identity.TokenResponse,
	rtOpts ...agentsruntime.Option) error {
	if sbx == nil {
		return fmt.Errorf("sandbox is nil")
	}
	if tokenResp == nil || tokenResp.AccessToken == "" {
		return fmt.Errorf("token response carries no token to propagate")
	}
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx), "path", c.cfg.Path)

	if _, err := agentsruntime.WriteFileWithRuntime(ctx, agentsruntime.WriteFileArgs{
		Sbx:         sbx,
		FilePath:    c.cfg.Path,
		Content:     []byte(tokenResp.AccessToken),
		AuthUser:    c.cfg.AuthUser,
		Timeout:     c.cfg.Timeout,
		Permissions: c.fileMode,
	}, rtOpts...); err != nil {
		return fmt.Errorf("failed to write credential to %s: %w", c.cfg.Path, err)
	}

	// The write lands at the runtime's default mode, so between the two calls the
	// credential is readable by anything else in the sandbox. That window is a
	// full round trip wide and cannot be closed here: the runtime files API has
	// no create-with-mode and no write-to-temp-then-rename to build one from.
	//
	// A chmod failure is the same exposure without an end, so the file is removed
	// before returning. Propagation has failed either way, and a token sitting at
	// the default mode is worse than no token: the caller retries, but nothing
	// else would ever come back for this file. A failed removal is reported
	// alongside the original error rather than replacing it, since the chmod is
	// what the caller needs to see first.
	if err := agentsruntime.ChmodFileOnRuntime(ctx, sbx, c.cfg.Path, c.cfg.Mode, rtOpts...); err != nil {
		if rmErr := c.Cleanup(ctx, sbx, rtOpts...); rmErr != nil {
			return fmt.Errorf("wrote credential to %s but failed to set mode %s (%w), and removing it also failed: %v",
				c.cfg.Path, c.cfg.Mode, err, rmErr)
		}
		return fmt.Errorf("wrote credential to %s but failed to set mode %s, credential removed: %w",
			c.cfg.Path, c.cfg.Mode, err)
	}

	log.Info("propagated security credential into sandbox", "mode", c.cfg.Mode)
	return nil
}

// Cleanup removes the credential file, the counterpart to Propagate.
//
// A path that is already gone counts as success. RemovePathWithRuntime documents
// that removal is not idempotent and reports a missing path as
// ErrRuntimePathNotFound, so any caller that can run twice has to ignore it with
// errors.Is. A cleaner runs on lifecycle events that retry, and it may run after a
// partial propagation that never wrote the file, so it meets that case normally.
//
// A runtime too old to serve the Filesystem service is also success. The
// credential cannot exist on a runtime that never accepted the write.
func (c *Credential) Cleanup(ctx context.Context, sbx *agentsv1alpha1.Sandbox,
	rtOpts ...agentsruntime.Option) error {
	if sbx == nil {
		return fmt.Errorf("sandbox is nil")
	}
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx), "path", c.cfg.Path)

	err := agentsruntime.RemovePathWithRuntime(ctx, agentsruntime.RemovePathArgs{
		Sbx:      sbx,
		Path:     c.cfg.Path,
		AuthUser: c.cfg.AuthUser,
		Timeout:  c.cfg.Timeout,
	}, rtOpts...)
	switch {
	case err == nil:
		log.Info("removed security credential from sandbox")
		return nil
	case errors.Is(err, agentsruntime.ErrRuntimePathNotFound):
		log.V(4).Info("security credential already absent")
		return nil
	case errors.Is(err, agentsruntime.ErrRuntimeFilesystemUnsupported):
		log.V(4).Info("sandbox runtime does not serve the filesystem service, nothing to remove")
		return nil
	default:
		return fmt.Errorf("failed to remove credential at %s: %w", c.cfg.Path, err)
	}
}
