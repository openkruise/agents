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

	kruiseappsv1alpha1 "github.com/openkruise/kruise-api/apps/v1alpha1"
	kruiseversioned "github.com/openkruise/kruise-api/client/clientset/versioned"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	apiv1alpha1 "github.com/openkruise/agents/client/clientset/versioned/typed/api/v1alpha1"
)

type restartOptions struct {
	global        *GlobalOptions
	containers    []string
	allContainers bool
	failurePolicy string
}

// NewRestartCommand returns the "restart" command with its subcommands.
func NewRestartCommand(globalOpts *GlobalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "restart",
		Short: "Restart containers in a Sandbox",
		Long: `Restart one or more containers in a running Sandbox.

Uses OpenKruise ContainerRecreateRequest (CRR) to perform in-place container
restarts without recreating the entire Pod.`,
	}
	cmd.AddCommand(newRestartSandboxCommand(globalOpts))
	return cmd
}

func newRestartSandboxCommand(globalOpts *GlobalOptions) *cobra.Command {
	opts := &restartOptions{global: globalOpts}

	cmd := &cobra.Command{
		Use:     "sandbox NAME [-c CONTAINER ...] [--all] [--failure-policy=Fail|Ignore]",
		Aliases: []string{"sbx"},
		Short:   "Restart containers in a Sandbox by creating an OpenKruise ContainerRecreateRequest",
		Long: `Restart one or more containers in a running Sandbox.
If -c is specified, only the listed containers are restarted.
If --all is specified, all user containers in the Sandbox are restarted.
At least one of -c or --all must be provided; running without either will
print available container names and exit with an error.
This command creates an OpenKruise ContainerRecreateRequest (CRR) targeting the Sandbox's Pod.

The --failure-policy flag controls how failures are handled:
  Fail    - Stop recreating remaining containers if one fails (default).
  Ignore  - Continue recreating remaining containers even if one fails.`,
		Example: `  # Restart a specific container
  okactl restart sandbox my-sbx -c app

  # Restart multiple containers
  okactl restart sandbox my-sbx -c app -c sidecar

  # Restart all containers (explicit)
  okactl restart sandbox my-sbx --all

  # Restart all containers, continuing even if one fails
  okactl restart sandbox my-sbx --all --failure-policy=Ignore

  # Restart in a specific namespace
  okactl -n agent-system restart sandbox my-sbx -c app`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return opts.run(args[0])
		},
	}
	cmd.Flags().StringArrayVarP(&opts.containers, "container", "c", nil, "Container name to restart (can be specified multiple times)")
	cmd.Flags().BoolVarP(&opts.allContainers, "all", "", false, "Restart all containers in the sandbox")
	cmd.Flags().StringVarP(&opts.failurePolicy, "failure-policy", "", "Fail", "Failure policy: Fail (stop on error) or Ignore (continue on error)")
	return cmd
}

func (opts *restartOptions) run(sandboxName string) error {
	agentsClient, err := opts.global.AgentsClient()
	if err != nil {
		return err
	}
	kruiseClient, err := opts.global.KruiseClient()
	if err != nil {
		return err
	}
	return runRestartWithClients(agentsClient, kruiseClient, opts, sandboxName)
}

