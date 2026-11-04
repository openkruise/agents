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

package opensandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/proto/envd/process"
)

type fileMetadata struct {
	Path  string `json:"path"`
	Owner string `json:"owner"`
	Group string `json:"group"`
	Mode  *int   `json:"mode"`
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	reader, err := r.MultipartReader()
	if err != nil {
		writeError(w, 400, "INVALID_REQUEST_BODY", "multipart upload is required")
		return
	}
	token := r.Header.Get(executionHeader)
	count := 0
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeError(w, 400, "INVALID_REQUEST_BODY", "invalid multipart body")
			return
		}
		var meta fileMetadata
		if part.FormName() != "metadata" {
			writeError(w, 400, "INVALID_FILE_METADATA", "metadata must precede each file")
			return
		}
		data, err := io.ReadAll(io.LimitReader(part, 64*1024+1))
		if err != nil || len(data) > 64*1024 || json.Unmarshal(data, &meta) != nil || meta.Path == "" {
			writeError(w, 400, "INVALID_FILE_METADATA", "invalid file metadata")
			return
		}
		mode := 755
		if meta.Mode != nil {
			mode = *meta.Mode
		}
		if _, err := strconv.ParseUint(strconv.Itoa(mode), 8, 12); err != nil {
			writeError(w, 400, "INVALID_FILE_METADATA", "invalid octal file mode")
			return
		}
		part, err = reader.NextPart()
		if err != nil || part.FormName() != "file" {
			writeError(w, 400, "INVALID_FILE_CONTENT", "file must follow metadata")
			return
		}
		user := meta.Owner
		if user == "" {
			user = executionUser
		}
		if err := s.uploadFile(r.Context(), token, user, meta.Path, part); err != nil {
			runtimeError(w, err)
			return
		}
		// envd creates the file with its native owner and umask. Apply only the
		// explicitly requested metadata using literal arguments in the workload.
		if meta.Group != "" {
			if err := s.fileCommand(r.Context(), token, "chown", []string{"--", user + ":" + meta.Group, meta.Path}); err != nil {
				runtimeError(w, err)
				return
			}
		}
		if err := s.fileCommand(r.Context(), token, "chmod", []string{"--", strconv.Itoa(mode), meta.Path}); err != nil {
			runtimeError(w, err)
			return
		}
		count++
	}
	if count == 0 {
		writeError(w, 400, "INVALID_REQUEST_BODY", "upload contains no files")
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) uploadFile(ctx context.Context, token, user, path string, content io.Reader) error {
	// Only the small multipart envelope is buffered. File content flows from
	// the incoming request to envd with backpressure and request cancellation.
	var prefix bytes.Buffer
	writer := multipart.NewWriter(&prefix)
	if _, err := writer.CreateFormFile("file", "upload"); err != nil {
		return err
	}
	body := io.MultiReader(bytes.NewReader(prefix.Bytes()), content, strings.NewReader("\r\n--"+writer.Boundary()+"--\r\n"))
	req, err := s.backend.FilesRequest(ctx, http.MethodPost, token, user, path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := s.backend.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			klog.FromContext(ctx).Error(err, "close native upload response")
		}
	}()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return nativeHTTPError(resp.StatusCode)
	}
	_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return err
}

func (s *Server) fileCommand(ctx context.Context, token, command string, args []string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	id := uuid.NewString()
	stream, err := s.backend.Start(ctx, token, "root", id, &process.ProcessConfig{Cmd: command, Args: args})
	if err != nil {
		return err
	}
	finished := false
	defer func() {
		if err := stream.Close(); err != nil {
			klog.FromContext(ctx).Error(err, "close file metadata operation")
		}
		if !finished {
			cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer stop()
			if err := s.backend.Kill(cleanup, token, "root", id); err != nil {
				klog.FromContext(ctx).Error(err, "stop interrupted file metadata operation")
			}
		}
	}()
	for stream.Receive() {
		if end := stream.Msg().GetEvent().GetEnd(); end != nil {
			finished = true
			if end.ExitCode != 0 || end.Error != nil || !end.Exited {
				return fmt.Errorf("file metadata operation failed (exit %d)", end.ExitCode)
			}
			return nil
		}
	}
	if err := stream.Err(); err != nil {
		return err
	}
	return errors.New("file metadata operation ended without completion")
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if query.Get("path") == "" {
		writeError(w, 400, "MISSING_QUERY", "path is required")
		return
	}
	if query.Has("offset") || query.Has("limit") {
		writeError(w, 501, "NOT_SUPPORTED", "line-based download is not supported; byte Range is available")
		return
	}
	req, err := s.backend.FilesRequest(r.Context(), http.MethodGet, r.Header.Get(executionHeader), executionUser, query.Get("path"), nil)
	if err != nil {
		runtimeError(w, err)
		return
	}
	for _, name := range []string{"Range", "If-Range", "If-Modified-Since", "If-None-Match"} {
		if value := r.Header.Get(name); value != "" {
			req.Header.Set(name, value)
		}
	}
	resp, err := s.backend.HTTP.Do(req)
	if err != nil {
		runtimeError(w, err)
		return
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			klog.FromContext(r.Context()).Error(err, "close native download response")
		}
	}()
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		runtimeError(w, nativeHTTPError(resp.StatusCode))
		return
	}
	for _, name := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified", "Content-Disposition"} {
		if value := resp.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		klog.FromContext(r.Context()).V(4).Info("download interrupted", "error", err)
	}
}
