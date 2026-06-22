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

// Package auditlog provides a non-blocking INFO-level audit logger for the
// traffic-extension data plane. Every successfully handled ext-proc
// RequestHeaders call produces exactly one Entry summarising which
// SecurityProfile rules fired, what the final outcome was, and which
// plugins were claimed but preempted by a terminal action.
//
// The default Logger buffers entries through an in-memory channel consumed
// by a single worker goroutine, so request-path latency only pays for a
// non-blocking channel send. When the buffer is full Submit drops the
// entry and increments a Prometheus counter rather than blocking the
// caller.
package auditlog

import (
	"context"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
)

// DefaultBufferSize is the channel capacity used when the caller does not
// configure --audit-log-buffer-size.
const DefaultBufferSize = 4096

// Entry is the per-request audit payload assembled by the request handler.
//
// Outcome takes one of: "passthrough", "mutated", "blocked", "bypassed",
// "error". Actions records every plugin that materially acted on the
// request in the form "<plugin>:<profile-namespace>/<profile-name>/<rule>".
// Skipped counts plugins that claimed a rule via ActionRecord but never
// got to commit a mutation because a later terminal action (or an error)
// preempted them.
type Entry struct {
	Pod      types.NamespacedName
	Method   string
	Host     string
	Path     string
	Profiles int
	Outcome  string
	Actions  []string
	Skipped  map[string]int
	Error    string
}

// Logger is the contract the request handler invokes once per request via
// a deferred call. Implementations MUST be safe for concurrent use and
// MUST NOT block the caller.
type Logger interface {
	Submit(entry Entry)
}

// Nop returns a Logger whose Submit is a no-op. Used as a default when the
// handler is constructed without an audit logger (e.g. unit tests) so the
// dispatch path can call Submit unconditionally.
func Nop() Logger { return nopLogger{} }

type nopLogger struct{}

func (nopLogger) Submit(Entry) {}

// BufferedLogger is the default Logger implementation. Submit performs a
// non-blocking channel send; a single worker goroutine started by Start
// consumes the channel and writes one logr Info record per entry.
//
// BufferedLogger satisfies sigs.k8s.io/controller-runtime/pkg/manager.Runnable
// so it can be wired into the manager alongside the ext-proc server.
type BufferedLogger struct {
	logger logr.Logger
	ch     chan Entry
}

// NewBufferedLogger constructs a BufferedLogger backed by the given logr.
// Logger and channel capacity. bufferSize <= 0 falls back to DefaultBufferSize.
func NewBufferedLogger(logger logr.Logger, bufferSize int) *BufferedLogger {
	if bufferSize <= 0 {
		bufferSize = DefaultBufferSize
	}
	return &BufferedLogger{
		logger: logger,
		ch:     make(chan Entry, bufferSize),
	}
}

// Submit enqueues entry for asynchronous emission. The call is
// non-blocking: when the buffer is full the entry is dropped and
// AuditLogDroppedTotal is incremented. A nil receiver is a no-op so
// uninitialised Loggers do not panic from inside the request path.
func (l *BufferedLogger) Submit(entry Entry) {
	if l == nil || l.ch == nil {
		return
	}
	select {
	case l.ch <- entry:
	default:
		AuditLogDroppedTotal.Inc()
	}
}

// Start drains the entry channel until ctx is cancelled, then flushes any
// remaining entries and returns. It implements manager.Runnable.
func (l *BufferedLogger) Start(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			l.drain()
			return nil
		case e := <-l.ch:
			l.emit(e)
		}
	}
}

// drain consumes any buffered entries without blocking when the channel
// empties. Called once during shutdown so the last few requests handled
// before ctx cancellation still surface in the log.
func (l *BufferedLogger) drain() {
	for {
		select {
		case e := <-l.ch:
			l.emit(e)
		default:
			return
		}
	}
}

// emit converts an Entry into structured key-value pairs and writes one
// logr Info record. Error is only emitted when non-empty to keep the
// happy-path line short.
func (l *BufferedLogger) emit(e Entry) {
	kvs := []any{
		"pod", e.Pod.String(),
		"method", e.Method,
		"host", e.Host,
		"path", e.Path,
		"profiles", e.Profiles,
		"outcome", e.Outcome,
		"actions", e.Actions,
		"skipped", e.Skipped,
	}
	if e.Error != "" {
		kvs = append(kvs, "error", e.Error)
	}
	l.logger.Info("egress request handled", kvs...)
}
