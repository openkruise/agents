package e2b

import (
	"context"
	"net/http"
	"time"

	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/servers/web"
	"golang.org/x/time/rate"
	"k8s.io/klog/v2"
)

func (sc *Controller) SimpleRateLimiter(qps int) web.MiddleWare {
	limiter := rate.NewLimiter(rate.Limit(qps), qps*2)
	return func(ctx context.Context, r *http.Request) (context.Context, *web.ApiError) {
		log := klog.FromContext(ctx).WithValues("middleware", "RateLimiter")
		start := time.Now()
		if err := limiter.Wait(r.Context()); err != nil {
			log.Error(err, "request blocked by rate limiter")
			return ctx, &web.ApiError{
				Code:    http.StatusTooManyRequests,
				Message: "request blocked by server throttle",
			}
		}
		log.V(consts.DebugLogLevel).Info("request allowed by rate limiter", "latency", time.Since(start))
		return ctx, nil
	}
}
