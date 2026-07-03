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
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	apiv1alpha1 "github.com/openkruise/agents/client/clientset/versioned/typed/api/v1alpha1"
)

type createSuoOptions struct {
	global   *GlobalOptions
	selector string
}

// NewCreateCommand returns the "create" command with its subcommands.
func NewCreateCommand(globalOpts *GlobalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create SUBCOMMAND",
		Short: "Create a resource",
		Long: `Create OpenKruise Agents resources.

Currently supports creating SandboxUpdateOps for batch updating claimed sandboxes.`,
	}
	cmd.AddCommand(newCreateSuoCommand(globalOpts))
	return cmd
}

func newCreateSuoCommand(globalOpts *GlobalOptions) *cobra.Command {
	opts := &createSuoOptions{global: globalOpts}

	cmd := &cobra.Command{
		Use:     "suo -l SELECTOR CONTAINER=IMAGE [CONTAINER=IMAGE ...]",
		Aliases: []string{"sandboxupdateops"},
		Short:   "Create a SandboxUpdateOps to update claimed sandbox images",
		Long: `Create a SandboxUpdateOps resource to batch update container images of claimed sandboxes.

This command creates a SandboxUpdateOps that applies a Strategic Merge Patch to all
sandboxes matching the label selector. Only claimed sandboxes (not controlled by
SandboxSet) can be updated this way.`,
		Example: `  # Update the gateway container image for all claimed sandboxes with app=openclaw
  okactl create suo -l app=openclaw gateway=nginx:1.27

  # Update multiple container images
  okactl create suo -l app=openclaw gateway=nginx:1.27 sidecar=envoy:1.28

  # Update in a specific namespace
  okactl -n production create suo -l app=openclaw gateway=nginx:1.27`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return opts.run(args)
		},
	}
	cmd.Flags().StringVarP(&opts.selector, "selector", "l", "", "Label selector to match target sandboxes (required)")
	_ = cmd.MarkFlagRequired("selector")
	return cmd
}

func (opts *createSuoOptions) run(imageArgs []string) error {
	if opts.selector == "" {
		return fmt.Errorf("--selector (-l) is required")
	}

	client, err := opts.global.AgentsClient()
	if err != nil {
		return err
	}
	return runCreateSuoWithClient(client, opts, imageArgs)
}

func runCreateSuoWithClient(client apiv1alpha1.ApiV1alpha1Interface, opts *createSuoOptions, imageArgs []string) error {
	if opts.selector == "" {
		return fmt.Errorf("--selector (-l) is required")
	}

	images, err := parseImageArgs(imageArgs)
	if err != nil {
		return err
	}

	ctx := context.TODO()
	ns := opts.global.Namespace

	patchData, err := buildSuoImagePatch(images)
	if err != nil {
		return fmt.Errorf("failed to build patch: %w", err)
	}

	labelSelector, err := metav1.ParseToLabelSelector(opts.selector)
	if err != nil {
		return fmt.Errorf("invalid label selector %q: %w", opts.selector, err)
	}

	// Validate that at least one sandbox matches the selector.
	selector, err := metav1.LabelSelectorAsSelector(labelSelector)
	if err != nil {
		return fmt.Errorf("invalid label selector %q: %w", opts.selector, err)
	}

	sandboxList, err := client.Sandboxes(ns).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return fmt.Errorf("failed to list sandboxes: %w", err)
	}

	if len(sandboxList.Items) == 0 {
		return fmt.Errorf("no sandboxes found matching selector %q in namespace %q", opts.selector, ns)
	}

	suo := &agentsv1alpha1.SandboxUpdateOps{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "suo-",
			Namespace:    ns,
		},
		Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
			Selector: labelSelector,
			Patch:    runtime.RawExtension{Raw: patchData},
		},
	}

	created, err := client.Sandboxupdateops(ns).Create(ctx, suo, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create SandboxUpdateOps: %w", err)
	}

	fmt.Printf("sandboxupdateops.agents.kruise.io/%s created (selector: %s, images: %s)\n",
		created.Name, opts.selector, strings.Join(formatSuoImagePairs(images), ", "))
	return nil
}

// buildSuoImagePatch generates a Strategic Merge Patch JSON for container image updates.
// The patch is applied to the sandbox's spec.template (PodTemplateSpec),
// so the structure must be relative to PodTemplateSpec (spec.containers),
// NOT relative to SandboxSpec (spec.template.spec.containers).
func buildSuoImagePatch(images map[string]string) ([]byte, error) {
	names := make([]string, 0, len(images))
	for name := range images {
		names = append(names, name)
	}
	sort.Strings(names)

	containers := make([]map[string]string, 0, len(images))
	for _, name := range names {
		containers = append(containers, map[string]string{
			"name":  name,
			"image": images[name],
		})
	}

	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"containers": containers,
		},
	}
	return json.Marshal(patch)
}

// formatSuoImagePairs formats a map of container=image pairs as a slice of "container=image" strings.
func formatSuoImagePairs(images map[string]string) []string {
	names := make([]string, 0, len(images))
	for name := range images {
		names = append(names, name)
	}
	sort.Strings(names)

	pairs := make([]string, 0, len(images))
	for _, name := range names {
		pairs = append(pairs, name+"="+images[name])
	}
	return pairs
}
