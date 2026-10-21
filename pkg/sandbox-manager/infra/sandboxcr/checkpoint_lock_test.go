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

package sandboxcr

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

// newCheckpointLockTestSandbox stores a Sandbox with the given annotations and
// wraps it with a client intercepted by funcs, which stands in for the informer
// view and the API server write path.
func newCheckpointLockTestSandbox(
	t *testing.T,
	annotations map[string]string,
	funcs interceptor.Funcs,
) (*Sandbox, client.Client, *countingReader) {
	t.Helper()
	cache, fc, err := cachetest.NewTestCache(t)
	require.NoError(t, err)
	sbx := createTestSandboxWithDefaults("lock-source", "default")
	sbx.Annotations = annotations
	require.NoError(t, fc.Create(t.Context(), sbx))

	reader := &countingReader{Reader: fc}
	provider := &apiReaderOverrideCache{
		Provider:  cache,
		apiReader: reader,
		client:    interceptor.NewClient(fc.(client.WithWatch), funcs),
	}
	stored := &v1alpha1.Sandbox{}
	require.NoError(t, fc.Get(t.Context(), client.ObjectKeyFromObject(sbx), stored))
	return AsSandbox(stored, provider), fc, reader
}

func mustEncodeCheckpointLock(t *testing.T, lock v1alpha1.CheckpointLock) string {
	t.Helper()
	value, err := encodeCheckpointLock(lock)
	require.NoError(t, err)
	return value
}

func sandboxConflict() error {
	return apierrors.NewConflict(schema.GroupResource{Group: v1alpha1.GroupVersion.Group, Resource: "sandboxes"}, "lock-source", errors.New("forced conflict"))
}

func TestSandbox_AcquireCheckpointLock(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	foreignActive := v1alpha1.CheckpointLock{Holder: "other", Operation: "fork", ExpireTime: metav1.NewTime(now.Add(time.Minute))}
	foreignExpired := v1alpha1.CheckpointLock{Holder: "other", Operation: "fork", ExpireTime: metav1.NewTime(now.Add(-time.Second))}
	const ttl = 2 * time.Minute

	tests := []struct {
		name     string
		existing func(t *testing.T) string
		// staleInformer serves an older view that still records foreignActive.
		staleInformer bool
		// staleWrapper rolls the wrapper back to before the existing lock.
		staleWrapper    bool
		conflicts       int32
		expectCode      managererrors.ErrorCode
		expectErrSubstr string
		expectPatches   int32
	}{
		{
			name:          "unlocked sandbox",
			expectPatches: 1,
		},
		{
			name:          "stale informer view yields to a newer wrapper",
			staleInformer: true,
			expectPatches: 1,
		},
		{
			name: "newer informer view with a foreign lock wins over a stale wrapper",
			existing: func(t *testing.T) string {
				return mustEncodeCheckpointLock(t, foreignActive)
			},
			staleWrapper:    true,
			expectCode:      managererrors.ErrorConflict,
			expectErrSubstr: "has a fork checkpoint in progress",
		},
		{
			name: "expired lock is replaced",
			existing: func(t *testing.T) string {
				return mustEncodeCheckpointLock(t, foreignExpired)
			},
			expectPatches: 1,
		},
		{
			name: "unparsable lock is replaced",
			existing: func(*testing.T) string {
				return "not-json"
			},
			expectPatches: 1,
		},
		{
			name: "active foreign lock conflicts",
			existing: func(t *testing.T) string {
				return mustEncodeCheckpointLock(t, foreignActive)
			},
			expectCode:      managererrors.ErrorConflict,
			expectErrSubstr: "has a fork checkpoint in progress",
		},
		{
			name:          "write conflict retries from the informer view",
			conflicts:     1,
			expectPatches: 2,
		},
		{
			name:          "exhausted write conflicts are internal",
			conflicts:     100,
			expectCode:    managererrors.ErrorInternal,
			expectPatches: 4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			annotations := map[string]string{"keep": "value"}
			if tt.existing != nil {
				annotations[v1alpha1.AnnotationCheckpointLock] = tt.existing(t)
			}
			var patches atomic.Int32
			s, fc, reader := newCheckpointLockTestSandbox(t, annotations, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if err := c.Get(ctx, key, obj, opts...); err != nil {
						return err
					}
					if sbx, ok := obj.(*v1alpha1.Sandbox); ok && tt.staleInformer {
						rv, err := strconv.ParseUint(sbx.ResourceVersion, 10, 64)
						require.NoError(t, err)
						sbx.ResourceVersion = strconv.FormatUint(rv-1, 10)
						sbx.Annotations[v1alpha1.AnnotationCheckpointLock] = mustEncodeCheckpointLock(t, foreignActive)
					}
					return nil
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if patches.Add(1) <= tt.conflicts {
						return sandboxConflict()
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
			})
			if tt.staleWrapper {
				rv, err := strconv.ParseUint(s.GetResourceVersion(), 10, 64)
				require.NoError(t, err)
				s.SetResourceVersion(strconv.FormatUint(rv-1, 10))
				delete(s.Annotations, v1alpha1.AnnotationCheckpointLock)
			}
			before := time.Now()

			lock, err := s.AcquireCheckpointLock(t.Context(), infra.CheckpointLockOptions{Operation: "snapshot", TTL: ttl})

			assert.Equal(t, tt.expectPatches, patches.Load())
			assert.Equal(t, int32(0), reader.Calls(), "acquisition must not read the API server directly")
			stored := &v1alpha1.Sandbox{}
			require.NoError(t, fc.Get(t.Context(), client.ObjectKeyFromObject(s.Sandbox), stored))
			assert.Equal(t, "value", stored.Annotations["keep"])
			if tt.expectCode != "" {
				require.Error(t, err)
				assert.Equal(t, tt.expectCode, managererrors.GetErrCode(err))
				assert.ErrorContains(t, err, tt.expectErrSubstr)
				assert.Equal(t, annotations[v1alpha1.AnnotationCheckpointLock], stored.Annotations[v1alpha1.AnnotationCheckpointLock])
				return
			}
			require.NoError(t, err)
			assert.NotEmpty(t, lock.Holder)
			assert.NotEqual(t, foreignActive.Holder, lock.Holder)
			assert.Equal(t, "snapshot", lock.Operation)
			assert.False(t, lock.ExpireTime.Time.Before(before.Add(ttl).Truncate(time.Second).Add(-time.Second)))
			assert.False(t, lock.ExpireTime.Time.After(time.Now().Add(ttl)))
			value := mustEncodeCheckpointLock(t, lock)
			assert.Equal(t, value, stored.Annotations[v1alpha1.AnnotationCheckpointLock])
			assert.Equal(t, value, s.GetAnnotations()[v1alpha1.AnnotationCheckpointLock], "acquisition refreshes the wrapper")
			assert.Equal(t, stored.ResourceVersion, s.GetResourceVersion())
		})
	}
}

