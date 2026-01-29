package consts

import "time"

const (
	OwnerManagerScaleDown = "__manager_scale_down"

	DefaultPoolingCandidateCounts = 100
	DefaultInplaceUpdateTimeout   = 30 * time.Second
)

const (
	ExtProcPort        = 9002
	ShutdownTimeout    = 90 * time.Second
	RequestPeerTimeout = 100 * time.Millisecond
)

const DebugLogLevel = 5
