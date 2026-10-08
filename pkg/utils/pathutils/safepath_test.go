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

package pathutils

import (
	"strings"
	"testing"
)

func TestValidateSafePath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{
			name:    "empty path",
			path:    "",
			wantErr: true,
		},
		{
			name: "absolute path",
			path: "/run/csi/mount-root/nas/hash",
		},
		{
			name: "relative path",
			path: "relative/path",
		},
		{
			name: "dot dot within file name",
			path: "a..b/file",
		},
		{
			name:    "leading parent directory",
			path:    "../etc/passwd",
			wantErr: true,
		},
		{
			name:    "multiple parent directories",
			path:    "a/../../b",
			wantErr: true,
		},
		{
			name:    "parent directory only",
			path:    "..",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSafePath(tt.path)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateSafePath(%q) error = %v, wantErr %t", tt.path, err, tt.wantErr)
			}
		})
	}
}

func TestNormalizeAbsoluteNonRootPath(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		want        string
		errorMarker string
	}{
		{name: "absolute path", input: "/workspace/data", want: "/workspace/data"},
		{name: "canonicalizes separators and dots", input: "/workspace//./data///", want: "/workspace/data"},
		{name: "dot dot inside filename", input: "/workspace/a..b", want: "/workspace/a..b"},
		{name: "empty path", input: "", errorMarker: "must not be empty"},
		{name: "NUL byte", input: "/workspace/\x00data", errorMarker: "NUL bytes"},
		{name: "relative path", input: "workspace/data", errorMarker: "must be absolute"},
		{name: "filesystem root", input: "/", errorMarker: "filesystem root"},
		{name: "dot collapsing to root", input: "/.", errorMarker: "filesystem root"},
		{name: "parent segment", input: "/workspace/../data", errorMarker: "'..' segments"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeAbsoluteNonRootPath(tt.input)
			if tt.errorMarker != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errorMarker) {
					t.Fatalf("NormalizeAbsoluteNonRootPath(%q) error = %v, want marker %q", tt.input, err, tt.errorMarker)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeAbsoluteNonRootPath(%q) unexpected error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("NormalizeAbsoluteNonRootPath(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