func TestSandbox_ReleaseCheckpointLock(t *testing.T) {
	own := v1alpha1.CheckpointLock{Holder: "own", Operation: "fork", ExpireTime: metav1.NewTime(time.Now().UTC().Truncate(time.Second).Add(time.Minute))}
	foreign := v1alpha1.CheckpointLock{Holder: "other", Operation: "snapshot", ExpireTime: own.ExpireTime}

	tests := []struct {
		name     string
		existing *v1alpha1.CheckpointLock
		// acquireFirst releases the lock returned by a real acquisition.
		acquireFirst bool
		patchErr     error
		// staleInformer serves an older view that has not observed the lock.
		staleInformer bool
		deleteBefore  bool
		expectErr     bool
		expectLock    *v1alpha1.CheckpointLock
	}{
		{
			name:         "releases the lock returned by acquisition",
			acquireFirst: true,
		},
		{
			name:       "keeps lock taken over after expiry",
			existing:   &foreign,
			expectLock: &foreign,
		},
		{
			name: "absent lock is a no-op",
		},
		{
			name:       "write failure on own lock is returned",
			existing:   &own,
			patchErr:   apierrors.NewServiceUnavailable("injected"),
			expectErr:  true,
			expectLock: &own,
		},
		{
			name:          "write failure is returned when the informer missed the lock",
			existing:      &own,
			patchErr:      apierrors.NewServiceUnavailable("injected"),
			staleInformer: true,
			expectErr:     true,
			expectLock:    &own,
		},
		{
			name:         "deleted sandbox is a no-op",
			existing:     &own,
			deleteBefore: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			annotations := map[string]string{"keep": "value"}
			if tt.existing != nil {
				annotations[v1alpha1.AnnotationCheckpointLock] = mustEncodeCheckpointLock(t, *tt.existing)
			}
			s, fc, reader := newCheckpointLockTestSandbox(t, annotations, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if err := c.Get(ctx, key, obj, opts...); err != nil {
						return err
					}
					if sbx, ok := obj.(*v1alpha1.Sandbox); ok && tt.staleInformer {
						rv, err := strconv.ParseUint(sbx.ResourceVersion, 10, 64)
						require.NoError(t, err)
						sbx.ResourceVersion = strconv.FormatUint(rv-1, 10)
						delete(sbx.Annotations, v1alpha1.AnnotationCheckpointLock)
					}
					return nil
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if tt.patchErr != nil {
						return tt.patchErr
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
			})
			if tt.deleteBefore {
				require.NoError(t, fc.Delete(t.Context(), s.Sandbox.DeepCopy()))
			}
			release := own
			if tt.acquireFirst {
				var err error
				release, err = s.AcquireCheckpointLock(t.Context(), infra.CheckpointLockOptions{Operation: "fork", TTL: time.Minute})
				require.NoError(t, err)
			}

			err := s.ReleaseCheckpointLock(t.Context(), release)

			assert.Equal(t, int32(0), reader.Calls(), "release must not read the API server directly")
			if tt.expectErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if tt.deleteBefore {
				return
			}
			stored := &v1alpha1.Sandbox{}
			require.NoError(t, fc.Get(t.Context(), client.ObjectKeyFromObject(s.Sandbox), stored))
			assert.Equal(t, "value", stored.Annotations["keep"])
			if tt.expectLock == nil {
				assert.NotContains(t, stored.Annotations, v1alpha1.AnnotationCheckpointLock)
				return
			}
			assert.Equal(t, mustEncodeCheckpointLock(t, *tt.expectLock), stored.Annotations[v1alpha1.AnnotationCheckpointLock])
		})
	}
}
