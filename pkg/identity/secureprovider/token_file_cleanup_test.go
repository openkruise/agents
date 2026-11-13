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
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils/runtime"
	"github.com/openkruise/agents/proto/envd/filesystem"
	"github.com/openkruise/agents/proto/envd/filesystem/filesystemconnect"
)

// tokenDir is the normalized form of DefaultSecurityTokenFilePath, i.e. what the prune
// hands to the runtime helpers.
const tokenDir = "/var/opt/sandbox/agent-token"

// fakeFilesystem serves the envd Filesystem service and records the paths the prune
// removes, so a test can assert both the listing input and the removal calls without a
// real sandbox.
type fakeFilesystem struct {
	filesystemconnect.UnimplementedFilesystemHandler

	// entries is returned by ListDir when listErr is nil.
	entries []*filesystem.EntryInfo
	// listErr, when set, is returned by ListDir instead of entries.
	listErr error
	// removeErr, when set, is returned by every Remove call.
	removeErr error

	mu sync.Mutex
	// removed records the path of every Remove call, in order.
	removed []string
}

func (f *fakeFilesystem) ListDir(_ context.Context, _ *connect.Request[filesystem.ListDirRequest],
) (*connect.Response[filesystem.ListDirResponse], error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return connect.NewResponse(&filesystem.ListDirResponse{Entries: f.entries}), nil
}

func (f *fakeFilesystem) Remove(_ context.Context, req *connect.Request[filesystem.RemoveRequest],
) (*connect.Response[filesystem.RemoveResponse], error) {
	f.mu.Lock()
	f.removed = append(f.removed, req.Msg.GetPath())
	f.mu.Unlock()
	if f.removeErr != nil {
		return nil, f.removeErr
	}
	return connect.NewResponse(&filesystem.RemoveResponse{}), nil
}

func (f *fakeFilesystem) recordedRemovals() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.removed...)
}

// newFakeFilesystemSandbox starts fake and returns a Sandbox annotated with its URL.
func newFakeFilesystemSandbox(t *testing.T, fake *fakeFilesystem) *agentsv1alpha1.Sandbox {
	t.Helper()
	_, h := filesystemconnect.NewFilesystemHandler(fake)
	mux := http.NewServeMux()
	mux.Handle("/", h)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   "ns-a",
			Name:        "sbx-a",
			Annotations: map[string]string{agentsv1alpha1.AnnotationRuntimeURL: server.URL},
		},
	}
}

// tokenFile builds a regular-file entry directly under tokenDir.
func tokenFile(name string) *filesystem.EntryInfo {
	return &filesystem.EntryInfo{Name: name, Path: tokenDir + "/" + name, Type: filesystem.FileType_FILE_TYPE_FILE}
}

// tokenDirEntry builds a directory entry directly under tokenDir.
func tokenDirEntry(name string) *filesystem.EntryInfo {
	return &filesystem.EntryInfo{Name: name, Path: tokenDir + "/" + name, Type: filesystem.FileType_FILE_TYPE_DIRECTORY}
}

// tokenUnclassifiedEntry builds an entry the service could not classify as a regular
// file or a directory. This is what envd reports for a socket, a device, a FIFO or a
// dangling symlink, and it is the shape a future runtime would use for a type this
// build does not know about.
func tokenUnclassifiedEntry(name string) *filesystem.EntryInfo {
	return &filesystem.EntryInfo{Name: name, Path: tokenDir + "/" + name, Type: filesystem.FileType_FILE_TYPE_UNSPECIFIED}
}

