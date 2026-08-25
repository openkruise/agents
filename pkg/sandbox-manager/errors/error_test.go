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

package errors

import (
	stderrors "errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestError(t *testing.T) {
	tests := []struct {
		name          string
		err           error
		expectCode    ErrorCode
		expectMessage string
	}{
		{
			name:          "internal error",
			err:           NewError(ErrorInternal, "foo"),
			expectCode:    ErrorInternal,
			expectMessage: "Internal: foo",
		},
		{
			name:       "quota exceeded",
			err:        NewError(ErrorQuotaExceeded, "quota exceeded"),
			expectCode: ErrorQuotaExceeded,
		},
		{
			name:       "wrapped typed error keeps its code",
			err:        fmt.Errorf("outer: %w", NewError(ErrorNotFound, "missing")),
			expectCode: ErrorNotFound,
		},
		{
			name:       "nil error is unknown",
			expectCode: ErrorUnknown,
		},
		{
			name:       "untyped error is unknown",
			err:        stderrors.New("plain"),
			expectCode: ErrorUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expectCode, GetErrCode(tt.err))
			if tt.expectMessage != "" {
				assert.Equal(t, tt.expectMessage, tt.err.Error())
			}
		})
	}
}

func TestWrapError(t *testing.T) {
	cause := stderrors.New("underlying failure")
	tests := []struct {
		name        string
		cause       error
		expectError string
		expectCause error
	}{
		{
			name:        "preserves cause",
			cause:       cause,
			expectError: "Internal: operation failed: underlying failure",
			expectCause: cause,
		},
		{
			name:        "allows nil cause",
			expectError: "Internal: operation failed: <nil>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := WrapError(ErrorInternal, tt.cause, "operation failed: %v", tt.cause)
			assert.Equal(t, tt.expectError, err.Error())
			assert.Equal(t, ErrorInternal, GetErrCode(err))
			if tt.expectCause != nil {
				assert.ErrorIs(t, err, tt.expectCause)
			} else {
				assert.NoError(t, err.Unwrap())
			}
		})
	}
}
