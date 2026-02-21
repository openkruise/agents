package infra

import (
	"fmt"
	"strings"
	"time"

	"github.com/openkruise/agents/pkg/sandbox-manager/config"
)

type ClaimSandboxOptions struct {
	// User specifies the owner of sandbox, Required
	User string `json:"user"`
	// Template specifies the pool to claim sandbox from, Required
	Template string `json:"template"`
	// CandidateCounts is the maximum number of available sandboxes to select from the cache
	CandidateCounts int `json:"candidateCounts"`
	// Lock string used in optimistic lock
	LockString string `json:"lockString"`
	// PreCheck checks the sandbox before modifying it
	PreCheck func(sandbox Sandbox) error `json:"-"`
	// Set Modifier to modify the Sandbox before it is updated
	Modifier func(sandbox Sandbox) `json:"-"`
	// Set ReserveFailedSandbox to true to reserve failed sandboxes
	ReserveFailedSandbox bool `json:"reserveFailedSandbox"`
	// Set InplaceUpdate to non-empty string trigger an inplace-update
	InplaceUpdate *config.InplaceUpdateOptions `json:"inplaceUpdate"`
	// Set InitRuntime to non-nil value to init the agent-runtime
	InitRuntime *config.InitRuntimeOptions `json:"initRuntime"`
	// Set CSIMount to non-nil value to mount a CSI volume
	CSIMount *config.CSIMountOptions `json:"CSIMount"`
	// Max ClaimTimeout duration
	ClaimTimeout time.Duration `json:"claimTimeout"`
	// Max WaitReadyTimeout duration
	WaitReadyTimeout time.Duration `json:"waitReadyTimeout"`
	// Create a Sandbox instance from the template if no available ones in SandboxSets
	CreateOnNoStock bool `json:"createOnNoStock"`
	// A creating sandbox lasts for SpeculateCreatingDuration may be picked as a candidate when no available ones in SandboxSets.
	// Set to 0 to disable speculation feature
	SpeculateCreatingDuration time.Duration `json:"speculateCreatingDuration"`
}

type ClaimMetrics struct {
	Retries     int
	Total       time.Duration
	Wait        time.Duration
	RetryCost   time.Duration
	PickAndLock time.Duration
	WaitReady   time.Duration
	InitRuntime time.Duration
	CSIMount    time.Duration
	LockType    LockType
	LastError   error
}

type LockType string

const (
	LockTypeCreate    = LockType("create")
	LockTypeUpdate    = LockType("update")
	LockTypeSpeculate = LockType("speculate")
)

func (m ClaimMetrics) String() string {
	var lastErrStr string
	if m.LastError != nil {
		// Replace newlines and control characters to ensure single-line output
		errMsg := m.LastError.Error()
		// Replace common control characters with space
		replacer := strings.NewReplacer("\n", " ", "\r", " ", "\t", " ")
		lastErrStr = replacer.Replace(errMsg)
	}
	return fmt.Sprintf("ClaimMetrics{Retries: %d, Total: %v, Wait: %v, RetryCost: %v, PickAndLock: %v, LockType: %v, WaitReady: %v, InitRuntime: %v, CSIMount: %v, LastError: %v}",
		m.Retries, m.Total, m.Wait, m.RetryCost, m.PickAndLock, m.LockType, m.WaitReady, m.InitRuntime, m.CSIMount, lastErrStr)
}
