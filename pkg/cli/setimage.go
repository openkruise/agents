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
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	apiv1alpha1 "github.com/openkruise/agents/client/clientset/versioned/typed/api/v1alpha1"
)

type setImageOptions struct {
	global  *GlobalOptions
	wait    bool
	timeout time.Duration
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
	opts := &setImageOptions{global: globalOpts}

	cmd := &cobra.Command{
		Use:   "image sandboxset NAME CONTAINER=IMAGE [CONTAINER=IMAGE ...]",
		Short: "Update container images of a SandboxSet",
		Long: `Update one or more container images in a SandboxSet's inline template.
This command only works with SandboxSets that use an inline template (spec.template).
For SandboxSets using a TemplateRef, modify the referenced SandboxTemplate directly.

Use --wait to poll until the rolling update is complete. When the update appears
stalled, the command automatically diagnoses potential issues (e.g., ImagePullBackOff,
insufficient resources) by inspecting sandbox and pod status.`,
		Example: `  # Update the gateway container image
  okactl set image sbs openclaw-sbs gateway=mirrors-ssl.aliyuncs.com/ghcr.io/openclaw/openclaw:2026.4.24

  # Update multiple container images at once
  okactl set image sbs my-pool app=myregistry.com/app:v2 sidecar=myregistry.com/sidecar:v2

  # Update in a specific namespace
  okactl -n agent-system set image sbs my-pool app=myregistry.com/app:v2

  # Update and wait for the rollout to complete (diagnoses issues if stalled)
  okactl set image sbs my-pool app=myregistry.com/app:v2 --wait

  # Check update progress after set image
  okactl status sbs my-pool`,
		Args: cobra.MinimumNArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "sandboxset", "sbs":
				return opts.run(args[1], args[2:])
			default:
				return fmt.Errorf("unsupported resource type %q, only 'sandboxset' (sbs) is supported", args[0])
			}
		},
	}
	cmd.Flags().BoolVarP(&opts.wait, "wait", "w", false, "Wait for the rollout to complete")
	cmd.Flags().DurationVarP(&opts.timeout, "timeout", "", 5*time.Minute, "Timeout for --wait (e.g., 5m, 10m; 0 disables timeout)")
	return cmd
}

// parseImageArgs parses "container=image" pairs and returns a map.
// It is shared by set image and create suo commands.
func parseImageArgs(args []string) (map[string]string, error) {
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

// validateSandboxSetContainers checks that every container name in images
// exists in the SandboxSet's inline template (containers or init containers).
// Returns an error naming the first missing container.
func validateSandboxSetContainers(sbs *agentsv1alpha1.SandboxSet, images map[string]string, name string) error {
	found := make(map[string]bool, len(images))
	for _, c := range sbs.Spec.Template.Spec.Containers {
		if _, ok := images[c.Name]; ok {
			found[c.Name] = true
		}
	}
	for _, c := range sbs.Spec.Template.Spec.InitContainers {
		if _, ok := images[c.Name]; ok {
			found[c.Name] = true
		}
	}
	for container := range images {
		if !found[container] {
			return fmt.Errorf("container %q not found in sandboxset %q", container, name)
		}
	}
	return nil
}

func (opts *setImageOptions) run(name string, imageArgs []string) error {
	client, err := opts.global.AgentsClient()
	if err != nil {
		return err
	}
	return runSetImageWithClient(client, opts, name, imageArgs, opts.wait)
}

func runSetImageWithClient(client apiv1alpha1.ApiV1alpha1Interface, opts *setImageOptions, name string, imageArgs []string, wait bool) error {
	images, err := parseImageArgs(imageArgs)
	if err != nil {
		return err
	}

	ctx := context.TODO()

	// Pre-validate before entering the retry loop. Validation errors are
	// deterministic (not transient conflicts) and must be returned directly
	// without wrapping as "failed to update" — the update was never attempted.
	sbs, err := client.SandboxSets(opts.global.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get sandboxset %q: %w", name, err)
	}

	if sbs.Spec.Template == nil {
		return fmt.Errorf("sandboxset %q uses a TemplateRef; modify the referenced SandboxTemplate directly instead", name)
	}

	if err := validateSandboxSetContainers(sbs, images, name); err != nil {
		return err
	}

	// Retry only the Get-Modify-Update cycle on conflict.
	var updated []string
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		sbs, getErr := client.SandboxSets(opts.global.Namespace).Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			return getErr
		}

		updated = updateContainerImages(sbs.Spec.Template.Spec.Containers, images)
		updated = append(updated, updateContainerImages(sbs.Spec.Template.Spec.InitContainers, images)...)

		_, updateErr := client.SandboxSets(opts.global.Namespace).Update(ctx, sbs, metav1.UpdateOptions{})
		return updateErr
	})
	if err != nil {
		return fmt.Errorf("failed to update sandboxset %q: %w", name, err)
	}

	fmt.Printf("sandboxset.agents.kruise.io/%s image updated (%s)\n", name, strings.Join(updated, ", "))

	if wait {
		if opts.timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, opts.timeout)
			defer cancel()
		}
		return waitForSandboxSetUpdate(client, ctx, opts.global.Namespace, name, opts.global)
	}
	return nil
}

