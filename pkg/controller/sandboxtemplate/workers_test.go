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

package sandboxtemplate

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestWorkerRegistryRunsOnlyOnLeader(t *testing.T) {
	require.True(t, (&Reconciler{}).NeedLeaderElection())
}

func TestWorkerRegistryLifecycle(t *testing.T) {
	r, version, _, _ := executionFixture(t)
	sibling := version.DeepCopy()
	sibling.Name, sibling.UID = "st-bcdefghijklmnopqrstu0", "sibling-uid"

	first, firstCancel := context.WithCancel(t.Context())
	defer firstCancel()
	second, secondCancel := context.WithCancel(t.Context())
	defer secondCancel()
	r.workers = map[types.UID]*buildWorker{
		version.UID: {key: client.ObjectKeyFromObject(version), cancel: firstCancel},
		sibling.UID: {key: client.ObjectKeyFromObject(sibling), cancel: secondCancel},
	}

	require.True(t, r.hasWorker(version.UID))
	require.True(t, r.hasWorker(sibling.UID))

	require.True(t, r.stopVersionWorker(client.ObjectKeyFromObject(version)))
	require.ErrorIs(t, first.Err(), context.Canceled)
	require.NoError(t, second.Err())
	// Registry entries are removed by the worker goroutines themselves; a
	// cancelled worker keeps its entry until it drains.
	require.True(t, r.hasWorker(version.UID))

	require.True(t, r.stopVersionWorker(client.ObjectKeyFromObject(sibling)))
	require.ErrorIs(t, second.Err(), context.Canceled)

	require.False(t, r.stopVersionWorker(client.ObjectKey{Namespace: "default", Name: "st-unknown000000000000"}))
}
