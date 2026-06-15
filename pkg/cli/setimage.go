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
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
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
		Example: `  # Update the gateway container image
  okactl set image sbs openclaw-sbs gateway=mirrors-ssl.aliyuncs.com/ghcr.io/openclaw/openclaw:2026.4.24

  # Update multiple container images at once
  okactl set image sbs my-pool app=myregistry.com/app:v2 sidecar=myregistry.com/sidecar:v2

  # Update in a specific namespace
  okactl -n agent-system set image sbs my-pool app=myregistry.com/app:v2

  # Check update progress after set image
  okactl set image status my-pool

  # Wait for update to complete (diagnoses issues if stalled)
  okactl set image status my-pool --wait`,
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
	cmd.AddCommand(newSetImageStatusCommand(globalOpts))
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

// newSetImageStatusCommand returns the "set image status" subcommand.
func newSetImageStatusCommand(globalOpts *GlobalOptions) *cobra.Command {
	var wait bool

	cmd := &cobra.Command{
		Use:   "status NAME",
		Short: "Show the update progress of a SandboxSet",
		Long: `Show the rolling update progress of a SandboxSet after "set image".

Displays how many replicas have been updated and how many are available.
If the update is stalled, automatically diagnoses the issue by checking
sandbox and pod status (e.g., ImagePullBackOff, insufficient resources).
With --wait, polls every 3 seconds until the update is fully complete.`,
		Example: `  # Show current update progress
  okactl set image status openclaw-sbs

  # Wait for the update to complete (with automatic diagnostics if stalled)
  okactl set image status openclaw-sbs --wait`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := globalOpts.AgentsClient()
			if err != nil {
				return err
			}
			return runSetImageStatusWithClient(client, globalOpts, args[0], wait)
		},
	}
	cmd.Flags().BoolVarP(&wait, "wait", "w", false, "Wait for the update to complete")
	return cmd
}

func runSetImageStatusWithClient(client apiv1alpha1.ApiV1alpha1Interface, globalOpts *GlobalOptions, name string, wait bool) error {
	ctx := context.TODO()
	ns := globalOpts.Namespace

	if wait {
		return waitForSandboxSetUpdate(client, ctx, ns, name, globalOpts)
	}

	sbs, err := client.SandboxSets(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get sandboxset %q: %w", name, err)
	}

	printSandboxSetStatus(sbs)
	var reported map[string]bool
	diagnoseSandboxSetUpdate(globalOpts, sbs, &reported)
	return nil
}

func waitForSandboxSetUpdate(client apiv1alpha1.ApiV1alpha1Interface, ctx context.Context, ns, name string, globalOpts *GlobalOptions) error {
	const pollInterval = 3 * time.Second
	var lastUpdated int32 = -1
	var stallCount int
	var reported map[string]bool

	for {
		sbs, err := client.SandboxSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get sandboxset %q: %w", name, err)
		}

		status := sbs.Status
		if status.UpdatedReplicas != lastUpdated {
			printSandboxSetStatus(sbs)
			lastUpdated = status.UpdatedReplicas
			stallCount = 0
		} else {
			stallCount++
		}

		if isSandboxSetUpdateComplete(sbs) {
			fmt.Printf("Update completed (%d/%d updated, %d/%d available)\n",
				status.UpdatedReplicas, sbs.Spec.Replicas,
				status.UpdatedAvailableReplicas, sbs.Spec.Replicas)
			return nil
		}

		// After stalling for ~10s (3 polls), diagnose the issue
		if stallCount >= 3 {
			diagnoseSandboxSetUpdate(globalOpts, sbs, &reported)
			stallCount = 0
		}

		time.Sleep(pollInterval)
	}
}

// isSandboxSetUpdateComplete checks if all replicas are updated and available.
func isSandboxSetUpdateComplete(sbs *agentsv1alpha1.SandboxSet) bool {
	status := sbs.Status
	return status.UpdatedReplicas >= sbs.Spec.Replicas &&
		status.AvailableReplicas >= sbs.Spec.Replicas
}

// printSandboxSetStatus prints a one-line status of the SandboxSet update progress.
func printSandboxSetStatus(sbs *agentsv1alpha1.SandboxSet) {
	status := sbs.Status
	phase := "Updating"
	if isSandboxSetUpdateComplete(sbs) {
		phase = "Complete"
	}

	fmt.Printf("%-30s %-10s %d/%d updated  %d/%d available\n",
		sbs.Name, phase,
		status.UpdatedReplicas, sbs.Spec.Replicas,
		status.UpdatedAvailableReplicas, sbs.Spec.Replicas)
}

// diagnoseSandboxSetUpdate checks sandboxes belonging to a SandboxSet and reports any issues.
// It builds a kubernetes client to inspect pod status when sandbox messages are empty.
func diagnoseSandboxSetUpdate(globalOpts *GlobalOptions, sbs *agentsv1alpha1.SandboxSet, reported *map[string]bool) {
	if isSandboxSetUpdateComplete(sbs) {
		return
	}

	client, err := globalOpts.AgentsClient()
	if err != nil {
		return
	}

	kubeClient, err := globalOpts.KubeClient()
	if err != nil {
		return
	}

	sbxList, err := client.Sandboxes(globalOpts.Namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("agents.kruise.io/sandbox-template=%s", sbs.Name),
	})
	if err != nil {
		return
	}

	for _, sbx := range sbxList.Items {
		// Skip if already reported
		if *reported != nil && (*reported)[sbx.Name] {
			continue
		}

		// Check if sandbox is in a problem state
		if sbx.Status.Phase == agentsv1alpha1.SandboxPending || sbx.Status.Phase == agentsv1alpha1.SandboxFailed {
			msg := sbx.Status.Message
			if msg == "" {
				// Try to get pod status for more details
				pod, err := kubeClient.CoreV1().Pods(globalOpts.Namespace).Get(context.TODO(), sbx.Name, metav1.GetOptions{})
				if err == nil {
					for _, cond := range pod.Status.Conditions {
						if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse {
							msg = cond.Message
							break
						}
					}
					// Check container statuses for ImagePullBackOff, etc.
					if msg == "" {
						for _, cs := range pod.Status.ContainerStatuses {
							if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
								msg = fmt.Sprintf("%s: %s", cs.Name, cs.State.Waiting.Reason)
								break
							}
						}
					}
				}
			}

			if msg == "" {
				msg = "unknown reason"
			}

			fmt.Printf("  Sandbox %s is %s: %s\n", sbx.Name, sbx.Status.Phase, msg)
			if *reported == nil {
				*reported = make(map[string]bool)
			}
			(*reported)[sbx.Name] = true
		}
	}
}
