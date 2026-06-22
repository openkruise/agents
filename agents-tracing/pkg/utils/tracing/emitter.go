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

package tracing

import "context"

// TraceEmitter defines how trace entries are output.
// Internal builds use a JSONL file emitter; community builds use an OTEL SDK emitter.
type TraceEmitter interface {
	Init()
	Emit(ctx context.Context, entry *TraceLogEntry)
	Enabled() bool
}

var defaultEmitter TraceEmitter = &noopEmitter{}

// SetEmitter replaces the global trace emitter.
// Must be called before any TraceOperation calls (typically in main or NewControl).
func SetEmitter(e TraceEmitter) {
	defaultEmitter = e
}

type noopEmitter struct{}

func (n *noopEmitter) Init()                                        {}
func (n *noopEmitter) Emit(_ context.Context, _ *TraceLogEntry)     {}
func (n *noopEmitter) Enabled() bool                                { return false }
