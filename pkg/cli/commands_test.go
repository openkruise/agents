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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/client-go/rest"
)

func TestNewCreateCommand(t *testing.T) {
	globalOpts := NewGlobalOptions()
	cmd := NewCreateCommand(globalOpts)

	assert.Equal(t, "create SUBCOMMAND", cmd.Use)
	assert.NotEmpty(t, cmd.Short)
	assert.NotEmpty(t, cmd.Long)
	assert.True(t, cmd.HasSubCommands())

	// Verify "suo" subcommand exists
	suoCmd, _, err := cmd.Find([]string{"suo"})
	assert.NoError(t, err)
	assert.NotNil(t, suoCmd)
	assert.Contains(t, suoCmd.Use, "suo")

	// Verify suo has the required flags
	selectorFlag := suoCmd.Flags().Lookup("selector")
	assert.NotNil(t, selectorFlag)
	assert.Equal(t, "l", selectorFlag.Shorthand)
}

func TestNewRestartCommand(t *testing.T) {
	globalOpts := NewGlobalOptions()
	cmd := NewRestartCommand(globalOpts)

	assert.Equal(t, "restart", cmd.Use)
	assert.NotEmpty(t, cmd.Short)
	assert.True(t, cmd.HasSubCommands())

	// Verify "sandbox" subcommand exists
	sbxCmd, _, err := cmd.Find([]string{"sandbox"})
	assert.NoError(t, err)
	assert.NotNil(t, sbxCmd)
	assert.Contains(t, sbxCmd.Use, "sandbox")

	// Verify sandbox has the container flag
	containerFlag := sbxCmd.Flags().Lookup("container")
	assert.NotNil(t, containerFlag)
	assert.Equal(t, "c", containerFlag.Shorthand)
}

func TestNewScaleCommand(t *testing.T) {
	globalOpts := NewGlobalOptions()
	cmd := NewScaleCommand(globalOpts)

	assert.Equal(t, "scale", cmd.Use)
	assert.NotEmpty(t, cmd.Short)
	assert.True(t, cmd.HasSubCommands())

	// Verify "sandboxset" subcommand exists
	sbsCmd, _, err := cmd.Find([]string{"sandboxset"})
	assert.NoError(t, err)
	assert.NotNil(t, sbsCmd)
	assert.Contains(t, sbsCmd.Use, "sandboxset")

	// Verify sandboxset has the replicas flag
	replicasFlag := sbsCmd.Flags().Lookup("replicas")
	assert.NotNil(t, replicasFlag)
}

func TestNewSetImageStatusCommand(t *testing.T) {
	globalOpts := NewGlobalOptions()
	cmd := NewSetCommand(globalOpts)

	// Find the image subcommand
	imageCmd, _, err := cmd.Find([]string{"image"})
	assert.NoError(t, err)
	assert.NotNil(t, imageCmd)

	// Find the status subcommand under image
	statusCmd, _, err := imageCmd.Find([]string{"status"})
	assert.NoError(t, err)
	assert.NotNil(t, statusCmd)
	assert.Contains(t, statusCmd.Use, "status")

	// Verify the --wait flag
	waitFlag := statusCmd.Flags().Lookup("wait")
	assert.NotNil(t, waitFlag)
	assert.Equal(t, "w", waitFlag.Shorthand)
}

func TestCreateSuoCommandRequiresSelector(t *testing.T) {
	globalOpts := NewGlobalOptions()
	cmd := NewCreateCommand(globalOpts)

	// Execute without -l flag should fail
	cmd.SetArgs([]string{"suo", "main=nginx:2.0"})
	err := cmd.Execute()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "selector")
}

func TestScaleCommandRequiresReplicas(t *testing.T) {
	globalOpts := NewGlobalOptions()
	cmd := NewScaleCommand(globalOpts)

	// Execute without --replicas flag should fail
	cmd.SetArgs([]string{"sandboxset", "my-pool"})
	err := cmd.Execute()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "replicas")
}

func TestCreateSuoRunFailsWithInvalidConfig(t *testing.T) {
	origFn := inClusterConfigFn
	defer func() { inClusterConfigFn = origFn }()
	inClusterConfigFn = func() (*rest.Config, error) {
		return nil, fmt.Errorf("not in cluster")
	}

	o := &createSuoOptions{
		global: &GlobalOptions{
			KubeConfig: "/nonexistent/config",
			Namespace:  "default",
		},
		selector: "app=my-app",
	}

	err := o.run([]string{"main=nginx:2.0"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to build kubeconfig")
}

func TestSetImageRunFailsWithInvalidConfig(t *testing.T) {
	origFn := inClusterConfigFn
	defer func() { inClusterConfigFn = origFn }()
	inClusterConfigFn = func() (*rest.Config, error) {
		return nil, fmt.Errorf("not in cluster")
	}

	o := &setImageOptions{
		global: &GlobalOptions{
			KubeConfig: "/nonexistent/config",
			Namespace:  "default",
		},
	}

	err := o.run("test-sbs", []string{"main=nginx:2.0"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to build kubeconfig")
}

func TestRestartRunFailsWithInvalidConfig(t *testing.T) {
	origFn := inClusterConfigFn
	defer func() { inClusterConfigFn = origFn }()
	inClusterConfigFn = func() (*rest.Config, error) {
		return nil, fmt.Errorf("not in cluster")
	}

	o := &restartOptions{
		global: &GlobalOptions{
			KubeConfig: "/nonexistent/config",
			Namespace:  "default",
		},
		containers: []string{"main"},
	}

	err := o.run("test-sbx")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to build kubeconfig")
}

func TestScaleRunFailsWithInvalidConfig(t *testing.T) {
	origFn := inClusterConfigFn
	defer func() { inClusterConfigFn = origFn }()
	inClusterConfigFn = func() (*rest.Config, error) {
		return nil, fmt.Errorf("not in cluster")
	}

	o := &scaleOptions{
		global: &GlobalOptions{
			KubeConfig: "/nonexistent/config",
			Namespace:  "default",
		},
		replicas: 5,
	}

	err := o.run("test-sbs")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to build kubeconfig")
}

func TestCreateSuoRunEmptySelectorFails(t *testing.T) {
	o := &createSuoOptions{
		global: &GlobalOptions{
			Namespace: "default",
		},
		selector: "",
	}

	err := o.run([]string{"main=nginx:2.0"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "--selector (-l) is required")
}
