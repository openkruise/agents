package sandboxcr

import (
	"sync"
	"time"
)

var (
	LockTimeout       = time.Minute
	RetryInterval     = 10 * time.Millisecond
	LockBackoffFactor = 1.0
	LockJitter        = 0.1
)

var configMu sync.Mutex

func SetClaimLockTimeout(duration time.Duration) {
	configMu.Lock()
	LockTimeout = duration
	configMu.Unlock()
}
