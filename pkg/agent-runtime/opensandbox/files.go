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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"time"

	"k8s.io/klog/v2"

	"github.com/openkruise/agents/proto/envd/process"
)

type fileMetadata struct {
	Path  string `json:"path"`
	Owner string `json:"owner"`
	Group string `json:"group"`
	Mode  *int   `json:"mode"`
}

type uploadInputError struct {
	code    string
	message string
}

func (e *uploadInputError) Error() string { return e.message }

type uploadProduction struct {
	metadata []fileMetadata
	err      error
}

type uploadedFile struct {
	Path string `json:"path"`
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	reader, err := r.MultipartReader()
	if err != nil {
		writeError(w, 400, "INVALID_REQUEST_BODY", "multipart upload is required")
		return
	}
	token := executionToken(r)
	metadata, paths, err := s.uploadBatch(w, r, reader, token)
	if err != nil {
		var inputErr *uploadInputError
		if errors.As(err, &inputErr) {
			writeError(w, http.StatusBadRequest, inputErr.code, inputErr.message)
		} else {
			runtimeError(w, err)
		}
		return
	}
	// All content must succeed before the first explicit attribute is applied.
	// Only the native response determines each file's actual path.
	for i, meta := range metadata {
		owner := meta.Owner
		if meta.Group != "" {
			owner += ":" + meta.Group
		}
		if owner != "" {
			if err := s.fileCommand(r.Context(), token, "chown", []string{"--", owner, paths[i].Path}); err != nil {
				runtimeError(w, err)
				return
			}
		}
		if meta.Mode != nil {
			if err := s.fileCommand(r.Context(), token, "chmod", []string{"--", strconv.Itoa(*meta.Mode), paths[i].Path}); err != nil {
				runtimeError(w, err)
				return
			}
		}
	}
	w.WriteHeader(http.StatusOK)
}

// produceUpload keeps only per-entry metadata. Neither file contents nor the
// incoming filename are used to determine the native target path.
func produceUpload(reader *multipart.Reader, writer *multipart.Writer, inputComplete chan<- struct{}) ([]fileMetadata, error) {
	var metadata []fileMetadata
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			if len(metadata) == 0 {
				return nil, &uploadInputError{"INVALID_REQUEST_BODY", "upload contains no files"}
			}
			close(inputComplete)
			break
		}
		if err != nil {
			return nil, &uploadInputError{"INVALID_REQUEST_BODY", "invalid multipart body"}
		}
		var meta fileMetadata
		if part.FormName() != "metadata" {
			return nil, &uploadInputError{"INVALID_FILE_METADATA", "metadata must precede each file"}
		}
		data, err := io.ReadAll(io.LimitReader(part, 64*1024+1))
		if err != nil || len(data) > 64*1024 || json.Unmarshal(data, &meta) != nil || meta.Path == "" {
			return nil, &uploadInputError{"INVALID_FILE_METADATA", "invalid file metadata"}
		}
		if meta.Mode != nil {
			if _, err := strconv.ParseUint(strconv.Itoa(*meta.Mode), 8, 12); err != nil {
				return nil, &uploadInputError{"INVALID_FILE_METADATA", "invalid octal file mode"}
			}
		}
		part, err = reader.NextPart()
		if err != nil || part.FormName() != "file" {
			return nil, &uploadInputError{"INVALID_FILE_CONTENT", "file must follow metadata"}
		}
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": meta.Path}))
		header.Set("Content-Type", "application/octet-stream")
		nativePart, err := writer.CreatePart(header)
		if err != nil {
			return nil, fmt.Errorf("create native upload part: %w", err)
		}
		if _, err := io.Copy(nativePart, part); err != nil {
			return nil, fmt.Errorf("forward upload content: %w", err)
		}
		metadata = append(metadata, meta)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("finish native upload multipart: %w", err)
	}
	return metadata, nil
}

