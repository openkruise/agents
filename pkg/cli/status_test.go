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

func TestStatusSbsRunEInvalidConfig(t *testing.T) {
	origFn := inClusterConfigFn
	defer func() { inClusterConfigFn = origFn }()
	inClusterConfigFn = func() (*rest.Config, error) {
		return nil, fmt.Errorf("not in cluster")
	}

	cmd := NewStatusCommand(&GlobalOptions{
		KubeConfig: "/nonexistent/config",
		Namespace:  "default",
	})
	cmd.SetArgs([]string{"sbs", "test-sbs"})
	err := cmd.Execute()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to build kubeconfig")
}

func TestStatusSuoRunEInvalidConfig(t *testing.T) {
	origFn := inClusterConfigFn
	defer func() { inClusterConfigFn = origFn }()
	inClusterConfigFn = func() (*rest.Config, error) {
		return nil, fmt.Errorf("not in cluster")
	}

	cmd := NewStatusCommand(&GlobalOptions{
		KubeConfig: "/nonexistent/config",
		Namespace:  "default",
	})
	cmd.SetArgs([]string{"suo", "test-suo"})
	err := cmd.Execute()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to build kubeconfig")
}

func TestStatusSbsSandboxsetAlias(t *testing.T) {
	origFn := inClusterConfigFn
	defer func() { inClusterConfigFn = origFn }()
	inClusterConfigFn = func() (*rest.Config, error) {
		return nil, fmt.Errorf("not in cluster")
	}

	// Both "sbs" and "sandboxset" should resolve to the same RunE
	cmd := NewStatusCommand(&GlobalOptions{
		KubeConfig: "/nonexistent/config",
		Namespace:  "default",
	})
	cmd.SetArgs([]string{"sandboxset", "test-sbs"})
	err := cmd.Execute()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to build kubeconfig")
}
