package common

import (
	"time"
)

type Status string

const (
	StatusSuccess Status = "Success"
	StatusFailure Status = "Failure"
	StatusUnknown Status = "Unknown"
)

const (
	IdleTimeout = 640 * time.Second
	MaxAge      = 2 * time.Hour
)