func TestPruneStaleSecurityTokenFiles(t *testing.T) {
	const keepFileName = "0123456789abcdef0123456789abcdef.token"

	tests := []struct {
		name           string
		keepFileName   string
		fake           *fakeFilesystem
		expectRemovals []string
		expectError    string
		expectSentinel error
	}{
		{
			name:         "directory holding only the current token issues no removal",
			keepFileName: keepFileName,
			fake:         &fakeFilesystem{entries: []*filesystem.EntryInfo{tokenFile(keepFileName)}},
		},
		{
			name:         "empty directory issues no removal",
			keepFileName: keepFileName,
			fake:         &fakeFilesystem{},
		},
		{
			name:         "stale token files are removed by exact path",
			keepFileName: keepFileName,
			fake: &fakeFilesystem{entries: []*filesystem.EntryInfo{
				tokenFile(keepFileName),
				tokenFile("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.token"),
				tokenFile("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.token"),
			}},
			expectRemovals: []string{
				tokenDir + "/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.token",
				tokenDir + "/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.token",
			},
		},
		{
			// Legacy naming schemes must be cleaned too: the runtime resolves an identity
			// from whatever token file it finds, not only from the current name shape.
			name:         "token files from other naming schemes are removed as well",
			keepFileName: keepFileName,
			fake: &fakeFilesystem{entries: []*filesystem.EntryInfo{
				tokenFile("sbx-legacy-uid-1234.token"),
			}},
			expectRemovals: []string{tokenDir + "/sbx-legacy-uid-1234.token"},
		},
		{
			// The directory is not formally owned by this mechanism, so non-token entries
			// stay untouched — and a token-named entry that is not a regular file is
			// skipped because the Remove RPC is recursive: a directory, and anything the
			// service could not classify (socket, device, FIFO, dangling symlink).
			name:         "non-token entries and token-named non-regular entries are left untouched",
			keepFileName: keepFileName,
			fake: &fakeFilesystem{entries: []*filesystem.EntryInfo{
				tokenFile("README"),
				tokenFile("notes.txt"),
				tokenFile(".token"),
				tokenDirEntry("stale-dir.token"),
				tokenUnclassifiedEntry("stale-socket.token"),
			}},
		},
		{
			name:           "listing failure is fatal",
			keepFileName:   keepFileName,
			fake:           &fakeFilesystem{listErr: connect.NewError(connect.CodeInternal, fmt.Errorf("boom"))},
			expectError:    "failed to list security token directory",
			expectSentinel: nil,
		},
		{
			name:         "absent token directory is benign",
			keepFileName: keepFileName,
			fake:         &fakeFilesystem{listErr: connect.NewError(connect.CodeNotFound, fmt.Errorf("not found"))},
		},
		{
			// An agent-runtime predating the Filesystem service answers Unimplemented;
			// the propagator degrades on this sentinel rather than failing propagation.
			name:           "unsupported filesystem service is classified as tolerable",
			keepFileName:   keepFileName,
			fake:           &fakeFilesystem{listErr: connect.NewError(connect.CodeUnimplemented, fmt.Errorf("unknown service"))},
			expectError:    "filesystem service unsupported",
			expectSentinel: runtime.ErrRuntimeFilesystemUnsupported,
		},
		{
			name:         "removal failure is fatal",
			keepFileName: keepFileName,
			fake: &fakeFilesystem{
				entries:   []*filesystem.EntryInfo{tokenFile("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.token")},
				removeErr: connect.NewError(connect.CodeInternal, fmt.Errorf("permission denied")),
			},
			expectRemovals: []string{tokenDir + "/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.token"},
			expectError:    "failed to remove stale security token file",
		},
		{
			name:         "invalid keep file name is rejected before any call",
			keepFileName: "",
			fake:         &fakeFilesystem{},
			expectError:  "refusing to prune security token files",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := newFakeFilesystemSandbox(t, tt.fake)

			err := pruneStaleSecurityTokenFiles(context.Background(), sbx, tt.keepFileName)
			if tt.expectError == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			}
			if tt.expectSentinel != nil {
				assert.ErrorIs(t, err, tt.expectSentinel,
					"the propagator degrades on this sentinel, so the wrap chain must preserve it")
			} else {
				assert.NotErrorIs(t, err, runtime.ErrRuntimeFilesystemUnsupported,
					"only an unsupported filesystem service may be classified as tolerable")
			}
			assert.Equal(t, tt.expectRemovals, tt.fake.recordedRemovals())
		})
	}
}

