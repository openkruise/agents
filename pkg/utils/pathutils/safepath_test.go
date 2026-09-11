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

import "testing"

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
