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

package cachetest_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache/cachetest"
)

func TestNewTestCacheRejectsStaleUpdates(t *testing.T) {
	_, client, err := cachetest.NewTestCache(t)
	require.NoError(t, err)
	ctx := t.Context()
	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "concurrent-timeout"},
	}
	require.NoError(t, client.Create(ctx, sandbox))
	stale := sandbox.DeepCopy()
	now := time.Now().UTC().Truncate(time.Second)
	sandbox.Spec.ShutdownTime = &metav1.Time{Time: now.Add(900 * time.Second)}
	require.NoError(t, client.Update(ctx, sandbox))

	stale.Spec.ShutdownTime = &metav1.Time{Time: now.Add(300 * time.Second)}
	err = client.Update(ctx, stale)
	assert.True(t, apierrors.IsConflict(err), "stale update must conflict, got %v", err)

	stored := &agentsv1alpha1.Sandbox{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: sandbox.Namespace, Name: sandbox.Name}, stored))
	assert.Equal(t, sandbox.Spec.ShutdownTime, stored.Spec.ShutdownTime)
}
