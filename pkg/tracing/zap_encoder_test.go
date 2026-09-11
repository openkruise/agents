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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// testJSONEncoder builds a base JSON encoder equivalent to the one used by
// NewTraceFirstJSONEncoder.
func testJSONEncoder() zapcore.Encoder {
	cfg := zap.NewProductionEncoderConfig()
	cfg.EncodeTime = zapcore.ISO8601TimeEncoder
	return zapcore.NewJSONEncoder(cfg)
}

// topLevelKeys decodes a single-line JSON object and returns its top-level
// keys in encoding order.
func topLevelKeys(t *testing.T, line string) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(line))
	tok, err := dec.Token()
	require.NoError(t, err)
	require.Equal(t, json.Delim('{'), tok, "line must be a JSON object: %s", line)

	var keys []string
	for dec.More() {
		keyTok, err := dec.Token()
		require.NoError(t, err)
		key, ok := keyTok.(string)
		require.True(t, ok, "object key must be a string: %s", line)
		keys = append(keys, key)

		var raw json.RawMessage
		require.NoError(t, dec.Decode(&raw), "failed to skip value of %q", key)
	}
	return keys
}

// testTime fills Entry.Time so the "ts" key is emitted; zap omits the
// timestamp for a zero entry time.
var testTime = time.Date(2026, 8, 11, 11, 46, 3, 0, time.Local)

func TestTraceFirstEncoderEncodeEntry(t *testing.T) {
	entry := zapcore.Entry{Level: zapcore.InfoLevel, Message: "update sandbox status success", Time: testTime}

	type testCase struct {
		name           string
		fields         []zapcore.Field
		encoderConfig  *zapcore.EncoderConfig
		contextTraceID string
		wantKeys       []string
		wantTraceID    any
		wantLine       string
	}
	cases := []testCase{
		{
			name: "trace id in the middle is promoted to the first key",
			fields: []zapcore.Field{
				zap.String("controller", "sandbox-controller"),
				zap.String("reconcileID", "1011dda1"),
				zap.String(TraceIDLogKey, "dce858203322f71c3598d87c81e88a5b"),
				zap.String("sandbox", "box"),
				zap.String("status", "{}"),
			},
			wantKeys: []string{
				TraceIDLogKey, "level", "ts", "msg",
				"controller", "reconcileID", "sandbox", "status",
			},
			wantTraceID: "dce858203322f71c3598d87c81e88a5b",
		},
		{
			name: "trace id as the last context field is promoted",
			fields: []zapcore.Field{
				zap.String("controller", "sandbox-controller"),
				zap.String(TraceIDLogKey, "tid"),
			},
			wantKeys:    []string{TraceIDLogKey, "level", "ts", "msg", "controller"},
			wantTraceID: "tid",
		},
		{
			name: "trace id already first keeps field order",
			fields: []zapcore.Field{
				zap.String(TraceIDLogKey, "tid"),
				zap.String("controller", "sandbox-controller"),
			},
			wantKeys:    []string{TraceIDLogKey, "level", "ts", "msg", "controller"},
			wantTraceID: "tid",
		},
		{
			name: "no trace id encodes unchanged",
			fields: []zapcore.Field{
				zap.String("controller", "sandbox-controller"),
				zap.String("status", "running"),
			},
			wantKeys:    []string{"level", "ts", "msg", "controller", "status"},
			wantTraceID: nil, // key absent
		},
		{
			name: "non string trace id field is not promoted",
			fields: []zapcore.Field{
				zap.Int32(TraceIDLogKey, 7),
				zap.String("controller", "sandbox-controller"),
			},
			wantKeys:    []string{"level", "ts", "msg", TraceIDLogKey, "controller"},
			wantTraceID: float64(7),
		},
	}

	for _, ending := range []struct {
		name   string
		config zapcore.EncoderConfig
		suffix string
	}{
		{name: "no line ending", config: zapcore.EncoderConfig{SkipLineEnding: true}},
		{name: "default LF", suffix: "\n"},
		{name: "CRLF", config: zapcore.EncoderConfig{LineEnding: "\r\n"}, suffix: "\r\n"},
	} {
		// Disabling built-in fields makes the real Zap encoder emit an empty object.
		emptyCase := testCase{
			name:          "empty object with per-call trace id/" + ending.name,
			fields:        []zapcore.Field{zap.String(TraceIDLogKey, "tid")},
			encoderConfig: &ending.config,
			wantKeys:      []string{TraceIDLogKey},
			wantTraceID:   "tid",
			wantLine:      `{"traceID":"tid"}` + ending.suffix,
		}
		cases = append(cases, emptyCase)

		emptyCase.name = "empty object with context trace id/" + ending.name
		emptyCase.fields = nil
		emptyCase.contextTraceID = "tid"
		cases = append(cases, emptyCase)

		emptyCase.name = "empty config with remaining field/" + ending.name
		emptyCase.fields = []zapcore.Field{zap.String("controller", "c")}
		emptyCase.wantKeys = []string{TraceIDLogKey, "controller"}
		emptyCase.wantLine = `{"traceID":"tid","controller":"c"}` + ending.suffix
		cases = append(cases, emptyCase)

		emptyCase.name = "empty object without trace id is unchanged/" + ending.name
		emptyCase.fields = nil
		emptyCase.contextTraceID = ""
		emptyCase.wantKeys = nil
		emptyCase.wantTraceID = nil
		emptyCase.wantLine = "{}" + ending.suffix
		cases = append(cases, emptyCase)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := testJSONEncoder()
			if tc.encoderConfig != nil {
				base = zapcore.NewJSONEncoder(*tc.encoderConfig)
			}
			enc := NewTraceFirstEncoder(base)
			if tc.contextTraceID != "" {
				enc.AddString(TraceIDLogKey, tc.contextTraceID)
			}
			buf, err := enc.EncodeEntry(entry, tc.fields)
			require.NoError(t, err)
			line := buf.String()
			buf.Free()

			assert.True(t, json.Valid([]byte(line)), "output must be valid JSON: %s", line)
			assert.Equal(t, tc.wantKeys, topLevelKeys(t, line))
			if tc.wantLine != "" {
				assert.Equal(t, tc.wantLine, line, "output must preserve the original line ending")
			}

			var decoded map[string]any
			require.NoError(t, json.Unmarshal([]byte(line), &decoded))
			assert.Equal(t, tc.wantTraceID, decoded[TraceIDLogKey])
		})
	}
}

