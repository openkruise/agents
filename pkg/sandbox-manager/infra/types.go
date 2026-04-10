/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package infra

import (
	"fmt"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/openkruise/agents/api/v1alpha1"
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
	// Set InplaceUpdate to trigger an inplace-update (image and/or resources)
	InplaceUpdate *config.InplaceUpdateOptions `json:"inplaceUpdate"`
	// Set RuntimeConfig to non-nil value to inject runtime configuration
	RuntimeConfig []v1alpha1.RuntimeConfig `json:"runtimeConfig"`
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

type CloneSandboxOptions struct {
	User               string                  `json:"user"`
	CheckPointID       string                  `json:"checkPointID"`
	WaitReadyTimeout   time.Duration           `json:"waitReadyTimeout"`
	CloneTimeout       time.Duration           `json:"cloneTimeout"`
	CSIMount           *config.CSIMountOptions `json:"CSIMount"`
	Modifier           func(sbx Sandbox)       `json:"-"`
	CreateLimiter      *rate.Limiter           `json:"-"`
	SkipWaitCheckpoint bool                    `json:"skipWaitCheckpoint"`
}

type CreateCheckpointOptions struct {
	KeepRunning        *bool         `json:"keepRunning,omitempty"`
	TTL                *string       `json:"TTL,omitempty"`
	PersistentContents []string      `json:"persistentMemory"`
	WaitSuccessTimeout time.Duration `json:"waitSuccessTimeout"`
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

type CloneMetrics struct {
	Wait          time.Duration
	GetTemplate   time.Duration
	CreateSandbox time.Duration
	WaitReady     time.Duration
	InitRuntime   time.Duration
	CSIMount      time.Duration
	Total         time.Duration
}

func (m CloneMetrics) String() string {
	return fmt.Sprintf("CloneMetrics{Wait: %v, GetTemplate: %v, CreateSandbox: %v, WaitReady: %v, InitRuntime: %v, CSIMount: %v, Total: %v}",
		m.Wait, m.GetTemplate, m.CreateSandbox, m.WaitReady, m.InitRuntime, m.CSIMount, m.Total)
}
