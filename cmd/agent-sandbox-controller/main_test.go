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

package main

import (
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResourceAccessModeStartup(t *testing.T) {
	if os.Getenv("AGENTS_TEST_RESOURCE_ACCESS_STARTUP") == "true" {
		os.Args = append([]string{os.Args[0]}, os.Args[3:]...)
		main()
		return
	}
	for _, tt := range []struct {
		name, arg, want string
		wantErr         bool
	}{
		{name: "flag defaults to trusted-domain", arg: "--help", want: `--resource-access-mode string`},
		{name: "unknown mode fails before Kubernetes setup", arg: "--resource-access-mode=invalid", want: `unsupported resource access mode`, wantErr: true},
		{name: "empty mode fails before Kubernetes setup", arg: "--resource-access-mode=", want: `--resource-access-mode must not be empty`, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestResourceAccessModeStartup$", "--", tt.arg)
			cmd.Env = append(os.Environ(), "AGENTS_TEST_RESOURCE_ACCESS_STARTUP=true")
			output, err := cmd.CombinedOutput()
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err, string(output))
				assert.Contains(t, string(output), `(default "trusted-domain")`)
			}
			assert.Contains(t, string(output), tt.want)
		})
	}
}
