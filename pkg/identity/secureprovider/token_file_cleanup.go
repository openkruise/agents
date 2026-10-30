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

package secureprovider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils/runtime"
	"github.com/openkruise/agents/proto/envd/filesystem"
)

// securityTokenPruneAuthUser is the runtime user the prune runs as. The token
// directory and its files are owned by root, so listing and removal must be
// performed as root — mirroring the identity the other runtime helpers use.
const securityTokenPruneAuthUser = "root"

// securityTokenFileSuffix is the suffix every security token file carries. It is the
// sole criterion for deciding whether a leftover entry in the token directory belongs
// to this mechanism.
//
// Matching by suffix rather than by the exact "<md5>.token" shape is deliberate: token
// files produced by earlier naming schemes must be cleaned up too, since the runtime
// resolves a sandbox identity from whatever token file it finds. Entries that are not
// token files are left untouched, which keeps the blast radius inside this mechanism's
// own namespace even though the directory is not formally owned by it.
const securityTokenFileSuffix = ".token"

// pruneStaleSecurityTokenFilesFn indirects pruneStaleSecurityTokenFiles so tests can
// assert the propagator's ordering and failure handling without standing up a
// ConnectRPC filesystem server. Production code never reassigns it.
var pruneStaleSecurityTokenFilesFn = pruneStaleSecurityTokenFiles

// pruneStaleSecurityTokenFiles removes every security token file under
// DefaultSecurityTokenFilePath except keepFileName.
//
// It exists because the token directory lives in the container's writable layer: a
// sandbox cloned from a checkpoint comes back with the *source* sandbox's
// "<md5>.token" file. Since the runtime side derives the sandbox identity from that
// file name, a leftover file lets a sandbox be resolved as a different one, so the
// directory must hold exactly one token file — the one belonging to this sandbox.
//
// The work is split into a read-only listing and a removal of exact paths via the
// envd Filesystem service, rather than a single destructive command. That ordering
// means the common case (nothing stale) costs exactly one call and touches nothing,
// and the removal names the precise files it deletes so operators can audit it from
// the logs.
//
// Only regular files (and symlinks) are removed; directories are skipped, because the
// Filesystem Remove RPC deletes recursively and the entry type is available from the
// listing anyway. keepFileName is the base name of the file the propagator has just
// written; excluding it guarantees the current credential is never removed, so a live
// agent never observes an empty token directory — including on the periodic refresh
// path, where propagation happens while the agent is running.
//
// Failures are returned to the caller rather than logged and swallowed: leaving a
// foreign token file behind would let the runtime resolve a wrong identity, which is
// worse than failing the propagation and letting the caller retry. Two outcomes are
// benign: a token directory that does not exist yet (ErrRuntimePathNotFound, handled
// here) and a runtime that does not serve the Filesystem service
// (ErrRuntimeFilesystemUnsupported, wrapped and left for the caller to degrade on).
//
// rtOpts is the transport resolved for the sandbox and is applied to both the
// listing and the removals, so the prune reaches a TLS-capable runtime over the
// same HTTPS transport that carried the token write it follows.
func pruneStaleSecurityTokenFiles(ctx context.Context, sbx *agentsv1alpha1.Sandbox, keepFileName string,
	rtOpts ...runtime.Option) error {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx))

	dir, err := resolvePruneTarget(DefaultSecurityTokenFilePath, keepFileName)
	if err != nil {
		return fmt.Errorf("refusing to prune security token files: %w", err)
	}

	entries, err := runtime.ListDirWithRuntime(ctx, runtime.ListDirArgs{
		Sbx:      sbx,
		Path:     dir,
		AuthUser: securityTokenPruneAuthUser,
	}, rtOpts...)
	if err != nil {
		// A directory that does not exist cannot hold a stale token file. This is not
		// expected after a successful write, but it is unambiguously benign.
		if errors.Is(err, runtime.ErrRuntimePathNotFound) {
			log.Info("security token directory absent, nothing to prune", "dir", dir)
			return nil
		}
		// ErrRuntimeFilesystemUnsupported is preserved through %w so the caller can
		// degrade on an older runtime instead of failing propagation.
		return fmt.Errorf("failed to list security token directory %s: %w", dir, err)
	}

	stale := selectStaleSecurityTokenFiles(entries, keepFileName)
	if len(stale) == 0 {
		return nil
	}

	log.Info("removing stale security token files", "count", len(stale), "kept", keepFileName)
	for _, p := range stale {
		if err := runtime.RemovePathWithRuntime(ctx, runtime.RemovePathArgs{
			Sbx:      sbx,
			Path:     p,
			AuthUser: securityTokenPruneAuthUser,
		}, rtOpts...); err != nil {
			return fmt.Errorf("failed to remove stale security token file %s: %w", p, err)
		}
		// Logged per path and only after the RPC returned, so the line records a deletion
		// that actually happened. A summary printed before or after the loop would claim
		// the whole set even when a later removal aborts the prune half way through.
		log.Info("removed stale security token file", "path", p)
	}
	return nil
}