func runRestartWithClients(agentsClient apiv1alpha1.ApiV1alpha1Interface, kruiseClient kruiseversioned.Interface, opts *restartOptions, sandboxName string) error {
	ctx := context.TODO()
	ns := opts.global.Namespace

	sbx, err := agentsClient.Sandboxes(ns).Get(ctx, sandboxName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get sandbox %q: %w", sandboxName, err)
	}

	// Check if the sandbox is running before creating a CRR.
	// CRR operates on container processes within a Pod; a non-Running sandbox
	// may not have a scheduled Pod or its Pod may have already terminated.
	if sbx.Status.Phase != agentsv1alpha1.SandboxRunning {
		return fmt.Errorf("sandbox %q is not running (current phase: %s)", sandboxName, sbx.Status.Phase)
	}

	// Validate failure policy
	failurePolicy := kruiseappsv1alpha1.ContainerRecreateRequestFailurePolicyType(opts.failurePolicy)
	if failurePolicy == "" {
		failurePolicy = kruiseappsv1alpha1.ContainerRecreateRequestFailurePolicyFail
	}
	if failurePolicy != kruiseappsv1alpha1.ContainerRecreateRequestFailurePolicyFail && failurePolicy != kruiseappsv1alpha1.ContainerRecreateRequestFailurePolicyIgnore {
		return fmt.Errorf("invalid failure-policy %q: must be Fail or Ignore", failurePolicy)
	}

	containers := opts.containers
	if len(containers) == 0 && !opts.allContainers {
		available, _, ferr := fetchContainerNames(ctx, agentsClient, sbx)
		if ferr != nil {
			return ferr
		}
		return fmt.Errorf("no containers specified: use -c <name> or --all to restart. Available containers: %v", available)
	}
	if len(containers) > 0 && opts.allContainers {
		return fmt.Errorf("--all cannot be used together with -c")
	}
	if len(containers) == 0 {
		containers, err = extractContainerNames(ctx, agentsClient, sbx)
		if err != nil {
			return err
		}
		if len(containers) == 0 {
			return fmt.Errorf("sandbox %q has no containers to restart", sandboxName)
		}
	} else {
		if err := validateContainerNames(ctx, agentsClient, sbx, containers); err != nil {
			return err
		}
	}

	// Build typed CRR spec.containers list
	containerTargets := make([]kruiseappsv1alpha1.ContainerRecreateRequestContainer, 0, len(containers))
	for _, c := range containers {
		containerTargets = append(containerTargets, kruiseappsv1alpha1.ContainerRecreateRequestContainer{
			Name: c,
		})
	}

	// Use a deterministic name so that repeated restarts of the same sandbox
	// reuse the same CRR instead of accumulating new ones.
	crrName := sandboxName + "-restart"

	// Check for an existing CRR with the same name. If it is still active,
	// refuse to create a duplicate. If it has completed, delete it so we can
	// create a fresh one.
	existing, getErr := kruiseClient.AppsV1alpha1().ContainerRecreateRequests(ns).Get(ctx, crrName, metav1.GetOptions{})
	if getErr != nil {
		if !apierrors.IsNotFound(getErr) {
			return fmt.Errorf("failed to check existing ContainerRecreateRequest: %w", getErr)
		}
	} else if isCRRActive(existing) {
		return fmt.Errorf("an active ContainerRecreateRequest %q already exists for sandbox %q (phase: %s); wait for it to complete or delete it manually",
			crrName, sandboxName, existing.Status.Phase)
	} else {
		// Previous CRR has completed — delete it to make room for the new one.
		if delErr := kruiseClient.AppsV1alpha1().ContainerRecreateRequests(ns).Delete(ctx, crrName, metav1.DeleteOptions{}); delErr != nil {
			if !apierrors.IsNotFound(delErr) {
				return fmt.Errorf("failed to delete completed ContainerRecreateRequest %q: %w", crrName, delErr)
			}
		}
	}

	// CRR targets the Pod which has the same name as the Sandbox
	crr := &kruiseappsv1alpha1.ContainerRecreateRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      crrName,
			Namespace: ns,
		},
		Spec: kruiseappsv1alpha1.ContainerRecreateRequestSpec{
			PodName:    sandboxName,
			Containers: containerTargets,
			Strategy: &kruiseappsv1alpha1.ContainerRecreateRequestStrategy{
				FailurePolicy: failurePolicy,
			},
		},
	}

	created, err := kruiseClient.AppsV1alpha1().ContainerRecreateRequests(ns).Create(ctx, crr, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("an active ContainerRecreateRequest %q already exists for sandbox %q; wait for it to complete or delete it manually",
				crrName, sandboxName)
		}
		return fmt.Errorf("failed to create ContainerRecreateRequest: %w", err)
	}

	fmt.Printf("containerrecreaterequests.apps.kruise.io/%s created (pod: %s, containers: %v)\n",
		created.GetName(), sandboxName, containers)
	return nil
}

// isCRRActive returns true if the ContainerRecreateRequest has not yet reached
// a terminal state. A CRR with an empty phase is considered active because it
// has just been created and the controller has not yet populated the status.
func isCRRActive(crr *kruiseappsv1alpha1.ContainerRecreateRequest) bool {
	phase := crr.Status.Phase
	return phase == "" ||
		phase == kruiseappsv1alpha1.ContainerRecreateRequestPending ||
		phase == kruiseappsv1alpha1.ContainerRecreateRequestRecreating
}

// fetchContainerNames retrieves container and init-container names from the sandbox's
// inline Template, or from the referenced SandboxTemplate when Template is nil.
// This allows both extractContainerNames and validateContainerNames to work with
// sandboxes that use TemplateRef instead of an inline Template.
func fetchContainerNames(ctx context.Context, agentsClient apiv1alpha1.ApiV1alpha1Interface, sbx *agentsv1alpha1.Sandbox) (containers, initContainers []string, err error) {
	if sbx.Spec.Template != nil {
		for _, c := range sbx.Spec.Template.Spec.Containers {
			containers = append(containers, c.Name)
		}
		for _, c := range sbx.Spec.Template.Spec.InitContainers {
			initContainers = append(initContainers, c.Name)
		}
		return containers, initContainers, nil
	}

	if sbx.Spec.TemplateRef == nil {
		return nil, nil, fmt.Errorf("sandbox %q has no template or templateRef", sbx.Name)
	}

	sbt, err := agentsClient.SandboxTemplates(sbx.Namespace).Get(ctx, sbx.Spec.TemplateRef.Name, metav1.GetOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get SandboxTemplate %q referenced by sandbox %q: %w", sbx.Spec.TemplateRef.Name, sbx.Name, err)
	}
	if sbt.Spec.Template == nil {
		return nil, nil, fmt.Errorf("SandboxTemplate %q has no template defined", sbx.Spec.TemplateRef.Name)
	}
	for _, c := range sbt.Spec.Template.Spec.Containers {
		containers = append(containers, c.Name)
	}
	for _, c := range sbt.Spec.Template.Spec.InitContainers {
		initContainers = append(initContainers, c.Name)
	}
	return containers, initContainers, nil
}

func extractContainerNames(ctx context.Context, agentsClient apiv1alpha1.ApiV1alpha1Interface, sbx *agentsv1alpha1.Sandbox) ([]string, error) {
	containers, _, err := fetchContainerNames(ctx, agentsClient, sbx)
	if err != nil {
		return nil, err
	}
	return containers, nil
}

func validateContainerNames(ctx context.Context, agentsClient apiv1alpha1.ApiV1alpha1Interface, sbx *agentsv1alpha1.Sandbox, requested []string) error {
	containers, initContainers, err := fetchContainerNames(ctx, agentsClient, sbx)
	if err != nil {
		return err
	}

	known := make(map[string]bool)
	for _, name := range containers {
		known[name] = true
	}
	for _, name := range initContainers {
		known[name] = true
	}

	for _, name := range requested {
		if !known[name] {
			return fmt.Errorf("container %q not found in sandbox %q", name, sbx.Name)
		}
	}
	return nil
}
