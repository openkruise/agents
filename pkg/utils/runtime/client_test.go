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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// fastBackoff keeps retry-based tests quick while still allowing several attempts.
var fastBackoff = wait.Backoff{
	Duration: time.Millisecond,
	Factor:   1.0,
	Steps:    5,
}

// newMountTestServer starts an httptest server that serves POST /v1/storage/mounts
// with the provided handler and returns the server plus a sandbox annotated with
// its URL. The atomic counter records how many times the handler was invoked.
func newMountTestServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *agentsv1alpha1.Sandbox) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/storage/mounts", handler)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, sandboxWithURL(server.URL)
}

func sandboxWithURL(url string) *agentsv1alpha1.Sandbox {
	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Annotations: map[string]string{
				agentsv1alpha1.AnnotationRuntimeURL: url,
			},
		},
	}
}

func writeMountResponse(t *testing.T, w http.ResponseWriter, status int, resp CreateMountResponse) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	require.NoError(t, json.NewEncoder(w).Encode(resp))
}

func TestStorageMount_RetriesOn5xxThenSucceeds(t *testing.T) {
	var calls int32
	server, sbx := newMountTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			writeMountResponse(t, w, http.StatusInternalServerError, CreateMountResponse{
				Success: false,
				Message: "transient failure",
			})
			return
		}
		writeMountResponse(t, w, http.StatusOK, CreateMountResponse{
			Success:   true,
			MountPath: "/run/csi/mount-root/oss/abc",
			LinkPath:  "/dir2/u2",
			Message:   "mount completed successfully",
		})
	})
	_ = server

	rt := NewRuntime(sbx, WithRetry(fastBackoff))
	resp, err := rt.Storage().Mount(context.Background(), CreateMountRequest{Driver: "oss", Config: "cfg"})
	require.NoError(t, err)
	assert.True(t, resp.Success)
	assert.Equal(t, "/run/csi/mount-root/oss/abc", resp.MountPath)
	assert.Equal(t, "/dir2/u2", resp.LinkPath)
	assert.Equal(t, int32(3), atomic.LoadInt32(&calls), "should have retried until the 3rd attempt succeeded")
}

func TestStorageMount_DoesNotRetryOn4xx(t *testing.T) {
	var calls int32
	server, sbx := newMountTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		writeMountResponse(t, w, http.StatusBadRequest, CreateMountResponse{
			Success: false,
			Message: "unsupported driver: bogus, no registered provider",
		})
	})
	_ = server

	rt := NewRuntime(sbx, WithRetry(fastBackoff))
	_, err := rt.Storage().Mount(context.Background(), CreateMountRequest{Driver: "bogus", Config: "cfg"})
	require.Error(t, err)

	var apiErr *APIError
	require.True(t, errors.As(err, &apiErr), "error should be an *APIError")
	assert.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
	assert.True(t, apiErr.IsClientError())
	assert.Contains(t, apiErr.Message, "unsupported driver")
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "4xx must not be retried")
}

func TestStorageMount_StopsRetryingWhenContextCancelled(t *testing.T) {
	var calls int32
	server, sbx := newMountTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		writeMountResponse(t, w, http.StatusInternalServerError, CreateMountResponse{Success: false, Message: "boom"})
	})
	_ = server

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the call so no retry loop proceeds

	rt := NewRuntime(sbx, WithRetry(fastBackoff))
	_, err := rt.Storage().Mount(ctx, CreateMountRequest{Driver: "oss", Config: "cfg"})
	require.Error(t, err)
	assert.LessOrEqual(t, atomic.LoadInt32(&calls), int32(1), "a cancelled context must not drive further retries")
}

func TestStorageMount_RefreshResolvesURLThenSucceeds(t *testing.T) {
	var calls int32
	server, readySbx := newMountTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		writeMountResponse(t, w, http.StatusOK, CreateMountResponse{Success: true, MountPath: "/m", LinkPath: "/l"})
	})
	_ = server

	// The bound sandbox starts without a runtime URL; the refresh hook only
	// surfaces the ready sandbox from the 2nd invocation onward.
	notReady := sandboxWithURL("")
	var refreshes int32
	refresh := func(ctx context.Context) (*agentsv1alpha1.Sandbox, error) {
		if atomic.AddInt32(&refreshes, 1) == 1 {
			return notReady, nil
		}
		return readySbx, nil
	}

	rt := NewRuntime(notReady, WithRetry(fastBackoff), WithRefresh(refresh))
	resp, err := rt.Storage().Mount(context.Background(), CreateMountRequest{Driver: "oss", Config: "cfg"})
	require.NoError(t, err)
	assert.Equal(t, "/m", resp.MountPath)
	assert.GreaterOrEqual(t, atomic.LoadInt32(&refreshes), int32(2), "should refresh again after the URL was not ready")
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "server should be hit once, after the URL resolved")
}

func TestStorageMount_SuccessFalseOn200IsError(t *testing.T) {
	server, sbx := newMountTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeMountResponse(t, w, http.StatusOK, CreateMountResponse{Success: false, Message: "provider rejected"})
	})
	_ = server

	rt := NewRuntime(sbx, WithRetry(fastBackoff))
	_, err := rt.Storage().Mount(context.Background(), CreateMountRequest{Driver: "oss", Config: "cfg"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "provider rejected")
}

func TestStorageMount_ErrorMessageFromErrorField(t *testing.T) {
	// The auth/permission middleware returns {"error": ...} rather than
	// {"message": ...}; extractErrorMessage must handle both shapes.
	server, sbx := newMountTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"Unauthorized access, please provide a valid access token"}`))
	})
	_ = server

	rt := NewRuntime(sbx, WithRetry(fastBackoff))
	_, err := rt.Storage().Mount(context.Background(), CreateMountRequest{Driver: "oss", Config: "cfg"})
	require.Error(t, err)

	var apiErr *APIError
	require.True(t, errors.As(err, &apiErr))
	assert.Equal(t, http.StatusUnauthorized, apiErr.StatusCode)
	assert.Contains(t, apiErr.Message, "Unauthorized access")
}

func TestStorageMount_NoRuntimeURL(t *testing.T) {
	rt := NewRuntime(sandboxWithURL(""), WithRetry(fastBackoff))
	_, err := rt.Storage().Mount(context.Background(), CreateMountRequest{Driver: "oss", Config: "cfg"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runtime url not found")
}
