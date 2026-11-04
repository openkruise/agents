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
	"net/http"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/proto/envd/process"
)

const runtimeErrorName = "RuntimeError"

type commandRequest struct {
	Command    *string           `json:"command"`
	Argv       []string          `json:"argv"`
	Cwd        *string           `json:"cwd"`
	Envs       map[string]string `json:"envs"`
	Timeout    int64             `json:"timeout"`
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

func sendEvent(w http.ResponseWriter, event streamEvent) error {
	event.Timestamp = time.Now().UnixMilli()
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}
	flushErr := controller.Flush()
	resetErr := controller.SetWriteDeadline(time.Time{})
	if errors.Is(resetErr, http.ErrNotSupported) {
		resetErr = nil
	}
	return errors.Join(flushErr, resetErr)
}

func (s *Server) runCommand(w http.ResponseWriter, r *http.Request) {
	var input commandRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil {
		writeError(w, 400, "INVALID_REQUEST_BODY", "invalid command request")
		return
	}
	if (input.Command != nil) == (input.Argv != nil) || input.Command != nil && *input.Command == "" || input.Argv != nil && (len(input.Argv) == 0 || input.Argv[0] == "") || input.Timeout < 0 {
		writeError(w, 400, "INVALID_REQUEST_BODY", "provide exactly one non-empty command or argv and a non-negative timeout")
		return
	}
	if input.Background || input.UID != nil || input.GID != nil {
		writeError(w, 501, "NOT_SUPPORTED", "background execution and numeric uid/gid overrides are not supported")
		return
	}
	cfg := &process.ProcessConfig{Cwd: input.Cwd, Envs: input.Envs}
	if input.Command != nil {
		cfg.Cmd, cfg.Args = "/bin/bash", []string{"-c", *input.Command}
	} else {
		cfg.Cmd, cfg.Args = input.Argv[0], input.Argv[1:]
	}
	if strings.ContainsRune(cfg.Cmd, 0) {
		writeError(w, 400, "INVALID_REQUEST_BODY", "command contains NUL")
		return
	}
	id, token := uuid.NewString(), r.Header.Get(executionHeader)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	if input.Timeout > 0 {
		if input.Timeout > int64((1<<63-1)/time.Millisecond) {
			writeError(w, 400, "INVALID_REQUEST_BODY", "timeout is too large")
			return
		}
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, time.Duration(input.Timeout)*time.Millisecond)
		defer stop()
	}
	s.mu.Lock()
	if token != s.token {
		s.mu.Unlock()
		writeError(w, 401, "UNAUTHORIZED", "execution credential changed")
		return
	}
	command := &activeCommand{cancel: cancel, done: make(chan struct{})}
	s.active[id] = command
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.active, id); s.mu.Unlock(); close(command.done) }()

	finished := false
	defer func() {
		if finished {
			return
		}
		// The native process outlives its request connection. Use a bounded,
		// detached context to stop it even after the caller has disconnected.
		cleanupCtx, stop := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Second)
		defer stop()
		err := s.backend.Kill(cleanupCtx, token, executionUser, id)
		if err != nil && connect.CodeOf(err) != connect.CodeNotFound {
			command.cleanupErr = err
			klog.FromContext(r.Context()).Error(err, "stop interrupted command", "executionID", id)
		}
	}()
	stream, err := s.backend.Start(ctx, token, executionUser, id, cfg)
	if err != nil {
		runtimeError(w, err)
		return
	}
	defer func() {
		if err := stream.Close(); err != nil {
			klog.FromContext(r.Context()).Error(err, "close command stream", "executionID", id)
		}
	}()
	// Wait for the first native event so startup and authentication errors can
	// still be represented as synchronous HTTP errors.
	if !stream.Receive() {
		err := stream.Err()
		if err == nil {
			err = errors.New("runtime stream ended before process start")
		}
		runtimeError(w, err)
		return
	}
	finished = forwardCommandStream(w, r, ctx, stream, id)
}

// forwardCommandStream reports whether the native process reached its end event.
func forwardCommandStream(w http.ResponseWriter, r *http.Request, ctx context.Context, stream *connect.ServerStreamForClient[process.StartResponse], id string) bool {
	start := time.Now()
	if err := sendEvent(w, streamEvent{Type: "init", Text: id}); err != nil {
		return false
	}
	for {
		event := stream.Msg().GetEvent()
		var output streamEvent
		switch e := event.GetEvent().(type) {
		case *process.ProcessEvent_Start:
			output = streamEvent{Type: "status", Text: "running"}
		case *process.ProcessEvent_Data:
			switch d := e.Data.Output.(type) {
			case *process.ProcessEvent_DataEvent_Stdout:
				output = streamEvent{Type: "stdout", Text: string(d.Stdout)}
			case *process.ProcessEvent_DataEvent_Stderr:
				output = streamEvent{Type: "stderr", Text: string(d.Stderr)}
			}
		case *process.ProcessEvent_End:
			if e.End.ExitCode != 0 || e.End.Error != nil || !e.End.Exited {
				failure := &streamError{Name: "CommandExecutionError", Value: strconv.Itoa(int(e.End.ExitCode)), Traceback: []string{}}
				// envd also sets Error for ordinary non-zero exits. Preserve
				// their numeric status so the SDK can infer exit_code.
				if e.End.Exited && e.End.ExitCode != 0 {
					failure.Name, failure.Value = "CommandExecutionError", strconv.Itoa(int(e.End.ExitCode))
				} else if e.End.Error != nil {
					failure.Name, failure.Value = runtimeErrorName, *e.End.Error
				} else if !e.End.Exited {
					failure.Name, failure.Value = runtimeErrorName, "process ended without an exit status"
				}
				if err := sendEvent(w, streamEvent{Type: "error", Error: failure}); err != nil {
					return true
				}
			}
			if err := sendEvent(w, streamEvent{Type: "execution_complete", ExecutionTime: time.Since(start).Milliseconds()}); err != nil {
				klog.FromContext(r.Context()).V(4).Info("command client disconnected", "executionID", id)
			}
			return true
		case *process.ProcessEvent_Keepalive:
			output = streamEvent{Type: "ping"}
		}
		if output.Type != "" {
			if err := sendEvent(w, output); err != nil {
				return false
			}
		}
		if !stream.Receive() {
			break
		}
	}
	err := stream.Err()
	if err == nil {
		err = errors.New("runtime stream ended without process completion")
	}
	if r.Context().Err() == nil {
		name := runtimeErrorName
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			name = "CommandTimeout"
		} else if ctx.Err() != nil {
			name = "CommandCancelled"
		}
		if writeErr := sendEvent(w, streamEvent{Type: "error", Error: &streamError{Name: name, Value: err.Error(), Traceback: []string{}}}); writeErr != nil {
			klog.FromContext(r.Context()).V(4).Info("command error stream disconnected", "executionID", id)
		}
	}
	return false
}

func (s *Server) interruptCommand(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	s.mu.RLock()
	command := s.active[id]
	s.mu.RUnlock()
	if command == nil {
		writeError(w, 404, "NOT_FOUND", "active command not found")
		return
	}
	command.cancel()
	select {
	case <-command.done:
		if command.cleanupErr != nil {
			runtimeError(w, command.cleanupErr)
			return
		}
	case <-r.Context().Done():
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