func TestTraceFirstJSONEncoderPromotesTraceID(t *testing.T) {
	enc := NewTraceFirstJSONEncoder()
	buf, err := enc.EncodeEntry(
		zapcore.Entry{Level: zapcore.InfoLevel, Message: "m", Time: testTime},
		[]zapcore.Field{
			zap.String("controller", "sandbox-controller"),
			zap.String(TraceIDLogKey, "dce858203322f71c3598d87c81e88a5b"),
		},
	)
	require.NoError(t, err)
	line := buf.String()
	buf.Free()

	assert.True(t, strings.HasPrefix(line,
		`{"traceID":"dce858203322f71c3598d87c81e88a5b","level":"INFO"`), "got: %s", line)
}

// TestTraceFirstEncoderWithZapLogger exercises the real write path: logger
// context fields (controller-runtime's WithValues chain plus the trace ID)
// are accumulated before the per-call fields, exactly like the klog-to-zapr
// bridging used by the controller.
func TestTraceFirstEncoderWithZapLogger(t *testing.T) {
	var out bytes.Buffer
	core := zapcore.NewCore(NewTraceFirstEncoder(testJSONEncoder()), zapcore.AddSync(&out), zapcore.InfoLevel)
	logger := zap.New(core)

	logger.
		With(zap.String("controller", "sandbox-controller"), zap.String("reconcileID", "1011dda1")).
		With(zap.String(TraceIDLogKey, "dce858203322f71c3598d87c81e88a5b")).
		Info("update sandbox status success", zap.String("sandbox", "box"), zap.String("status", "{\"phase\":\"Running\"}"))

	line := strings.TrimSpace(out.String())
	assert.Equal(t, []string{
		TraceIDLogKey, "level", "ts", "msg",
		"controller", "reconcileID", "sandbox", "status",
	}, topLevelKeys(t, line))

	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(line), &decoded))
	assert.Equal(t, "dce858203322f71c3598d87c81e88a5b", decoded[TraceIDLogKey])
	assert.Equal(t, "{\"phase\":\"Running\"}", decoded["status"])
}

func TestTraceFirstEncoderClone(t *testing.T) {
	enc := NewTraceFirstEncoder(testJSONEncoder())
	cloned, ok := enc.Clone().(*traceFirstEncoder)
	require.True(t, ok, "Clone must keep the wrapper")

	buf, err := cloned.EncodeEntry(
		zapcore.Entry{Level: zapcore.InfoLevel, Message: "m", Time: testTime},
		[]zapcore.Field{zap.String("controller", "c"), zap.String(TraceIDLogKey, "tid")},
	)
	require.NoError(t, err)
	line := buf.String()
	buf.Free()

	assert.Equal(t, []string{TraceIDLogKey, "level", "ts", "msg", "controller"}, topLevelKeys(t, line))
}

// TestTraceFirstEncoderConsoleFallback covers a non-object base encoder: the
// splice is impossible, so the trace ID is moved to the front of the field
// list instead of being dropped.
func TestTraceFirstEncoderConsoleFallback(t *testing.T) {
	cfg := zap.NewDevelopmentEncoderConfig()
	enc := NewTraceFirstEncoder(zapcore.NewConsoleEncoder(cfg))

	buf, err := enc.EncodeEntry(
		zapcore.Entry{Level: zapcore.InfoLevel, Message: "m", Time: testTime},
		[]zapcore.Field{zap.String("controller", "c"), zap.String(TraceIDLogKey, "tid")},
	)
	require.NoError(t, err)
	line := buf.String()
	buf.Free()

	assert.NotEmpty(t, line)
	// The console encoder separates fields with ", " after the colon.
	assert.Contains(t, line, `"traceID": "tid"`)
	assert.Contains(t, line, `"controller": "c"`)
}
