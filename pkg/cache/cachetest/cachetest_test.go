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

package cachetest

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestSandboxSetLookupDoesNotWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, _, err := NewTestCache(t, &agentsv1alpha1.SandboxSet{
			ObjectMeta: metav1.ObjectMeta{Name: "template", Namespace: "team-a"},
		})
		require.NoError(t, err)
		start := time.Now()
		got, err := c.PickSandboxSet(t.Context(), cache.PickSandboxSetOptions{Name: "template"})
		require.NoError(t, err)
		require.Equal(t, "team-a", got.Namespace)
		for _, opts := range []cache.PickSandboxSetOptions{
			{Namespace: "team-b", Name: "template"},
			{Namespace: "team-a", Name: "missing"},
		} {
			_, err := c.PickSandboxSet(t.Context(), opts)
			require.ErrorContains(t, err, "not found in cache")
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err = c.PickSandboxSet(ctx, cache.PickSandboxSetOptions{Name: "template"})
		require.ErrorIs(t, err, context.Canceled)
		require.Zero(t, time.Since(start), "fake reads must not wait for informer propagation")
	})
}

func TestResourceVersionInterceptor(t *testing.T) {
	for _, tt := range []struct {
		name    string
		version string
		missing bool
	}{
		{name: "unconditional CRD update"},
		{name: "current resource version", version: "2"},
		{name: "stale resource version", version: "1"},
		{name: "missing object", missing: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stored := &agentsv1alpha1.SandboxSet{ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "team-a", ResourceVersion: "2", Labels: map[string]string{"writer": "winner"}}}
			_, c, err := NewTestCache(t, stored)
			require.NoError(t, err)
			object := stored.DeepCopy()
			object.ResourceVersion = tt.version
			object.Labels["writer"] = "request"
			if tt.missing {
				object.Name = "missing"
			}
			err = c.Update(t.Context(), object)
			current := &agentsv1alpha1.SandboxSet{}
			require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(stored), current))
			switch {
			case tt.missing:
				require.True(t, apierrors.IsNotFound(err), "%v", err)
				require.Equal(t, "", object.ResourceVersion)
				require.Equal(t, "winner", current.Labels["writer"])
			case tt.version == "1":
				require.True(t, apierrors.IsConflict(err), "%v", err)
				require.Equal(t, "1", object.ResourceVersion)
				require.Equal(t, "winner", current.Labels["writer"])
				require.Equal(t, "2", current.ResourceVersion)
			default:
				require.NoError(t, err)
				require.Equal(t, "request", current.Labels["writer"])
				require.Equal(t, current.ResourceVersion, object.ResourceVersion)
			}
		})
	}
}
