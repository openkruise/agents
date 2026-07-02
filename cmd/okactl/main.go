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
	"strings"

	"github.com/spf13/cobra"

	"github.com/openkruise/agents/pkg/cli"
)

// usageTemplate is a custom cobra usage template that displays subcommand aliases.
// It is based on cobra's defaultUsageTemplate with alias support added to the
// Available Commands sections.
const usageTemplate = `Usage:{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

Aliases:
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

Examples:
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}{{$cmds := .Commands}}{{if eq (len .Groups) 0}}

Available Commands:{{range $cmds}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }}{{if .Aliases}} ({{join .Aliases ", "}}){{end}} {{.Short}}{{end}}{{end}}{{else}}{{range $group := .Groups}}

{{.Title}}{{range $cmds}}{{if (and (eq .GroupID $group.ID) (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }}{{if .Aliases}} ({{join .Aliases ", "}}){{end}} {{.Short}}{{end}}{{end}}{{end}}{{if not .AllChildCommandsHaveGroup}}

Additional Commands:{{range $cmds}}{{if (and (eq .GroupID "") (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }}{{if .Aliases}} ({{join .Aliases ", "}}){{end}} {{.Short}}{{end}}{{end}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

Flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

Global Flags:
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasHelpSubCommands}}

Additional help topics:{{range .Commands}}{{if .IsAdditionalHelpTopicCommand}}
  {{rpad .CommandPath .CommandPathPadding}} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableSubCommands}}

Use "{{.CommandPath}} [command] --help" for more information about a command.{{end}}
`

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

	// Register custom template functions and customize usage template to show
	// subcommand aliases in the Available Commands list.
	cobra.AddTemplateFunc("join", strings.Join)
	root.SetUsageTemplate(usageTemplate)

	scaleCmd := cli.NewScaleCommand(globalOpts)
	scaleCmd.GroupID = groupResource
	setCmd := cli.NewSetCommand(globalOpts)
	setCmd.GroupID = groupResource
	restartCmd := cli.NewRestartCommand(globalOpts)
	restartCmd.GroupID = groupResource
	createCmd := cli.NewCreateCommand(globalOpts)
	createCmd.GroupID = groupResource
	statusCmd := cli.NewStatusCommand(globalOpts)
	statusCmd.GroupID = groupResource

	root.AddCommand(scaleCmd, setCmd, restartCmd, createCmd, statusCmd)

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
