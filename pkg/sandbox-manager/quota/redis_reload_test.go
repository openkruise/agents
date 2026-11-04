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

package quota

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Block one real command while the backend operation owns its connection.
type blockedRedisCommand struct {
	started, release chan struct{}
	once             sync.Once
}

func (h *blockedRedisCommand) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *blockedRedisCommand) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *blockedRedisCommand) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "del" {
			h.once.Do(func() { close(h.started); <-h.release })
		}
		return next(ctx, cmd)
	}
}

func TestRedisReplacementDrainsOperations(t *testing.T) {
	srv := miniredis.RunT(t)
	old := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	next := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	backend := NewRedisBackend(old, time.Second)
	t.Cleanup(func() { require.NoError(t, backend.Close()) })
	hook := &blockedRedisCommand{started: make(chan struct{}), release: make(chan struct{})}
	old.AddHook(hook)
	operation := make(chan error, 1)
	go func() { operation <- backend.Cleanup(t.Context(), "user") }()
	<-hook.started
	replaced := make(chan error, 1)
	go func() {
		published, err := backend.ReplaceClient(t.Context(), next)
		if !published {
			t.Error("replacement did not publish")
		}
		replaced <- err
	}()
	select {
	case err := <-replaced:
		t.Fatalf("closed a connection with an active operation: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	require.NoError(t, old.Ping(t.Context()).Err())
	close(hook.release)
	require.NoError(t, <-operation)
	require.NoError(t, <-replaced)
	require.Error(t, old.Ping(t.Context()).Err())
	require.NoError(t, backend.Cleanup(t.Context(), "user"))
}

func TestRedisReplacementRespectsOperationDeadlines(t *testing.T) {
	for _, operation := range []string{"acquire", "release", "list", "cleanup"} {
		t.Run(operation, func(t *testing.T) {
			srv := miniredis.RunT(t)
			old := redis.NewClient(&redis.Options{Addr: srv.Addr()})
			next := redis.NewClient(&redis.Options{Addr: srv.Addr()})
			backend := NewRedisBackend(old, 50*time.Millisecond)
			hook := &blockedRedisCommand{started: make(chan struct{}), release: make(chan struct{})}
			old.AddHook(hook)
			maintenance := make(chan error, 1)
			go func() { maintenance <- backend.Cleanup(t.Context(), "user") }()
			<-hook.started
			replaced := make(chan error, 1)
			go func() {
				published, err := backend.ReplaceClient(t.Context(), next)
				if !published {
					t.Error("replacement did not publish")
				}
				replaced <- err
			}()
			defer func() {
				close(hook.release)
				require.NoError(t, <-maintenance)
				require.NoError(t, <-replaced)
				require.NoError(t, backend.Close())
			}()
			// A queued writer prevents a new reader even though tokens remain free.
			require.Eventually(t, func() bool {
				if backend.mu.TryAcquire(1) {
					backend.mu.Release(1)
					return false
				}
				return true
			}, time.Second, time.Millisecond)
			done := make(chan error, 1)
			go func() {
				if operation == "acquire" {
					done <- backend.Acquire(t.Context(), AcquireParams{User: "user", LockString: "lock"})
					return
				}
				ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
				defer cancel()
				switch operation {
				case "release":
					done <- backend.Release(ctx, "user", "lock")
				case "list":
					_, err := backend.ListEntries(ctx, "user")
					done <- err
				case "cleanup":
					done <- backend.Cleanup(ctx, "user")
				}
			}()
			select {
			case err := <-done:
				require.ErrorIs(t, err, context.DeadlineExceeded)
			case <-time.After(500 * time.Millisecond):
				t.Error("new operation exceeded its budget while replacement waited for maintenance")
			}
		})
	}
}

func TestRedisReplacementCancellationKeepsOldClient(t *testing.T) {
	srv := miniredis.RunT(t)
	old := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	next := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	backend := NewRedisBackend(old, time.Second)
	hook := &blockedRedisCommand{started: make(chan struct{}), release: make(chan struct{})}
	old.AddHook(hook)
	maintenance := make(chan error, 1)
	go func() { maintenance <- backend.Cleanup(t.Context(), "user") }()
	<-hook.started
	defer func() {
		close(hook.release)
		require.NoError(t, <-maintenance)
		require.NoError(t, backend.Close())
		require.NoError(t, next.Close())
	}()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		published, err := backend.ReplaceClient(ctx, next)
		assert.False(t, published)
		assert.ErrorIs(t, err, context.Canceled)
	}()
	require.Eventually(t, func() bool {
		if backend.mu.TryAcquire(1) {
			backend.mu.Release(1)
			return false
		}
		return true
	}, time.Second, time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled replacement did not return while old operation was active")
	}
	require.Same(t, old, backend.client)
	require.NoError(t, old.Ping(t.Context()).Err())
	// The caller still owns an unpublished candidate.
	require.NoError(t, next.Ping(t.Context()).Err())
}
