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

	// Verify "sbs" alias resolves to the same subcommand
	sbsAliasCmd, _, err := cmd.Find([]string{"sbs"})
	assert.NoError(t, err)
	assert.Equal(t, sbsCmd, sbsAliasCmd)

	// Verify sandboxset has the replicas flag
	replicasFlag := sbsCmd.Flags().Lookup("replicas")
	assert.NotNil(t, replicasFlag)
}

func TestNewStatusCommand(t *testing.T) {
	globalOpts := NewGlobalOptions()
	cmd := NewStatusCommand(globalOpts)

	assert.Equal(t, "status", cmd.Use)
	assert.NotEmpty(t, cmd.Short)
	assert.True(t, cmd.HasSubCommands())

	// Verify "sbs" subcommand exists
	sbsCmd, _, err := cmd.Find([]string{"sbs"})
	assert.NoError(t, err)
	assert.NotNil(t, sbsCmd)
	assert.Contains(t, sbsCmd.Use, "sbs")

	// Verify "sandboxset" alias resolves to the same subcommand
	sandboxsetCmd, _, err := cmd.Find([]string{"sandboxset"})
	assert.NoError(t, err)
	assert.Equal(t, sbsCmd, sandboxsetCmd)

	// Verify the --wait flag is NOT on sbs (it belongs to "set image", not "status")
	assert.Nil(t, sbsCmd.Flags().Lookup("wait"))

	// Verify "suo" subcommand exists
	suoCmd, _, err := cmd.Find([]string{"suo"})
	assert.NoError(t, err)
	assert.NotNil(t, suoCmd)
	assert.Contains(t, suoCmd.Use, "suo")

	// Verify "sandboxupdateops" alias resolves to the same subcommand
	suoAliasCmd, _, err := cmd.Find([]string{"sandboxupdateops"})
	assert.NoError(t, err)
	assert.Equal(t, suoCmd, suoAliasCmd)

	// Verify the --wait flag is NOT on suo (it belongs to "set image", not "status")
	assert.Nil(t, suoCmd.Flags().Lookup("wait"))
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

	opts := &createSuoOptions{
		global: &GlobalOptions{
			KubeConfig: "/nonexistent/config",
			Namespace:  "default",
		},
		selector: "app=my-app",
	}

	err := opts.run([]string{"main=nginx:2.0"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to build kubeconfig")
}

func TestSetImageRunFailsWithInvalidConfig(t *testing.T) {
	origFn := inClusterConfigFn
	defer func() { inClusterConfigFn = origFn }()
	inClusterConfigFn = func() (*rest.Config, error) {
		return nil, fmt.Errorf("not in cluster")
	}

	opts := &setImageOptions{
		global: &GlobalOptions{
			KubeConfig: "/nonexistent/config",
			Namespace:  "default",
		},
	}

	err := opts.run("test-sbs", []string{"main=nginx:2.0"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to build kubeconfig")
}

func TestRestartRunFailsWithInvalidConfig(t *testing.T) {
	origFn := inClusterConfigFn
	defer func() { inClusterConfigFn = origFn }()
	inClusterConfigFn = func() (*rest.Config, error) {
		return nil, fmt.Errorf("not in cluster")
	}

	opts := &restartOptions{
		global: &GlobalOptions{
			KubeConfig: "/nonexistent/config",
			Namespace:  "default",
		},
		containers: []string{"main"},
	}

	err := opts.run("test-sbx")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to build kubeconfig")
}

func TestScaleRunFailsWithInvalidConfig(t *testing.T) {
	origFn := inClusterConfigFn
	defer func() { inClusterConfigFn = origFn }()
	inClusterConfigFn = func() (*rest.Config, error) {
		return nil, fmt.Errorf("not in cluster")
	}

	opts := &scaleOptions{
		global: &GlobalOptions{
			KubeConfig: "/nonexistent/config",
			Namespace:  "default",
		},
		replicas: 5,
	}

	err := opts.run("test-sbs")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to build kubeconfig")
}

func TestCreateSuoRunEmptySelectorFails(t *testing.T) {
	opts := &createSuoOptions{
		global: &GlobalOptions{
			Namespace: "default",
		},
		selector: "",
	}

	err := opts.run([]string{"main=nginx:2.0"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "--selector (-l) is required")
}

// RunE closure tests: verify cobra command execution paths hit AgentsClient error

func TestScaleSandboxsetRunEInvalidConfig(t *testing.T) {
	origFn := inClusterConfigFn
	defer func() { inClusterConfigFn = origFn }()
	inClusterConfigFn = func() (*rest.Config, error) {
		return nil, fmt.Errorf("not in cluster")
	}

	cmd := NewScaleCommand(&GlobalOptions{
		KubeConfig: "/nonexistent/config",
		Namespace:  "default",
	})
	cmd.SetArgs([]string{"sandboxset", "test-sbs", "--replicas=5"})
	err := cmd.Execute()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to build kubeconfig")
}

func TestScaleSbsAliasRunEInvalidConfig(t *testing.T) {
	origFn := inClusterConfigFn
	defer func() { inClusterConfigFn = origFn }()
	inClusterConfigFn = func() (*rest.Config, error) {
		return nil, fmt.Errorf("not in cluster")
	}

	cmd := NewScaleCommand(&GlobalOptions{
		KubeConfig: "/nonexistent/config",
		Namespace:  "default",
	})
	cmd.SetArgs([]string{"sbs", "test-sbs", "--replicas=5"})
	err := cmd.Execute()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to build kubeconfig")
}

func TestSetImageSbsRunEInvalidConfig(t *testing.T) {
	origFn := inClusterConfigFn
	defer func() { inClusterConfigFn = origFn }()
	inClusterConfigFn = func() (*rest.Config, error) {
		return nil, fmt.Errorf("not in cluster")
	}

	cmd := NewSetCommand(&GlobalOptions{
		KubeConfig: "/nonexistent/config",
		Namespace:  "default",
	})
	cmd.SetArgs([]string{"image", "sbs", "test-sbs", "app=nginx:2.0"})
	err := cmd.Execute()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to build kubeconfig")
}

func TestRestartSandboxRunEInvalidConfig(t *testing.T) {
	origFn := inClusterConfigFn
	defer func() { inClusterConfigFn = origFn }()
	inClusterConfigFn = func() (*rest.Config, error) {
		return nil, fmt.Errorf("not in cluster")
	}

	cmd := NewRestartCommand(&GlobalOptions{
		KubeConfig: "/nonexistent/config",
		Namespace:  "default",
	})
	cmd.SetArgs([]string{"sandbox", "test-sbx"})
	err := cmd.Execute()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to build kubeconfig")
}

func TestCreateSuoRunEInvalidConfig(t *testing.T) {
	origFn := inClusterConfigFn
	defer func() { inClusterConfigFn = origFn }()
	inClusterConfigFn = func() (*rest.Config, error) {
		return nil, fmt.Errorf("not in cluster")
	}

	cmd := NewCreateCommand(&GlobalOptions{
		KubeConfig: "/nonexistent/config",
		Namespace:  "default",
	})
	cmd.SetArgs([]string{"suo", "-l", "app=test", "app=nginx:2.0"})
	err := cmd.Execute()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to build kubeconfig")
}
