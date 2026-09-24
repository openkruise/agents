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

package customfusesidecar

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadProxyMsg(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		want        proxyResponse
		expectError string
	}{
		{
			name:  "valid message with terminator",
			input: `{"seq":7,"error":""}` + string(proxyMessageEnd),
			want:  proxyResponse{Seq: 7},
		},
		{
			name:  "error message decodes",
			input: `{"error":"mount failed"}` + string(proxyMessageEnd),
			want:  proxyResponse{Error: "mount failed"},
		},
		{
			name:        "missing terminator is rejected",
			input:       `{"seq":1}`,
			expectError: "read proxy message end",
		},
		{
			name:        "malformed json is rejected",
			input:       `{broken` + string(proxyMessageEnd),
			expectError: "decode proxy message",
		},
		{
			name:        "empty input is rejected",
			input:       "",
			expectError: "decode proxy message",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got proxyResponse
			err := readProxyMsg(strings.NewReader(tt.input), &got)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestReadProxyMsgOversize(t *testing.T) {
	// A valid JSON frame whose payload exceeds maxProxyMsgSize exhausts the
	// LimitedReader during decoding and must be rejected with a size error.
	// Shrink the limit so the payload stays small instead of allocating a
	// 256MB string.
	old := maxProxyMsgSize
	maxProxyMsgSize = 1 << 10
	t.Cleanup(func() { maxProxyMsgSize = old })

	big := `{"seq":1,"error":"` + strings.Repeat("x", maxProxyMsgSize) + `"}`
	var got proxyResponse
	err := readProxyMsg(bytes.NewReader([]byte(big)), &got)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "too large")
}

func TestProxyMountPropagatesServerError(t *testing.T) {
	// Mount must surface the server-side error message so the node server can
	// map it to a CSI status code.
	orig := proxyDoRequestFn
	proxyDoRequestFn = func(_ *proxyClient, _ context.Context, _ *proxyRequest) (*proxyResponse, error) {
		return &proxyResponse{Error: "entrypoint exited: boom"}, nil
	}
	defer func() { proxyDoRequestFn = orig }()

	client := newProxyClient("/var/run/csi/mounter.sock")
	err := client.Mount(context.Background(), &proxyMountRequest{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "entrypoint exited: boom")
}
