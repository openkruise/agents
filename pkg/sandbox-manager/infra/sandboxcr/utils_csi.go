package sandboxcr

import (
	"context"
	"time"

	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

func CSIMount(ctx context.Context, sbx *Sandbox, opts infra.CSIMountOptions) (time.Duration, error) {
	start := time.Now()
	err := sbx.CSIMount(ctx, opts.Driver, opts.RequestRaw)
	return time.Since(start), err
}
