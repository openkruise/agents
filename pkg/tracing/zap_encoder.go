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

import (
	"bytes"
	"encoding/json"

	"go.uber.org/zap"
	"go.uber.org/zap/buffer"
	"go.uber.org/zap/zapcore"
)

// TraceIDLogKey is the structured-log field key carrying the trace ID that
// controller code injects into its logger after StartReconcileSpan (see
// TraceIDFromContext). Encoders and call sites must share this constant so
// the key never drifts.
const TraceIDLogKey = "traceID"

// splicePool recycles the buffers returned by the splicing encoder below.
var splicePool = buffer.NewPool()

// NewTraceFirstJSONEncoder returns a JSON encoder that emits every log line
// as one JSON object with the trace ID, when present, as its first field:
//
//	{"traceID":"...","level":"INFO","ts":"...","msg":"...",...}
//
// It builds on the zap production encoder config with ISO8601 timestamps and
// capitalized levels so lines stay as readable as the previous console
// format.
func NewTraceFirstJSONEncoder() zapcore.Encoder {
	cfg := zap.NewProductionEncoderConfig()
	cfg.EncodeTime = zapcore.ISO8601TimeEncoder
	cfg.EncodeLevel = zapcore.CapitalLevelEncoder
	return NewTraceFirstEncoder(zapcore.NewJSONEncoder(cfg))
}

// NewTraceFirstEncoder wraps base so that the TraceIDLogKey field, when
// present, is emitted as the first key of each encoded entry instead of its
// natural position at the end of the accumulated logger context. This keeps
// the trace ID at a fixed leading position for log collectors that index the
// first field of JSON log lines.
func NewTraceFirstEncoder(base zapcore.Encoder) zapcore.Encoder {
	return &traceFirstEncoder{Encoder: base}
}

// traceFirstEncoder implements zapcore.Encoder by delegation (embedding) and
// overrides AddString and EncodeEntry to promote the trace ID field.
//
// It must intercept AddString because zap never delivers logger context
// fields (With/WithValues) to EncodeEntry's fields slice: ioCore.With writes
// them straight into the encoder via its ObjectEncoder methods. The wrapper
// therefore swallows the trace ID AddString call, remembers the value, and
// re-emits it during EncodeEntry ahead of everything else.
//
// Pointer receivers are required because AddString mutates the captured
// trace ID; NewTraceFirstEncoder and Clone must both return pointers so the
// mutation is visible to ioCore.Write.
type traceFirstEncoder struct {
	zapcore.Encoder

	// traceID is captured from AddString calls made by ioCore.With while
	// the logger accumulates context fields (e.g. klog WithValues chains).
	traceID string
}

// Clone keeps the wrapper around the cloned base encoder and carries over
// the captured trace ID: zap clones the encoder on every Logger.With, so a
// value captured by an earlier With would otherwise be lost.
func (e *traceFirstEncoder) Clone() zapcore.Encoder {
	return &traceFirstEncoder{Encoder: e.Encoder.Clone(), traceID: e.traceID}
}

// AddString captures the trace ID instead of letting it flow into the base
// encoder's field namespace; every other key is forwarded unchanged.
func (e *traceFirstEncoder) AddString(key, val string) {
	if key == TraceIDLogKey {
		e.traceID = val
		return
	}
	e.Encoder.AddString(key, val)
}

// EncodeEntry encodes the entry without the trace ID field and splices the
// captured value right after the leading '{' of the base encoding, making it
// the first key of the JSON object. Entries without a trace ID (startup
// logs, paths outside a Reconcile) are encoded unchanged.
func (e *traceFirstEncoder) EncodeEntry(entry zapcore.Entry, fields []zapcore.Field) (*buffer.Buffer, error) {
	// Prefer the value captured from With/WithValues; a trace ID supplied as
	// a per-call field (when callers log directly with the encoder) is used
	// as a fallback. The per-call copy is dropped so the ID is never emitted
	// twice.
	traceID := e.traceID
	idx := -1
	for i := range fields {
		if fields[i].Key == TraceIDLogKey && fields[i].Type == zapcore.StringType {
			idx = i
			if traceID == "" {
				traceID = fields[i].String
			}
			break
		}
	}
	if idx < 0 && traceID == "" {
		return e.Encoder.EncodeEntry(entry, fields)
	}

	rest := make([]zapcore.Field, 0, len(fields))
	for i := range fields {
		if i == idx {
			continue
		}
		rest = append(rest, fields[i])
	}

	encoded, err := e.Encoder.EncodeEntry(entry, rest)
	if err != nil {
		return nil, err
	}

	// A string always marshals; the error branch only satisfies the linter.
	quoted, err := json.Marshal(traceID)
	if err != nil {
		encoded.Free()
		return nil, err
	}

	if encoded.Len() == 0 || encoded.Bytes()[0] != '{' {
		// base does not emit a JSON object (e.g. a console encoder): splice
		// is impossible, so re-encode with the trace ID moved to the front
		// of the field list instead.
		encoded.Free()
		promoted := make([]zapcore.Field, 0, len(fields)+1)
		promoted = append(promoted, zap.String(TraceIDLogKey, traceID))
		promoted = append(promoted, rest...)
		return e.Encoder.EncodeEntry(entry, promoted)
	}

	spliced := splicePool.Get()
	spliced.AppendByte('{')
	spliced.AppendString(`"` + TraceIDLogKey + `":`)
	spliced.AppendBytes(quoted)
	// Empty objects may include a line ending. Add a comma only when the
	// original object has fields, and preserve its unmodified suffix.
	body := bytes.TrimSpace(encoded.Bytes()[1:])
	if len(body) > 0 && body[0] != '}' {
		spliced.AppendByte(',')
	}
	spliced.AppendBytes(encoded.Bytes()[1:])
	encoded.Free()
	return spliced, nil
}
