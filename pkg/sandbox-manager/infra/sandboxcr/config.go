package sandboxcr

import (
	"time"
)

var (
	DefaultClaimTimeout = time.Minute
	RetryInterval       = 10 * time.Millisecond
	LockBackoffFactor   = 1.0
	LockJitter          = 0.2
)
