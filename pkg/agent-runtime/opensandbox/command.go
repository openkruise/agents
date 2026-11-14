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
	"net/http"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/proto/envd/process"
)

const runtimeErrorName = "RuntimeError"

type commandRequest struct {
	Command    *string           `json:"command"`
	Argv       []string          `json:"argv"`
	Cwd        *string           `json:"cwd"`
	Envs       map[string]string `json:"envs"`
	Timeout    json.RawMessage   `json:"timeout"`
	Background bool              `json:"background"`
	UID        *uint32           `json:"uid"`
	GID        *uint32           `json:"gid"`
}

type streamError struct {
	Name      string   `json:"ename"`
	Value     string   `json:"evalue"`
	Traceback []string `json:"traceback"`
}

type streamEvent struct {
	Type          string       `json:"type"`
	Text          string       `json:"text,omitempty"`
	Timestamp     int64        `json:"timestamp"`
	ExecutionTime int64        `json:"execution_time,omitempty"`
	Error         *streamError `json:"error,omitempty"`
}

func sendEvent(ctx context.Context, w http.ResponseWriter, event streamEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	event.Timestamp = time.Now().UnixMilli()
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := controller.Flush(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return controller.SetWriteDeadline(time.Time{})
}

// checkCommandWriter checks streaming capabilities without committing headers.
func checkCommandWriter(w http.ResponseWriter) error {
	for current := w; ; {
		if _, ok := current.(interface{ FlushError() error }); ok {
			break
		}
		if _, ok := current.(http.Flusher); ok {
			break
		}
		unwrapper, ok := current.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return http.ErrNotSupported
		}
		current = unwrapper.Unwrap()
	}
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	return controller.SetWriteDeadline(time.Time{})
}

