/*
Copyright 2025.

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

// Package cachetest provides test utilities for constructing Cache instances
// with a fake client. It is intended exclusively for use in test code.
package cachetest

import (
	"fmt"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/cache/controllers"
	cacheutils "github.com/openkruise/agents/pkg/cache/utils"
)

// NewTestCache creates a Cache with a fake client for testing.
// It reuses BuildCacheConfig to ensure the fake client has the same informer filtering
// configuration as production. This allows tests to verify namespace and label selector behavior.
func NewTestCache(t *testing.T, initObjs ...ctrlclient.Object) (*cache.Cache, ctrlclient.Client, error) {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(agentsv1alpha1.AddToScheme(scheme))

	builder := fake.NewClientBuilder().WithScheme(scheme)

	// Apply indexes from GetIndexFuncs (single source of truth)
	for _, idx := range cache.GetIndexFuncs() {
		builder = builder.WithIndex(idx.Obj, idx.FieldName, idx.Extract)
	}

	builder = builder.WithStatusSubresource(
		&agentsv1alpha1.Sandbox{},
		&agentsv1alpha1.SandboxSet{},
		&agentsv1alpha1.Checkpoint{},
		&agentsv1alpha1.SandboxClaim{},
		&agentsv1alpha1.SandboxTemplate{},
	)

	// Add interceptor to handle resourceVersion conflicts in tests
	builder = builder.WithInterceptorFuncs(cacheutils.ResourceVersionInterceptorFuncs())

	if len(initObjs) > 0 {
		builder = builder.WithObjects(initObjs...)
	}
	fakeClient := builder.Build()
	mgrBuilder, err := controllers.NewMockManagerBuilder(t)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create mock manager builder: %w", err)
	}
	mgr := mgrBuilder.
		WithScheme(scheme).
		WithClient(fakeClient).
		WithWaitSimulation(). // enable wait simulation by default
		Build()

	c, err := cache.NewCache(mgr)
	if err != nil {
		return nil, nil, err
	}

	// Inject waitHooks into MockManager for wait simulation
	mgr.SetWaitHooks(c.GetWaitHooks())

	return c, fakeClient, nil
}
