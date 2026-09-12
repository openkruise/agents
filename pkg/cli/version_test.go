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

package cli

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewVersionCommand(t *testing.T) {
	cmd := NewVersionCommand()
	assert.Equal(t, "version", cmd.Use)
	assert.NotEmpty(t, cmd.Short)
	assert.False(t, cmd.HasSubCommands())
}

func TestFormatVersion(t *testing.T) {
	tests := []struct {
		name      string
		version   string
		gitCommit string
		buildDate string
		want      string
	}{
		{
			name:    "version only",
			version: "v0.1.0",
			want:    "okactl version v0.1.0",
		},
		{
			name:      "version with commit and date",
			version:   "v0.1.0",
			gitCommit: "abc1234",
			buildDate: "2026-09-12T00:00:00Z",
			want:      "okactl version v0.1.0  git:abc1234  built:2026-09-12T00:00:00Z",
		},
		{
			name:      "version with commit only",
			version:   "dev",
			gitCommit: "deadbeef",
			want:      "okactl version dev  git:deadbeef",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			origVersion, origCommit, origDate := Version, GitCommit, BuildDate
			t.Cleanup(func() {
				Version, GitCommit, BuildDate = origVersion, origCommit, origDate
			})
			Version = tt.version
			GitCommit = tt.gitCommit
			BuildDate = tt.buildDate
			assert.Equal(t, tt.want, formatVersion())
		})
	}
}

func TestVersionCommandPrintsInjectedVersion(t *testing.T) {
	origVersion, origCommit, origDate := Version, GitCommit, BuildDate
	t.Cleanup(func() {
		Version, GitCommit, BuildDate = origVersion, origCommit, origDate
	})
	Version = "v9.9.9"
	GitCommit = ""
	BuildDate = ""

	cmd := NewVersionCommand()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetArgs(nil)
	require.NoError(t, cmd.Execute())
	assert.Equal(t, "okactl version v9.9.9\n", buf.String())
}
