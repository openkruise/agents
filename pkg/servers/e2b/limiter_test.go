package e2b

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	utils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestController returns a minimal Controller for testing SimpleRateLimiter.
// It avoids starting the full server or initialising Kubernetes clients.
func newTestController(basicQps int) *Controller {
	return &Controller{
		basicQps: basicQps,
	}
}

// makeRequest creates an http.Request whose context is derived from the
// supplied context so that context cancellation is propagated correctly.
func makeRequest(t *testing.T, ctx context.Context) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	return req.WithContext(ctx)
}

// TestSimpleRateLimiter_Allow verifies that a request is allowed when the
// limiter has not been exhausted.
func TestSimpleRateLimiter_Allow(t *testing.T) {
	utils.InitLogOutput()
	sc := newTestController(10)
	middleware := sc.SimpleRateLimiter(sc.basicQps)

	req := makeRequest(t, context.Background())
	ctx, apiErr := middleware(context.Background(), req)

	assert.Nil(t, apiErr, "expected no error for first request within quota")
	assert.NotNil(t, ctx)
}

// TestSimpleRateLimiter_ContextCanceled verifies that a request is rejected
// with an error (not 429) when the request context is already cancelled before
// the middleware is called. This exercises the limiter.Wait cancellation path.
func TestSimpleRateLimiter_ContextCanceled(t *testing.T) {
	utils.InitLogOutput()
	// Use QPS=1, burst=2 – tokens are sufficient, so only a cancelled context
	// will trigger an error here.
	sc := newTestController(1)
	middleware := sc.SimpleRateLimiter(sc.basicQps)

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	req := makeRequest(t, cancelledCtx)
	_, apiErr := middleware(cancelledCtx, req)

	require.NotNil(t, apiErr, "expected an error when request context is cancelled")
	// The limiter.Wait returns a context error; we surface it as 429.
	assert.Equal(t, http.StatusTooManyRequests, apiErr.Code)
	assert.Equal(t, "request blocked by server throttle", apiErr.Message)
}

// TestSimpleRateLimiter_BurstAllowed verifies that burst requests (up to
// burst=qps*2) are all accepted without delay.
func TestSimpleRateLimiter_BurstAllowed(t *testing.T) {
	utils.InitLogOutput()
	qps := 5
	sc := newTestController(qps)
	middleware := sc.SimpleRateLimiter(sc.basicQps)

	burst := qps * 2 // burst capacity as configured in SimpleRateLimiter
	start := time.Now()
	for i := 0; i < burst; i++ {
		req := makeRequest(t, context.Background())
		_, apiErr := middleware(context.Background(), req)
		assert.Nil(t, apiErr, "request %d should be allowed within burst", i)
	}
	// All burst requests should complete without significant waiting.
	assert.Less(t, time.Since(start), time.Second,
		"burst requests should complete quickly without rate limiting")
}

// TestSimpleRateLimiter_ThrottledAfterBurst verifies that once the burst
// capacity is exhausted, a subsequent request is blocked (context times out
// before a token becomes available), confirming the limiter is active.
func TestSimpleRateLimiter_ThrottledAfterBurst(t *testing.T) {
	utils.InitLogOutput()
	// Very low QPS to make throttling obvious: refill is ~1 token/second.
	qps := 1
	sc := newTestController(qps)
	middleware := sc.SimpleRateLimiter(sc.basicQps)

	burst := qps * 2
	// Drain all tokens in the burst bucket.
	for i := 0; i < burst; i++ {
		req := makeRequest(t, context.Background())
		_, apiErr := middleware(context.Background(), req)
		assert.Nil(t, apiErr, "draining request %d should succeed", i)
	}

	// The next request should be throttled because tokens won't be available
	// before the short context deadline expires.
	timeoutCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	req := makeRequest(t, timeoutCtx)
	_, apiErr := middleware(timeoutCtx, req)

	require.NotNil(t, apiErr, "expected request to be blocked after burst exhausted")
	assert.Equal(t, http.StatusTooManyRequests, apiErr.Code)
}

// TestSimpleRateLimiter_ConcurrentAllowedWithinBurst verifies that concurrent
// requests within burst capacity are all accepted without any error.
func TestSimpleRateLimiter_ConcurrentAllowedWithinBurst(t *testing.T) {
	utils.InitLogOutput()
	qps := 10
	sc := newTestController(qps)
	middleware := sc.SimpleRateLimiter(sc.basicQps)

	burst := qps * 2
	var wg sync.WaitGroup
	errors := make([]*string, burst)

	for i := 0; i < burst; i++ {
		wg.Add(1)
		idx := i
		go func() {
			defer wg.Done()
			req := makeRequest(t, context.Background())
			_, apiErr := middleware(context.Background(), req)
			if apiErr != nil {
				msg := apiErr.Message
				errors[idx] = &msg
			}
		}()
	}
	wg.Wait()

	for i, errMsg := range errors {
		assert.Nil(t, errMsg, "concurrent request %d should not be throttled within burst", i)
	}
}

// TestSimpleRateLimiter_IndependentInstances verifies that each call to
// SimpleRateLimiter creates an independent limiter instance, so exhausting
// one does not affect the other.
func TestSimpleRateLimiter_IndependentInstances(t *testing.T) {
	utils.InitLogOutput()
	qps := 1
	sc := newTestController(qps)

	middlewareA := sc.SimpleRateLimiter(qps)
	middlewareB := sc.SimpleRateLimiter(qps)

	burst := qps * 2

	// Exhaust middlewareA's bucket.
	for i := 0; i < burst; i++ {
		req := makeRequest(t, context.Background())
		_, apiErr := middlewareA(context.Background(), req)
		assert.Nil(t, apiErr, "draining middlewareA request %d", i)
	}

	// middlewareB should still have tokens and allow requests.
	req := makeRequest(t, context.Background())
	_, apiErr := middlewareB(context.Background(), req)
	assert.Nil(t, apiErr, "middlewareB should be unaffected by middlewareA exhaustion")
}