func (s *Server) runCommand(w http.ResponseWriter, r *http.Request) {
	var input commandRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil {
		writeError(w, 400, "INVALID_REQUEST_BODY", "invalid command request")
		return
	}
	if (input.Command != nil) == (input.Argv != nil) || input.Command != nil && *input.Command == "" || input.Argv != nil && (len(input.Argv) == 0 || input.Argv[0] == "") {
		writeError(w, 400, "INVALID_REQUEST_BODY", "provide exactly one non-empty command or argv and a non-negative timeout")
		return
	}
	timeout, err := commandTimeout(input.Timeout)
	if err != nil {
		writeError(w, 400, "INVALID_REQUEST_BODY", "timeout must be a non-negative integer number of milliseconds within the supported range")
		return
	}
	if input.Background || input.UID != nil || input.GID != nil {
		writeError(w, 501, "NOT_SUPPORTED", "background execution and numeric uid/gid overrides are not supported")
		return
	}
	cfg := &process.ProcessConfig{Cwd: input.Cwd, Envs: input.Envs}
	if input.Command != nil {
		cfg.Cmd, cfg.Args = "/bin/bash", []string{"-l", "-c", *input.Command}
	} else {
		cfg.Cmd, cfg.Args = input.Argv[0], input.Argv[1:]
	}
	if strings.ContainsRune(cfg.Cmd, 0) {
		writeError(w, 400, "INVALID_REQUEST_BODY", "command contains NUL")
		return
	}
	if err := r.Context().Err(); err != nil {
		return
	}
	if err := checkCommandWriter(w); err != nil {
		writeError(w, 500, "STREAMING_UNAVAILABLE", "response writer does not support command streaming")
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	if timeout > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, timeout)
		defer stop()
	}
	stream, err := s.backend.Start(ctx, executionToken(r), executionUser, cfg, timeout)
	if err != nil {
		if r.Context().Err() == nil {
			runtimeError(w, err)
		}
		return
	}
	defer func() {
		// Closing the forwarding connection does not terminate the native process.
		cancel()
		if err := stream.Close(); err != nil {
			klog.FromContext(r.Context()).Error(err, "close command stream")
		}
	}()
	// Wait for the first native event so startup and authentication errors can
	// still be represented as synchronous HTTP errors.
	if !stream.Receive() {
		if r.Context().Err() != nil {
			return
		}
		err := stream.Err()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = connect.NewError(connect.CodeDeadlineExceeded, ctx.Err())
		}
		if err == nil {
			err = errors.New("runtime stream ended before process start")
		}
		runtimeError(w, err)
		return
	}
	if r.Context().Err() != nil {
		return
	}
	start := stream.Msg().GetEvent().GetStart()
	if start == nil || start.Pid == 0 {
		runtimeError(w, errors.New("runtime stream did not begin with a valid process start"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	if r.ProtoMajor == 1 {
		w.Header().Set("Connection", "keep-alive")
	}
	if r.Context().Err() != nil {
		return
	}
	w.WriteHeader(http.StatusOK)
	forwardCommandStream(w, r, ctx, stream, strconv.FormatUint(uint64(start.Pid), 10))
}

// commandTimeout preserves the distinction between omitted and explicit null.
func commandTimeout(raw json.RawMessage) (time.Duration, error) {
	if len(raw) == 0 {
		return time.Minute, nil
	}
	var milliseconds int64
	if !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if err := json.Unmarshal(raw, &milliseconds); err != nil {
			return 0, err
		}
	}
	if milliseconds < 0 || milliseconds > int64((1<<63-1)/time.Millisecond) {
		return 0, errors.New("execution timeout is outside the supported range")
	}
	return time.Duration(milliseconds) * time.Millisecond, nil
}

// commandOutput decodes one segment, retaining only an incomplete UTF-8 tail.
// The decoder replaces maximal ill-formed subsequences, matching the SDK.
func commandOutput(tail *[]byte, chunk []byte, final bool) (string, error) {
	input := chunk
	if len(*tail) > 0 {
		input = append(*tail, chunk...)
	}
	output := make([]byte, 3*len(input))
	written, consumed, err := unicode.UTF8.NewDecoder().Transform(output, input, final)
	if err != nil && !(errors.Is(err, transform.ErrShortSrc) && !final) {
		return "", err
	}
	*tail = bytes.Clone(input[consumed:])
	return string(output[:written]), nil
}

func forwardCommandStream(w http.ResponseWriter, r *http.Request, ctx context.Context, stream *connect.ServerStreamForClient[process.StartResponse], id string) {
	start := time.Now()
	if err := sendEvent(r.Context(), w, streamEvent{Type: "init", Text: id}); err != nil {
		return
	}
	if err := sendEvent(r.Context(), w, streamEvent{Type: "status", Text: "running"}); err != nil {
		return
	}
	var stdoutTail, stderrTail []byte
	writeOutput := func(kind string, tail *[]byte, chunk []byte, final bool) error {
		text, err := commandOutput(tail, chunk, final)
		if err != nil || text == "" {
			return err
		}
		return sendEvent(r.Context(), w, streamEvent{Type: kind, Text: text})
	}
	finish := func(failure *streamError) {
		if r.Context().Err() != nil {
			return
		}
		if err := writeOutput("stdout", &stdoutTail, nil, true); err != nil {
			return
		}
		if err := writeOutput("stderr", &stderrTail, nil, true); err != nil {
			return
		}
		if failure != nil {
			if err := sendEvent(r.Context(), w, streamEvent{Type: "error", Error: failure}); err != nil {
				return
			}
		}
		if err := sendEvent(r.Context(), w, streamEvent{Type: "execution_complete", ExecutionTime: time.Since(start).Milliseconds()}); err != nil {
			klog.FromContext(r.Context()).V(4).Info("command client disconnected", "executionID", id)
		}
	}
	var streamErr error
	for stream.Receive() {
		event := stream.Msg().GetEvent()
		var writeErr error
		switch e := event.GetEvent().(type) {
		case *process.ProcessEvent_Data:
			switch d := e.Data.GetOutput().(type) {
			case *process.ProcessEvent_DataEvent_Stdout:
				writeErr = writeOutput("stdout", &stdoutTail, d.Stdout, false)
			case *process.ProcessEvent_DataEvent_Stderr:
				writeErr = writeOutput("stderr", &stderrTail, d.Stderr, false)
			default:
				streamErr = errors.New("runtime stream returned unsupported process output")
			}
		case *process.ProcessEvent_End:
			if e.End == nil {
				streamErr = errors.New("runtime stream returned an empty process end")
				break
			}
			var failure *streamError
			if e.End.ExitCode != 0 || e.End.Error != nil || !e.End.Exited {
				failure = &streamError{Name: "CommandExecutionError", Value: strconv.Itoa(int(e.End.ExitCode)), Traceback: []string{}}
				// envd also sets Error for ordinary non-zero exits. Preserve
				// their numeric status so the SDK can infer exit_code.
				if e.End.Exited && e.End.ExitCode != 0 {
					failure.Name, failure.Value = "CommandExecutionError", strconv.Itoa(int(e.End.ExitCode))
				} else if e.End.Error != nil {
					failure.Name, failure.Value = runtimeErrorName, "native process error: "+*e.End.Error
				} else if !e.End.Exited {
					failure.Name, failure.Value = runtimeErrorName, "process ended without an exit status"
				}
			}
			finish(failure)
			return
		case *process.ProcessEvent_Keepalive:
			writeErr = sendEvent(r.Context(), w, streamEvent{Type: "ping"})
		default:
			streamErr = errors.New("runtime stream returned an unexpected process event")
		}
		if writeErr != nil {
			return
		}
		if streamErr != nil {
			break
		}
	}
	if streamErr == nil {
		streamErr = stream.Err()
	}
	if streamErr == nil {
		streamErr = errors.New("runtime stream ended without process completion")
	}
	name := runtimeErrorName
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		name = "CommandTimeout"
	}
	klog.FromContext(r.Context()).V(4).Info("command stream ended without a process end", "executionID", id, "error", streamErr)
	finish(&streamError{Name: name, Value: "native execution stream ended without process completion", Traceback: []string{}})
}

func (s *Server) interruptCommand(w http.ResponseWriter, r *http.Request) {
	pid, err := strconv.ParseUint(r.URL.Query().Get("id"), 10, 32)
	if err != nil || pid == 0 {
		writeError(w, 400, "INVALID_REQUEST_BODY", "id must be a nonzero uint32 process ID")
		return
	}
	// This unary request has its own budget, independent of the POST stream.
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	if err := s.backend.Kill(ctx, executionToken(r), executionUser, uint32(pid)); err != nil {
		if r.Context().Err() != nil {
			return
		}
		if connect.CodeOf(err) == connect.CodeNotFound {
			writeError(w, 404, "NOT_FOUND", "command not found")
			return
		}
		runtimeError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}
