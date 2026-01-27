package infra

import (
	"fmt"
	"time"
)

type ClaimSandboxOptions struct {
	// User specifies the owner of sandbox, Required
	User string
	// Template specifies the pool to claim sandbox from, Required
	Template string
	// CandidateCounts is the maximum number of available sandboxes to select from the cache
	CandidateCounts int
	// Lock string used in optimistic lock
	LockString string
	// Set Modifier to modify the Sandbox before it is updated
	Modifier func(sandbox Sandbox)
	// Set ReserveFailedSandbox to true to reserve failed sandboxes
	ReserveFailedSandbox bool
	// Set InplaceUpdate to non-empty string trigger an inplace-update
	InplaceUpdate *InplaceUpdateOptions
	// Set InitRuntime to non-nil value to init the agent-runtime
	InitRuntime *InitRuntimeOptions
	// Set CSIMount to non-nil value to mount a CSI volume
	CSIMount *CSIMountOptions
}

type ClaimMetrics struct {
	Retries       int
	Total         time.Duration
	Wait          time.Duration
	PickAndLock   time.Duration
	InplaceUpdate time.Duration
	InitRuntime   time.Duration
	CSIMount      time.Duration
}

func (m ClaimMetrics) String() string {
	return fmt.Sprintf(
		"ClaimMetrics{Retries: %d, Total: %s, Wait: %s, PickAndLock: %s, InplaceUpdate: %s, InitRuntime: %s, CSIMount: %s}",
		m.Retries, m.Total, m.Wait, m.PickAndLock, m.InplaceUpdate, m.InitRuntime, m.CSIMount)
}

type InplaceUpdateOptions struct {
	Image   string
	Timeout time.Duration
}

type InitRuntimeOptions struct {
	EnvVars     map[string]string
	AccessToken string
}

type CSIMountOptions struct {
	Driver     string
	RequestRaw string
}
