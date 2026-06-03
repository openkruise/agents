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
	"strings"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1alpha1 "github.com/openkruise/agents/client/clientset/versioned/typed/api/v1alpha1"
)

type setImageOptions struct {
	global *GlobalOptions
}

// NewSetCommand returns the "set" command with its subcommands.
func NewSetCommand(globalOpts *GlobalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set SUBCOMMAND",
		Short: "Update specific fields on a resource",
		Long: `Configure OpenKruise Agents resources.

These commands help you update container images on SandboxSets.`,
	}
	cmd.AddCommand(newSetImageCommand(globalOpts))
	return cmd
}

func newSetImageCommand(globalOpts *GlobalOptions) *cobra.Command {
	o := &setImageOptions{global: globalOpts}

	cmd := &cobra.Command{
		Use:   "image sandboxset NAME CONTAINER=IMAGE [CONTAINER=IMAGE ...]",
		Short: "Update container images of a SandboxSet",
		Long: `Update one or more container images in a SandboxSet's inline template.
This command only works with SandboxSets that use an inline template (spec.template).
For SandboxSets using a TemplateRef, modify the referenced SandboxTemplate directly.`,
		Example: `  # Update the app container image
  okactl set image sandboxset my-pool app=nginx:1.27

  # Update multiple container images at once
  okactl set image sandboxset my-pool app=nginx:1.27 sidecar=envoyproxy/envoy:v1.30

  # Update in a specific namespace
  okactl -n agent-system set image sandboxset my-pool app=nginx:1.27`,
		Args: cobra.MinimumNArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "sandboxset", "sbs":
				return o.run(args[1], args[2:])
			default:
				return fmt.Errorf("unsupported resource type %q, only 'sandboxset' (sbs) is supported", args[0])
			}
		},
	}
	return cmd
}

// parseContainerImages parses "container=image" pairs and returns a map.
func parseContainerImages(args []string) (map[string]string, error) {
	images := make(map[string]string, len(args))
	for _, arg := range args {
		parts := strings.SplitN(arg, "=", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("invalid container=image argument %q, expected format CONTAINER=IMAGE", arg)
		}
		images[parts[0]] = parts[1]
	}
	return images, nil
}

// updateContainerImages applies image updates to a container slice, returning the names that were updated.
func updateContainerImages(containers []corev1.Container, images map[string]string) []string {
	var updated []string
	for i := range containers {
		if newImage, ok := images[containers[i].Name]; ok {
			containers[i].Image = newImage
			updated = append(updated, containers[i].Name)
		}
	}
	return updated
}

func (o *setImageOptions) run(name string, imageArgs []string) error {
	client, err := o.global.AgentsClient()
	if err != nil {
		return err
	}
	return runSetImageWithClient(client, o, name, imageArgs)
}

func runSetImageWithClient(client apiv1alpha1.ApiV1alpha1Interface, o *setImageOptions, name string, imageArgs []string) error {
	images, err := parseContainerImages(imageArgs)
	if err != nil {
		return err
	}

	ctx := context.TODO()
	sbs, err := client.SandboxSets(o.global.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get sandboxset %q: %w", name, err)
	}

	if sbs.Spec.Template == nil {
		return fmt.Errorf("sandboxset %q uses a TemplateRef; modify the referenced SandboxTemplate directly instead", name)
	}

	updated := updateContainerImages(sbs.Spec.Template.Spec.Containers, images)
	updated = append(updated, updateContainerImages(sbs.Spec.Template.Spec.InitContainers, images)...)

	found := make(map[string]bool, len(updated))
	for _, u := range updated {
		found[u] = true
	}
	for container := range images {
		if !found[container] {
			return fmt.Errorf("container %q not found in sandboxset %q", container, name)
		}
	}

	_, err = client.SandboxSets(o.global.Namespace).Update(ctx, sbs, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update sandboxset %q: %w", name, err)
	}

	fmt.Printf("sandboxset.agents.kruise.io/%s image updated (%s)\n", name, strings.Join(updated, ", "))
	return nil
}
