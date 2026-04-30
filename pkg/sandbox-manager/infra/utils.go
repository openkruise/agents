package infra

import "time"

func PushTimeout(timeout *TimeoutOptions, duration time.Duration) {
	if timeout == nil {
		return
	}
	if !timeout.PauseTime.IsZero() {
		timeout.PauseTime = timeout.PauseTime.Add(duration)
	}
	if !timeout.ShutdownTime.IsZero() {
		timeout.ShutdownTime = timeout.ShutdownTime.Add(duration)
	}
}