func (s *Server) uploadBatch(w http.ResponseWriter, r *http.Request, reader *multipart.Reader, token string) ([]fileMetadata, []uploadedFile, error) {
	ctx, cancel := context.WithCancel(r.Context())
	pipeReader, pipeWriter := io.Pipe()
	writer := multipart.NewWriter(pipeWriter)
	inputComplete := make(chan struct{})
	produced := make(chan uploadProduction, 1)
	producerDone := make(chan struct{})
	cancelDone := make(chan struct{})
	// Closing the incoming body also interrupts a producer blocked waiting for
	// client bytes. The HTTP entry additionally cancels network read deadlines.
	stop := context.AfterFunc(ctx, func() {
		defer close(cancelDone)
		closeUploadPipe(ctx, pipeReader, pipeWriter, ctx.Err())
		// Native rejection cancels this transfer without cancelling the parent
		// HTTP request. Interrupt its network read before Close can drain or wait
		// for an incoming body reader, preserving the ability to write the error.
		if err := http.NewResponseController(w).SetReadDeadline(time.Now()); err != nil && !errors.Is(err, http.ErrNotSupported) {
			klog.FromContext(ctx).Error(err, "interrupt cancelled upload input")
		}
		closeFileBody(ctx, r.Body, "close cancelled upload input")
	})
	defer func() {
		if !stop() {
			<-cancelDone
		}
		cancel()
		closeUploadPipe(ctx, pipeReader, pipeWriter, nil)
	}()
	go func() {
		metadata, err := produceUpload(reader, writer, inputComplete)
		// Publish the parse result before closing the native body, so an input
		// error cannot be confused with the transport error it causes.
		produced <- uploadProduction{metadata: metadata, err: err}
		if closeErr := pipeWriter.CloseWithError(err); closeErr != nil {
			klog.FromContext(ctx).Error(closeErr, "close native upload production")
		}
		close(producerDone)
	}()
	// Every failure cancels both transfer directions and joins the sole producer.
	// A parse failure remains a client error even if it also aborts native HTTP.
	fail := func(err error) ([]fileMetadata, []uploadedFile, error) {
		select {
		case result := <-produced:
			cancel()
			<-producerDone
			var inputErr *uploadInputError
			if errors.As(result.err, &inputErr) && r.Context().Err() == nil {
				return nil, nil, inputErr
			}
		default:
			cancel()
			<-produced
			<-producerDone
		}
		return nil, nil, err
	}
	req, err := s.backend.FilesRequest(ctx, http.MethodPost, token, executionUser, "", pipeReader)
	if err != nil {
		return fail(err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := s.backend.HTTP.Do(req)
	if err != nil {
		return fail(err)
	}
	responseCancelDone := make(chan struct{})
	stopResponse := context.AfterFunc(ctx, func() {
		defer close(responseCancelDone)
		closeFileBody(ctx, resp.Body, "close cancelled native upload response")
	})
	defer func() {
		if !stopResponse() {
			<-responseCancelDone
		}
		closeFileBody(ctx, resp.Body, "close native upload response")
	}()
	if resp.StatusCode != http.StatusOK {
		return fail(nativeHTTPError(resp.StatusCode))
	}
	select {
	case <-inputComplete:
	default:
		return fail(errors.New("native upload responded before the input was complete"))
	}
	// Native HTTP has replied, so an unfinished final-boundary Write must not
	// block the producer join. Closing the writer releases that Write with an
	// error, while an already acknowledged Write still succeeds. Leave the reader
	// open so the transport sees EOF instead of an error that would tear down the
	// connection while its success response is still being consumed.
	if err := pipeWriter.CloseWithError(nil); err != nil {
		return fail(fmt.Errorf("close completed native upload request: %w", err))
	}
	result := <-produced
	<-producerDone
	if result.err != nil {
		return nil, nil, result.err
	}
	var paths []uploadedFile
	decoder := json.NewDecoder(resp.Body)
	if err := decoder.Decode(&paths); err != nil {
		return nil, nil, fmt.Errorf("decode native upload paths: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, nil, errors.New("native upload response contains trailing data")
	}
	if len(paths) != len(result.metadata) {
		return nil, nil, errors.New("native upload response entry count does not match the request")
	}
	for _, entry := range paths {
		if entry.Path == "" {
			return nil, nil, errors.New("native upload response contains an empty path")
		}
	}
	return result.metadata, paths, nil
}

func closeUploadPipe(ctx context.Context, reader *io.PipeReader, writer *io.PipeWriter, cause error) {
	if err := errors.Join(reader.CloseWithError(cause), writer.CloseWithError(cause)); err != nil {
		klog.FromContext(ctx).Error(err, "close upload transfer pipe")
	}
}

func closeFileBody(ctx context.Context, body io.Closer, message string) {
	if err := body.Close(); err != nil {
		klog.FromContext(ctx).Error(err, message)
	}
}

func (s *Server) fileCommand(ctx context.Context, token, command string, args []string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cwd := "/"
	stream, err := s.backend.Start(ctx, token, "root", &process.ProcessConfig{Cmd: command, Args: args, Cwd: &cwd}, 10*time.Second)
	if err != nil {
		return err
	}
	defer func() {
		if err := stream.Close(); err != nil {
			klog.FromContext(ctx).Error(err, "close file metadata operation")
		}
	}()
	for stream.Receive() {
		if end := stream.Msg().GetEvent().GetEnd(); end != nil {
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
	req, err := s.backend.FilesRequest(r.Context(), http.MethodGet, executionToken(r), executionUser, query.Get("path"), nil)
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
	cancelDone := make(chan struct{})
	stop := context.AfterFunc(r.Context(), func() {
		defer close(cancelDone)
		closeFileBody(r.Context(), resp.Body, "close cancelled native download response")
	})
	defer func() {
		if !stop() {
			<-cancelDone
		}
		closeFileBody(r.Context(), resp.Body, "close native download response")
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
