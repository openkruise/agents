package sandboxcr

import (
	"sync"
	"time"
)

var (
	DefaultClaimTimeout = time.Minute
	RetryInterval       = 10 * time.Millisecond
	LockBackoffFactor   = 1.0
	LockJitter          = 0.1
)

var configMu sync.Mutex

func SetClaimTimeout(duration time.Duration) {
	configMu.Lock()
	DefaultClaimTimeout = duration
	configMu.Unlock()
}