func TestSelectStaleSecurityTokenFiles(t *testing.T) {
	tests := []struct {
		name         string
		entries      []*filesystem.EntryInfo
		keepFileName string
		expected     []string
	}{
		{
			name:         "no entries",
			entries:      nil,
			keepFileName: "keep.token",
			expected:     nil,
		},
		{
			name:         "the kept file is never selected",
			entries:      []*filesystem.EntryInfo{tokenFile("keep.token")},
			keepFileName: "keep.token",
			expected:     nil,
		},
		{
			name: "only regular token files are selected",
			entries: []*filesystem.EntryInfo{
				tokenFile("keep.token"),
				tokenFile("stale.token"),
				tokenFile("keep.token.bak"),
				tokenFile("README"),
				tokenFile(".token"),
				tokenDirEntry("stale-dir.token"),
				tokenUnclassifiedEntry("stale-socket.token"),
			},
			keepFileName: "keep.token",
			expected:     []string{tokenDir + "/stale.token"},
		},
		{
			// The Remove RPC is recursive, so a token-named directory must never be
			// selected even though its name matches the suffix.
			name:         "a token-named directory is excluded",
			entries:      []*filesystem.EntryInfo{tokenDirEntry("stale.token")},
			keepFileName: "keep.token",
			expected:     nil,
		},
		{
			// The type check is a whitelist: an entry the service could not classify as a
			// regular file (socket, device, FIFO, dangling symlink, or a type this build
			// does not know) must never reach the recursive Remove RPC, even though its
			// name matches the token suffix.
			name:         "an unclassified token-named entry is excluded",
			entries:      []*filesystem.EntryInfo{tokenUnclassifiedEntry("stale.token")},
			keepFileName: "keep.token",
			expected:     nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, selectStaleSecurityTokenFiles(tt.entries, tt.keepFileName))
		})
	}
}

func TestResolvePruneTarget(t *testing.T) {
	tests := []struct {
		name         string
		dir          string
		keepFileName string
		expectedDir  string
		expectError  string
	}{
		{
			name:         "production constant is accepted and normalized",
			dir:          DefaultSecurityTokenFilePath,
			keepFileName: "abcd.token",
			expectedDir:  tokenDir,
		},
		{
			name:         "repeated trailing slashes are all trimmed",
			dir:          tokenDir + "///",
			keepFileName: "abcd.token",
			expectedDir:  tokenDir,
		},
		{
			// An empty name would stop excluding the file just written, turning the
			// prune into a deletion of the live credential.
			name:         "empty keep file name is rejected",
			dir:          DefaultSecurityTokenFilePath,
			keepFileName: "",
			expectError:  "invalid keep file name",
		},
		{
			name:         "keep file name that is not a token file is rejected",
			dir:          DefaultSecurityTokenFilePath,
			keepFileName: "abcd",
			expectError:  "invalid keep file name",
		},
		{
			name:         "keep file name with a path separator is rejected",
			dir:          DefaultSecurityTokenFilePath,
			keepFileName: "../abcd.token",
			expectError:  "invalid keep file name",
		},
		{
			name:         "keep file name with a glob metacharacter is rejected",
			dir:          DefaultSecurityTokenFilePath,
			keepFileName: "*.token",
			expectError:  "invalid keep file name",
		},
		{
			name:         "root directory is rejected",
			dir:          "/",
			keepFileName: "abcd.token",
			expectError:  "refusing to prune unexpected token directory",
		},
		{
			name:         "top level directory is rejected",
			dir:          "/var",
			keepFileName: "abcd.token",
			expectError:  "refusing to prune unexpected token directory",
		},
		{
			name:         "relative directory is rejected",
			dir:          "var/opt/sandbox/agent-token",
			keepFileName: "abcd.token",
			expectError:  "refusing to prune unexpected token directory",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, err := resolvePruneTarget(tt.dir, tt.keepFileName)
			if tt.expectError == "" {
				require.NoError(t, err)
				assert.Equal(t, tt.expectedDir, dir)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectError)
			assert.Empty(t, dir)
		})
	}
}
