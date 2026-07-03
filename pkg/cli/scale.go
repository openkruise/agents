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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv1alpha1 "github.com/openkruise/agents/client/clientset/versioned/typed/api/v1alpha1"
)

type scaleOptions struct {
	global   *GlobalOptions
	replicas int32
}

// NewScaleCommand returns the "scale" command with its subcommands.
func NewScaleCommand(globalOpts *GlobalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scale",
		Short: "Scale a resource to a desired replica count",
		Long: `Scale an OpenKruise Agents resource to a desired number of replicas.

Currently supports scaling SandboxSet resources.`,
	}
	cmd.AddCommand(newScaleSandboxSetCommand(globalOpts))
	return cmd
}

func newScaleSandboxSetCommand(globalOpts *GlobalOptions) *cobra.Command {
	opts := &scaleOptions{global: globalOpts}

	cmd := &cobra.Command{
		Use:     "sandboxset NAME --replicas=N",
		Aliases: []string{"sbs"},
		Short:   "Scale a SandboxSet to the specified number of replicas",
		Long: `Scale a SandboxSet to the specified number of idle sandbox replicas.

The SandboxSet maintains a pool of pre-warmed, idle Sandbox instances.
Scaling up adds more idle sandboxes; scaling down removes excess ones.
Setting --replicas=0 drains the pool entirely.`,
		Example: `  # Scale the pool to 5 idle sandboxes
  okactl scale sandboxset my-pool --replicas=5

  # Use the short name "sbs"
  okactl scale sbs my-pool --replicas=5

  # Drain the pool completely
  okactl scale sandboxset my-pool --replicas=0

  # Scale in a specific namespace
  okactl -n agent-system scale sandboxset my-pool --replicas=10`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return opts.run(args[0])
		},
	}
	cmd.Flags().Int32Var(&opts.replicas, "replicas", 0, "The new desired number of replicas (required)")
	_ = cmd.MarkFlagRequired("replicas")
	return cmd
}

func (opts *scaleOptions) run(name string) error {
	client, err := opts.global.AgentsClient()
	if err != nil {
		return err
	}
	return runScaleWithClient(client, opts, name)
}

func runScaleWithClient(client apiv1alpha1.ApiV1alpha1Interface, opts *scaleOptions, name string) error {
	if opts.replicas < 0 {
		return fmt.Errorf("--replicas must be >= 0, got %d", opts.replicas)
	}

	ctx := context.TODO()

	patch := fmt.Sprintf(`{"spec":{"replicas":%d}}`, opts.replicas)
	_, err := client.SandboxSets(opts.global.Namespace).Patch(
		ctx, name, types.MergePatchType, []byte(patch), metav1.PatchOptions{},
	)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("sandboxset %q not found", name)
		}
		return fmt.Errorf("failed to scale sandboxset %q: %w", name, err)
	}

	fmt.Printf("sandboxset.agents.kruise.io/%s scaled to %d\n", name, opts.replicas)
	return nil
}
