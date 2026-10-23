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
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

// Version metadata set via -ldflags at build time.
var (
	Version   = "dev"
	GitCommit = ""
	BuildDate = ""
)

// NewVersionCommand returns the "version" command.
func NewVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the okactl version",
		Long:  `Print the okactl client version information.`,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return printVersion(cmd.OutOrStdout())
		},
	}
}

func printVersion(w io.Writer) error {
	_, err := fmt.Fprintln(w, formatVersion())
	return err
}

func formatVersion() string {
	var b strings.Builder
	b.WriteString("okactl version ")
	b.WriteString(Version)
	if GitCommit != "" {
		b.WriteString("  git:")
		b.WriteString(GitCommit)
	}
	if BuildDate != "" {
		b.WriteString("  built:")
		b.WriteString(BuildDate)
	}
	return b.String()
}
