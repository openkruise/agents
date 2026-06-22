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
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/openkruise/agents/pkg/cli"
)

func main() {
	const (
		groupResource = "resource"
		groupOther    = "other"
	)

	globalOpts := cli.NewGlobalOptions()

	root := &cobra.Command{
		Use:   "okactl",
		Short: "okactl controls the OpenKruise Agents sandbox cluster",
		Long: `okactl controls the OpenKruise Agents sandbox cluster.

 Find more information at: https://github.com/openkruise/agents`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	globalOpts.AddFlags(root.PersistentFlags())

	root.AddGroup(&cobra.Group{ID: groupResource, Title: "Resource Commands:"})
	root.AddGroup(&cobra.Group{ID: groupOther, Title: "Other Commands:"})

	scaleCmd := cli.NewScaleCommand(globalOpts)
	scaleCmd.GroupID = groupResource
	setCmd := cli.NewSetCommand(globalOpts)
	setCmd.GroupID = groupResource
	restartCmd := cli.NewRestartCommand(globalOpts)
	restartCmd.GroupID = groupResource
	createCmd := cli.NewCreateCommand(globalOpts)
	createCmd.GroupID = groupResource

	root.AddCommand(scaleCmd, setCmd, restartCmd, createCmd)

	// Assign group ID to auto-generated commands (completion, help)
	for _, cmd := range root.Commands() {
		if cmd.GroupID == "" {
			cmd.GroupID = groupOther
		}
	}

	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
