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

package identityprovider

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func TestRegisterSecurityTokenPropagator(t *testing.T) {
	// Save and restore the global slice.
	saved := securityTokenPropagators
	defer func() { securityTokenPropagators = saved }()
	securityTokenPropagators = nil

	assert.Equal(t, 0, SecurityTokenPropagatorCount())

	// Register first propagator.
	RegisterSecurityTokenPropagator(func(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *TokenResponse) error {
		return nil
	})
	assert.Equal(t, 1, SecurityTokenPropagatorCount())

	// Register second propagator.
	RegisterSecurityTokenPropagator(func(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *TokenResponse) error {
		return fmt.Errorf("err")
	})
	assert.Equal(t, 2, SecurityTokenPropagatorCount())
}

func TestSecurityTokenPropagatorCount_Empty(t *testing.T) {
	saved := securityTokenPropagators
	defer func() { securityTokenPropagators = saved }()
	securityTokenPropagators = nil

	assert.Equal(t, 0, SecurityTokenPropagatorCount())
}