func runSetImageStatusWithClient(client apiv1alpha1.ApiV1alpha1Interface, globalOpts *GlobalOptions, name string) error {
	ctx := context.TODO()
	ns := globalOpts.Namespace

	sbs, err := client.SandboxSets(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get sandboxset %q: %w", name, err)
	}

	printSandboxSetStatus(sbs)
	reported := make(map[string]bool)
	kubeClient, err := globalOpts.KubeClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to create kube client for diagnosis: %v\n", err)
	}
	diagnoseSandboxSetUpdate(client, kubeClient, ns, sbs, reported)
	return nil
}

func waitForSandboxSetUpdate(client apiv1alpha1.ApiV1alpha1Interface, ctx context.Context, ns, name string, globalOpts *GlobalOptions) error {
	const pollInterval = 3 * time.Second
	var lastUpdated int32 = -1
	var stallCount int
	reported := make(map[string]bool)

	// Create kubeClient once for diagnosis instead of on every poll cycle
	kubeClient, err := globalOpts.KubeClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to create kube client for diagnosis: %v\n", err)
	}

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
			diagnoseSandboxSetUpdate(client, kubeClient, ns, sbs, reported)
			stallCount = 0
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for sandboxset %q update: %w", name, ctx.Err())
		case <-time.After(pollInterval):
		}
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
// It uses the provided clients to inspect sandbox and pod status, avoiding repeated
// kubeconfig reads and TLS connection setup on every call.
func diagnoseSandboxSetUpdate(agentsClient apiv1alpha1.ApiV1alpha1Interface, kubeClient kubernetes.Interface, ns string, sbs *agentsv1alpha1.SandboxSet, reported map[string]bool) {
	if isSandboxSetUpdateComplete(sbs) {
		return
	}

	if agentsClient == nil {
		return
	}

	sbxList, err := agentsClient.Sandboxes(ns).List(context.TODO(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", agentsv1alpha1.LabelSandboxTemplate, sbs.Name),
	})
	if err != nil {
		return
	}

	for _, sbx := range sbxList.Items {
		// Skip if already reported
		if reported[sbx.Name] {
			continue
		}

		// Check if sandbox is in a problem state
		if sbx.Status.Phase == agentsv1alpha1.SandboxPending || sbx.Status.Phase == agentsv1alpha1.SandboxFailed {
			msg := sbx.Status.Message
			if msg == "" && kubeClient != nil {
				// Try to get pod status for more details
				pod, err := kubeClient.CoreV1().Pods(ns).Get(context.TODO(), sbx.Name, metav1.GetOptions{})
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
			reported[sbx.Name] = true
		}
	}
}
