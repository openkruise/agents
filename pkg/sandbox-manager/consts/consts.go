package consts

import "time"

const (
	OwnerManagerScaleDown = "__manager_scale_down"

	DefaultPoolingCandidateCounts = 100
	DefaultWaitReadyTimeout       = 30 * time.Second
	DefaultClaimWorkers           = 500
)

const (
	ExtProcPort               = 9002
	DefaultExtProcConcurrency = 1000
	RuntimePort               = 49983
	ShutdownTimeout           = 90 * time.Second
	RequestPeerTimeout        = 100 * time.Millisecond
)

const DebugLogLevel = 5
