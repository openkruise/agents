package limiter

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewConcurrencyLimiter_ChannelCapacity verifies that the internal channel
// is created with the requested concurrency as its buffer size.
func TestNewConcurrencyLimiter_ChannelCapacity(t *testing.T) {
	concurrency := 5
	l := NewConcurrencyLimiter(concurrency)
	require.NotNil(t, l)
	assert.Equal(t, concurrency, cap(l.C))
}

// TestConcurrencyLimiter_Wait_Acquire verifies that Wait returns a valid
// release function and no error when a slot is available.
func TestConcurrencyLimiter_Wait_Acquire(t *testing.T) {
	l := NewConcurrencyLimiter(1)
	release, err := l.Wait(context.Background())
	require.NoError(t, err)
	require.NotNil(t, release)
	// One slot should be occupied.
	assert.Equal(t, 1, len(l.C))
}

// TestConcurrencyLimiter_Wait_Release verifies that calling the returned
// release function frees the slot so a subsequent Wait can proceed.
func TestConcurrencyLimiter_Wait_Release(t *testing.T) {
	l := NewConcurrencyLimiter(1)

	release, err := l.Wait(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, len(l.C))

	release()
	assert.Equal(t, 0, len(l.C), "slot should be freed after release")

	// A second Wait should now succeed immediately.
	release2, err := l.Wait(context.Background())
	require.NoError(t, err)
	require.NotNil(t, release2)
	release2()
}

// TestConcurrencyLimiter_Wait_ReleaseIdempotent verifies that calling the
// release function more than once does not double-free the channel slot
// (sync.OnceFunc guarantees idempotency).
func TestConcurrencyLimiter_Wait_ReleaseIdempotent(t *testing.T) {
	l := NewConcurrencyLimiter(1)

	release, err := l.Wait(context.Background())
	require.NoError(t, err)

	release()
	release() // second call must not panic or block

	assert.Equal(t, 0, len(l.C))
}

// TestConcurrencyLimiter_Wait_ContextAlreadyCanceled verifies that Wait
// returns the context error when the context is already done and all slots are
// occupied. The limiter is intentionally filled first so that only the
// ctx.Done() case can fire in the select – otherwise Go's random select
// scheduling could pick the channel case even with a cancelled context.
func TestConcurrencyLimiter_Wait_ContextAlreadyCanceled(t *testing.T) {
	l := NewConcurrencyLimiter(1)

	// Fill the only slot so the channel case is not selectable.
	hold, err := l.Wait(context.Background())
	require.NoError(t, err)
	defer hold()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	release, err := l.Wait(ctx)
	assert.Nil(t, release)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestConcurrencyLimiter_Wait_BlocksWhenFull verifies that Wait blocks when
// all slots are occupied, and unblocks once a slot is released.
func TestConcurrencyLimiter_Wait_BlocksWhenFull(t *testing.T) {
	l := NewConcurrencyLimiter(1)

	// Occupy the only slot.
	release, err := l.Wait(context.Background())
	require.NoError(t, err)

	// Start a goroutine that should block until the slot is freed.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	acquired := make(chan struct{})
	go func() {
		r, e := l.Wait(ctx)
		if e == nil {
			r()
		}
		close(acquired)
	}()

	// Give the goroutine a moment to enter Wait and confirm it is blocked.
	time.Sleep(20 * time.Millisecond)
	select {
	case <-acquired:
		t.Fatal("goroutine should still be blocked")
	default:
	}

	// Release the slot – the goroutine should now proceed.
	release()

	select {
	case <-acquired:
		// expected
	case <-time.After(time.Second):
		t.Fatal("goroutine did not unblock after slot was released")
	}
}

// TestConcurrencyLimiter_Wait_ContextCanceledWhileBlocked verifies that a
// goroutine blocked in Wait returns the context error when the context is
// cancelled, without leaking goroutines.
func TestConcurrencyLimiter_Wait_ContextCanceledWhileBlocked(t *testing.T) {
	l := NewConcurrencyLimiter(1)

	// Occupy the only slot so the next Wait will block.
	hold, err := l.Wait(context.Background())
	require.NoError(t, err)
	defer hold()

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		_, e := l.Wait(ctx)
		errCh <- e
	}()

	// Let the goroutine block in Wait, then cancel.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case e := <-errCh:
		assert.ErrorIs(t, e, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("goroutine did not return after context cancellation")
	}
}

// TestConcurrencyLimiter_ConcurrentAcquire verifies that at most `concurrency`
// goroutines hold a slot simultaneously.
func TestConcurrencyLimiter_ConcurrentAcquire(t *testing.T) {
	concurrency := 3
	l := NewConcurrencyLimiter(concurrency)

	const total = 10
	var (
		active    atomic.Int32
		maxActive atomic.Int32
		wg        sync.WaitGroup
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for i := 0; i < total; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := l.Wait(ctx)
			if err != nil {
				return
			}
			current := active.Add(1)
			// Track the peak concurrency observed.
			for {
				old := maxActive.Load()
				if current <= old || maxActive.CompareAndSwap(old, current) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
			active.Add(-1)
			release()
		}()
	}

	wg.Wait()
	assert.LessOrEqual(t, maxActive.Load(), int32(concurrency),
		"concurrent holders must never exceed the configured concurrency")
}
