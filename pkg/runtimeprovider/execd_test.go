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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestExecdProvider(t *testing.T, srv *httptest.Server) *execdProvider {
	t.Helper()
	p, err := newExecdProvider(strings.TrimPrefix(srv.URL, "http://"))
	require.NoError(t, err)
	ep := p.(*execdProvider)
	require.NoError(t, ep.Init(t.Context(), InitOptions{AccessToken: "test-token"}))
	return ep
}

func TestExecdProvider_Exec(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/command", r.URL.Path)
		assert.Equal(t, "test-token", r.Header.Get(execdAccessTokenHdr))
		var req execdCommandRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.Equal(t, "echo", req.Cmd)
		assert.Equal(t, []string{"hi"}, req.Args)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: stdout\ndata: {\"chunk\":\"hi\"}\n\n")
		fmt.Fprint(w, "event: stderr\ndata: {\"chunk\":\"warn\"}\n\n")
		fmt.Fprint(w, "event: status\ndata: {\"exitCode\":0,\"exited\":true}\n\n")
	}))
	defer srv.Close()

	p := newTestExecdProvider(t, srv)
	result, err := p.Exec(t.Context(), ExecOptions{Cmd: "echo", Args: []string{"hi"}})
	require.NoError(t, err)
	assert.Equal(t, "hi", result.Stdout)
	assert.Equal(t, "warn", result.Stderr)
	assert.True(t, result.Exited)
	assert.Equal(t, int32(0), result.ExitCode)
}

func TestExecdProvider_Exec_NonZeroExit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: status\ndata: {\"exitCode\":7,\"exited\":true}\n\n")
	}))
	defer srv.Close()

	p := newTestExecdProvider(t, srv)
	result, err := p.Exec(t.Context(), ExecOptions{Cmd: "false"})
	require.NoError(t, err)
	assert.Equal(t, int32(7), result.ExitCode)
	assert.True(t, result.Exited)
}

func TestExecdProvider_Exec_DaemonError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "boom")
	}))
	defer srv.Close()

	p := newTestExecdProvider(t, srv)
	_, err := p.Exec(t.Context(), ExecOptions{Cmd: "echo"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

func TestExecdProvider_WriteFile(t *testing.T) {
	var gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/files/upload", r.URL.Path)
		assert.Equal(t, "test-token", r.Header.Get(execdAccessTokenHdr))
		require.NoError(t, r.ParseMultipartForm(1<<20))
		gotPath = r.FormValue("path")
		file, _, err := r.FormFile("file")
		require.NoError(t, err)
		defer func() { _ = file.Close() }()
		gotBody, err = io.ReadAll(file)
		require.NoError(t, err)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	p := newTestExecdProvider(t, srv)
	err := p.WriteFile(t.Context(), WriteFileOptions{
		Path:    "/tmp/a.txt",
		Content: strings.NewReader("payload"),
	})
	require.NoError(t, err)
	assert.Equal(t, "/tmp/a.txt", gotPath)
	assert.Equal(t, "payload", string(gotBody))
}

func TestExecdProvider_ReadFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/files/download", r.URL.Path)
		assert.Equal(t, "/tmp/a.txt", r.URL.Query().Get("path"))
		w.Header().Set("Content-Length", "7")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("payload"))
	}))
	defer srv.Close()

	p := newTestExecdProvider(t, srv)
	result, err := p.ReadFile(t.Context(), ReadFileOptions{Path: "/tmp/a.txt"})
	require.NoError(t, err)
	defer func() { _ = result.Content.Close() }()
	assert.EqualValues(t, 7, result.Size)
	content, err := io.ReadAll(result.Content)
	require.NoError(t, err)
	assert.Equal(t, "payload", string(content))
}

func TestExecdProvider_ReadFile_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "no such file")
	}))
	defer srv.Close()

	p := newTestExecdProvider(t, srv)
	_, err := p.ReadFile(t.Context(), ReadFileOptions{Path: "/missing"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "404")
}

func TestExecdProvider_Init_RequiresAccessToken(t *testing.T) {
	p, err := newExecdProvider("10.0.0.5")
	require.NoError(t, err)
	err = p.Init(t.Context(), InitOptions{})
	require.Error(t, err)
}

func TestNewExecdProvider_DefaultPort(t *testing.T) {
	p, err := newExecdProvider("10.0.0.5")
	require.NoError(t, err)
	ep := p.(*execdProvider)
	assert.Equal(t, "http://10.0.0.5:44772", ep.baseURL)
}
