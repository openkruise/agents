package limiter

import (
	"context"
	"sync"
)

func NewConcurrencyLimiter(concurrency int) *ConcurrencyLimiter {
	return &ConcurrencyLimiter{C: make(chan struct{}, concurrency)}
}

type ConcurrencyLimiter struct {
	C chan struct{}
}

func (r *ConcurrencyLimiter) Wait(ctx context.Context) (func(), error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r.C <- struct{}{}:
		return sync.OnceFunc(func() {
			<-r.C // free the worker
		}), nil
	}
}
