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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	apiv1alpha1 "github.com/openkruise/agents/client/clientset/versioned/typed/api/v1alpha1"
)

// OpenKruise ContainerRecreateRequest GVR
var containerRecreateRequestGVR = schema.GroupVersionResource{
	Group:    "apps.kruise.io",
	Version:  "v1alpha1",
	Resource: "containerrecreaterequests",
}

type restartOptions struct {
	global     *GlobalOptions
	containers []string
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
	o := &restartOptions{global: globalOpts}

	cmd := &cobra.Command{
		Use:   "sandbox NAME [-c CONTAINER ...]",
		Short: "Restart containers in a Sandbox by creating an OpenKruise ContainerRecreateRequest",
		Long: `Restart one or more containers in a running Sandbox.
If no -c flags are specified, all user containers in the Sandbox will be restarted.
This command creates an OpenKruise ContainerRecreateRequest (CRR) targeting the Sandbox's Pod.`,
		Example: `  # Restart all user containers in a Sandbox
  okactl restart sandbox my-sbx

  # Restart a specific container
  okactl restart sandbox my-sbx -c app

  # Restart multiple containers
  okactl restart sandbox my-sbx -c app -c sidecar

  # Restart in a specific namespace
  okactl -n agent-system restart sandbox my-sbx`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.run(args[0])
		},
	}
	cmd.Flags().StringArrayVarP(&o.containers, "container", "c", nil, "Container name to restart (can be specified multiple times)")
	return cmd
}

func (o *restartOptions) run(sandboxName string) error {
	agentsClient, err := o.global.AgentsClient()
	if err != nil {
		return err
	}
	dynClient, err := o.global.DynamicClient()
	if err != nil {
		return err
	}
	return runRestartWithClients(agentsClient, dynClient, o, sandboxName)
}

func runRestartWithClients(agentsClient apiv1alpha1.ApiV1alpha1Interface, dynClient dynamic.Interface, o *restartOptions, sandboxName string) error {
	ctx := context.TODO()
	ns := o.global.Namespace

	sbx, err := agentsClient.Sandboxes(ns).Get(ctx, sandboxName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get sandbox %q: %w", sandboxName, err)
	}

	containers := o.containers
	if len(containers) == 0 {
		containers, err = extractContainerNames(sbx)
		if err != nil {
			return err
		}
		if len(containers) == 0 {
			return fmt.Errorf("sandbox %q has no containers to restart", sandboxName)
		}
	} else {
		if err := validateContainerNames(sbx, containers); err != nil {
			return err
		}
	}

	// Build OpenKruise CRR spec.containers list
	containerTargets := make([]interface{}, 0, len(containers))
	for _, c := range containers {
		containerTargets = append(containerTargets, map[string]interface{}{
			"name": c,
		})
	}

	// CRR targets the Pod which has the same name as the Sandbox
	crr := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apps.kruise.io/v1alpha1",
			"kind":       "ContainerRecreateRequest",
			"metadata": map[string]interface{}{
				"generateName": sandboxName + "-restart-",
				"namespace":    ns,
			},
			"spec": map[string]interface{}{
				"podName":    sandboxName,
				"containers": containerTargets,
				"strategy": map[string]interface{}{
					"failurePolicy": "Fail",
				},
			},
		},
	}

	created, err := dynClient.Resource(containerRecreateRequestGVR).Namespace(ns).Create(ctx, crr, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create ContainerRecreateRequest: %w", err)
	}

	fmt.Printf("containerrecreaterequests.apps.kruise.io/%s created (pod: %s, containers: %v)\n",
		created.GetName(), sandboxName, containers)
	return nil
}

func extractContainerNames(sbx *agentsv1alpha1.Sandbox) ([]string, error) {
	if sbx.Spec.Template == nil {
		return nil, fmt.Errorf("sandbox %q uses a TemplateRef; cannot auto-detect containers, specify -c explicitly", sbx.Name)
	}
	var names []string
	for _, c := range sbx.Spec.Template.Spec.Containers {
		names = append(names, c.Name)
	}
	return names, nil
}

func validateContainerNames(sbx *agentsv1alpha1.Sandbox, requested []string) error {
	known := make(map[string]bool)
	if sbx.Spec.Template != nil {
		for _, c := range sbx.Spec.Template.Spec.Containers {
			known[c.Name] = true
		}
		for _, c := range sbx.Spec.Template.Spec.InitContainers {
			known[c.Name] = true
		}
	}

	if sbx.Spec.Template == nil {
		return nil
	}

	for _, name := range requested {
		if !known[name] {
			return fmt.Errorf("container %q not found in sandbox %q", name, sbx.Name)
		}
	}
	return nil
}
