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
	"context"
	"fmt"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	apiv1alpha1 "github.com/openkruise/agents/client/clientset/versioned/typed/api/v1alpha1"
)

// NewStatusCommand returns the "status" command with its subcommands.
func NewStatusCommand(globalOpts *GlobalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the status of a resource",
		Long: `Show the status of OpenKruise Agents resources.

These commands help you monitor the progress of resource updates.`,
	}
	cmd.AddCommand(newStatusSandboxSetCommand(globalOpts))
	cmd.AddCommand(newStatusSandboxUpdateOpsCommand(globalOpts))
	return cmd
}

func newStatusSandboxSetCommand(globalOpts *GlobalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "sbs NAME",
		Aliases: []string{"sandboxset"},
		Short:   "Show the update progress of a SandboxSet",
		Long: `Show the rolling update progress of a SandboxSet after "set image".

Displays how many replicas have been updated and how many are available.
If the update is stalled, automatically diagnoses the issue by checking
sandbox and pod status (e.g., ImagePullBackOff, insufficient resources).`,
		Example: `  # Show current update progress
  okactl status sbs openclaw-sbs`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := globalOpts.AgentsClient()
			if err != nil {
				return err
			}
			return runSetImageStatusWithClient(client, globalOpts, args[0])
		},
	}
	return cmd
}

func newStatusSandboxUpdateOpsCommand(globalOpts *GlobalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "suo NAME",
		Aliases: []string{"sandboxupdateops"},
		Short:   "Show the update progress of a SandboxUpdateOps",
		Long: `Show the progress of a SandboxUpdateOps batch update operation.

Displays the current phase, total/updated/updating/failed replica counts.`,
		Example: `  # Show current SUO progress
  okactl status suo suo-zk7h7`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := globalOpts.AgentsClient()
			if err != nil {
				return err
			}
			return runSuoStatusWithClient(client, globalOpts, args[0])
		},
	}
	return cmd
}

func runSuoStatusWithClient(client apiv1alpha1.ApiV1alpha1Interface, globalOpts *GlobalOptions, name string) error {
	ctx := context.TODO()
	ns := globalOpts.Namespace

	suo, err := client.Sandboxupdateops(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get sandboxupdateops %q: %w", name, err)
	}

	printSuoStatus(suo)
	return nil
}

// printSuoStatus prints a one-line status of the SandboxUpdateOps.
func printSuoStatus(suo *agentsv1alpha1.SandboxUpdateOps) {
	status := suo.Status
	fmt.Printf("%-30s %-10s %d/%d updated  %d updating  %d failed\n",
		suo.Name, status.Phase,
		status.UpdatedReplicas, status.Replicas,
		status.UpdatingReplicas,
		status.FailedReplicas)
}