// selectStaleSecurityTokenFiles narrows directory entries down to the token files that
// belong to another sandbox: every regular-file entry whose base name carries the token
// suffix, except the file the propagator has just written.
//
// The type check is a whitelist on purpose: only FILE_TYPE_FILE is acted on. The removal
// that follows runs as root and the Remove RPC deletes recursively, so any entry this
// side cannot positively identify as a regular file is left alone — a directory that
// merely happens to be named like a token file, and everything the service reports as
// FILE_TYPE_UNSPECIFIED (sockets, devices, FIFOs, dangling symlinks, plus any type a
// future runtime classifies differently). None of those can be a token file this
// mechanism wrote, so skipping them costs nothing and keeps a misclassified entry from
// ever being handed to a recursive delete.
//
// A symlink whose target resolves to a regular file still arrives as FILE_TYPE_FILE,
// because the service reports the type of the target rather than of the link. That is
// intended: the runtime resolves identity from the file name, so a symlink named like a
// token file is just as misleading as a regular one.
func selectStaleSecurityTokenFiles(entries []*filesystem.EntryInfo, keepFileName string) []string {
	var stale []string
	for _, entry := range entries {
		if entry.GetType() != filesystem.FileType_FILE_TYPE_FILE {
			continue
		}
		name := entry.GetName()
		if name == keepFileName || !isSecurityTokenFileName(name) {
			continue
		}
		stale = append(stale, entry.GetPath())
	}
	return stale
}

// isSecurityTokenFileName reports whether name looks like a security token file, i.e.
// carries the token suffix preceded by at least one character.
func isSecurityTokenFileName(name string) bool {
	return len(name) > len(securityTokenFileSuffix) && strings.HasSuffix(name, securityTokenFileSuffix)
}

// resolvePruneTarget validates and normalizes the directory to prune, rejecting any
// input that could widen the blast radius of the removal that follows.
//
// The guard is deliberately paranoid rather than merely sufficient: today's only caller
// derives keepFileName from an MD5 digest and dir from a constant, but the removal runs
// as root, and a silently degenerate keepFileName would stop excluding the token file
// just written — turning the prune into a deletion of the live credential.
func resolvePruneTarget(dir, keepFileName string) (string, error) {
	if keepFileName == "" || !isSecurityTokenFileName(keepFileName) ||
		strings.ContainsAny(keepFileName, "/ \t\n*?[]") {
		return "", fmt.Errorf("invalid keep file name %q", keepFileName)
	}
	normalized := strings.TrimRight(dir, "/")
	// Require at least two path segments so the target can never degenerate into "/", a
	// top-level directory, or an empty path.
	if !strings.HasPrefix(normalized, "/") || strings.Count(normalized, "/") < 2 {
		return "", fmt.Errorf("refusing to prune unexpected token directory %q", dir)
	}
	return normalized, nil
}
