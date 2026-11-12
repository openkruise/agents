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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
