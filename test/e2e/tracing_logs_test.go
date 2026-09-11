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

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// spansForTrace 从混合日志中解析指定 trace 的 span，返回 SpanID 到名称的映射。
// 同时支持紧凑和多行 JSON；按 SpanID 去重，且只检查 SpanContext.TraceID。
// 本文件可通过 go test test/e2e/tracing_logs_test.go 独立验证，不加载集群初始化。
func spansForTrace(logs, traceID string) map[string]string {
	spans := make(map[string]string)
	if traceID == "" {
		return spans
	}
	for logs != "" {
		line, rest, _ := strings.Cut(logs, "\n")
		if !strings.HasPrefix(strings.TrimSpace(line), "{") {
			logs = rest
			continue
		}
		var span struct {
			Name        string
			SpanContext struct {
				TraceID string
				SpanID  string
			}
		}
		decoder := json.NewDecoder(strings.NewReader(logs))
		if err := decoder.Decode(&span); err != nil {
			// 日志可能含非 JSON 内容或尚未写完的记录，跳过当前行后继续解析。
			logs = rest
			continue
		}
		logs = logs[decoder.InputOffset():]
		if span.Name != "" && span.SpanContext.TraceID == traceID && span.SpanContext.SpanID != "" {
			spans[span.SpanContext.SpanID] = span.Name
		}
	}
	return spans
}

func TestSpansForTrace(t *testing.T) {
	const (
		traceID      = "50293b018bb7d1f4e75518d2ba6feb0e"
		otherTraceID = "11111111111111111111111111111111"
		spanID       = "5bffabc97d55aa1a"
		otherSpanID  = "b66efda2b875d73a"
	)
	spanJSON := func(name, tid, id string) string {
		return fmt.Sprintf(`{"Name":%q,"SpanContext":{"TraceID":%q,"SpanID":%q},"Parent":{"TraceID":%q,"SpanID":"0123456789abcdef"}}`, name, tid, id, tid)
	}
	compact := spanJSON("POST /sandboxes", traceID, spanID)
	var pretty bytes.Buffer
	require.NoError(t, json.Indent(&pretty, []byte(compact), "", "\t"))
	wantSpan := map[string]string{spanID: "POST /sandboxes"}

	tests := []struct {
		name string
		logs string
		want map[string]string
	}{
		{
			name: "compact JSON without final newline",
			logs: compact,
			want: wantSpan,
		},
		{
			name: "pretty JSON with surrounding whitespace",
			logs: " \n\t" + pretty.String() + "\r\n",
			want: wantSpan,
		},
		{
			name: "mixed application logs and spans",
			logs: "I0910 startup completed\n{\"level\":\"info\",\"msg\":\"ready\"}\n" + compact + "\nI0910 request completed\n",
			want: wantSpan,
		},
		{
			name: "duplicate records and parent trace ID count once",
			logs: compact + "\n" + pretty.String(),
			want: wantSpan,
		},
		{
			name: "distinct spans with the same name count separately",
			logs: compact + "\n" + spanJSON("POST /sandboxes", traceID, otherSpanID),
			want: map[string]string{spanID: "POST /sandboxes", otherSpanID: "POST /sandboxes"},
		},
		{
			name: "name from another trace does not match",
			logs: spanJSON("POST /sandboxes", otherTraceID, spanID) + "\n" + spanJSON("unrelated", traceID, otherSpanID),
			want: map[string]string{otherSpanID: "unrelated"},
		},
		{
			name: "parent trace ID alone does not match",
			logs: strings.Replace(compact, traceID, otherTraceID, 1),
			want: map[string]string{},
		},
		{
			name: "missing span ID is not a span",
			logs: spanJSON("POST /sandboxes", traceID, ""),
			want: map[string]string{},
		},
		{
			name: "missing name is not a span",
			logs: spanJSON("", traceID, spanID),
			want: map[string]string{},
		},
		{
			name: "JSON embedded in an application message is not a span",
			logs: fmt.Sprintf(`{"level":"info","msg":%q}`, compact),
			want: map[string]string{},
		},
		{
			name: "malformed record does not hide the next span",
			logs: "{invalid JSON}\n{\"Name\":\n" + compact,
			want: wantSpan,
		},
		{
			name: "truncated final record is ignored",
			logs: compact + "\n{\"Name\":\"unfinished\"",
			want: wantSpan,
		},
		{
			name: "span larger than scanner default token limit",
			logs: strings.TrimSuffix(compact, "}") + fmt.Sprintf(",\"Extra\":%q}", strings.Repeat("x", 128*1024)),
			want: wantSpan,
		},
		{
			name: "empty logs",
			want: map[string]string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, spansForTrace(tt.logs, traceID))
		})
	}
	t.Run("empty trace ID does not match incomplete records", func(t *testing.T) {
		assert.Empty(t, spansForTrace(spanJSON("POST /sandboxes", "", spanID), ""))
	})
}

func TestSpansForTraceStdoutExporter(t *testing.T) {
	for _, pretty := range []bool{false, true} {
		t.Run(fmt.Sprintf("pretty=%t", pretty), func(t *testing.T) {
			var output bytes.Buffer
			opts := []stdouttrace.Option{stdouttrace.WithWriter(&output)}
			if pretty {
				opts = append(opts, stdouttrace.WithPrettyPrint())
			}
			exporter, err := stdouttrace.New(opts...)
			require.NoError(t, err)
			provider := sdktrace.NewTracerProvider(
				sdktrace.WithSyncer(exporter),
				sdktrace.WithSampler(sdktrace.AlwaysSample()),
			)
			t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })
			tracer := provider.Tracer("stdout-parser-test")
			ctx, root := tracer.Start(t.Context(), "POST /sandboxes")
			_, child := tracer.Start(ctx, "controller.Reconcile")
			child.End()
			root.End()
			_, unrelated := tracer.Start(t.Context(), "controller.CreatePod")
			unrelated.End()

			got := spansForTrace(output.String(), root.SpanContext().TraceID().String())
			assert.Equal(t, map[string]string{
				root.SpanContext().SpanID().String():  "POST /sandboxes",
				child.SpanContext().SpanID().String(): "controller.Reconcile",
			}, got)
		})
	}
}
