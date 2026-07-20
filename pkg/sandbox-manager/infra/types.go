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
	"context"
	"fmt"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/utils/timeout"
)

type SaveTimeoutOptions struct {
	Timeout          timeout.Options
	ExtraAnnotations map[string]string
}

type SandboxAdmission struct {
	Acquire func(ctx context.Context, lockString string, resource SandboxResource) error
	Release func(ctx context.Context, lockString string) error
}

const SandboxAdmissionReleaseTimeout = 250 * time.Millisecond

type ClaimSandboxOptions struct {
	Namespace string `json:"namespace,omitempty"`
	// User specifies the owner of sandbox, Required
	User string `json:"user"`
	// Template specifies the pool to claim sandbox from, Required
	Template string `json:"template"`
	// CandidateCounts is the maximum number of available sandboxes to select from the cache
	CandidateCounts int `json:"candidateCounts"`
	// Lock string used in optimistic lock
	LockString string            `json:"lockString"`
	Admission  *SandboxAdmission `json:"-"`
	// PreCheck checks the sandbox before modifying it
	PreCheck func(sandbox Sandbox) error `json:"-"`
	// Set Modifier to modify the Sandbox before it is updated
	Modifier func(sandbox Sandbox) `json:"-"`
	// ReserveFailedSandboxFor controls how long failed sandboxes are kept for debugging.
	//   nil                          — backend default (DefaultReserveFailedSandboxFor)
	//   ReserveFailedSandboxNever    — delete immediately
	//   positive                     — reserve for that duration, then delete
	//   ReserveFailedSandboxForever  — reserve forever (never delete)
	ReserveFailedSandboxFor *time.Duration `json:"reserveFailedSandboxFor"`
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
	// UserMetadataKeys records the keys of user-specified labels/annotations
	// added during claim (from SandboxClaim.Spec or E2B request). Used by the
	// cleanup flow to clean up user metadata when returning the sandbox to the pool.
	UserMetadataKeys *v1alpha1.UpdatedMetadataInClaim `json:"-"`
	// Claim is the SandboxClaim that triggers this claim. May be nil for
	// non-CRD paths such as the E2B API. IdentityProvider implementations must
	// handle a nil Claim gracefully.
	Claim *v1alpha1.SandboxClaim `json:"-"`
}

type CloneSandboxOptions struct {
	Namespace          string                  `json:"namespace,omitempty"`
	User               string                  `json:"user"`
	CheckPointID       string                  `json:"checkPointID"`
	LockString         string                  `json:"lockString"`
	Admission          *SandboxAdmission       `json:"-"`
	WaitReadyTimeout   time.Duration           `json:"waitReadyTimeout"`
	CloneTimeout       time.Duration           `json:"cloneTimeout"`
	CSIMount           *config.CSIMountOptions `json:"CSIMount"`
	Modifier           func(sbx Sandbox)       `json:"-"`
	CreateLimiter      *rate.Limiter           `json:"-"`
	SkipWaitCheckpoint bool                    `json:"skipWaitCheckpoint"`
	// See ReserveFailedSandboxFor on ClaimSandboxOptions.
	ReserveFailedSandboxFor *time.Duration `json:"reserveFailedSandboxFor"`
	// Name sets ObjectMeta.Name on the cloned sandbox (exact name).
	// Mutually exclusive with GenerateName.
	Name string `json:"name,omitempty"`
	// GenerateName sets ObjectMeta.GenerateName on the cloned sandbox (prefix).
	// Mutually exclusive with Name.
	GenerateName string `json:"generateName,omitempty"`
}

type CreateCheckpointOptions struct {
	KeepRunning        *bool         `json:"keepRunning,omitempty"`
	TTL                *string       `json:"TTL,omitempty"`
	PersistentContents []string      `json:"persistentMemory"`
	WaitSuccessTimeout time.Duration `json:"waitSuccessTimeout"`
}

type ClaimMetrics struct {
	Retries             int
	Total               time.Duration
	Wait                time.Duration
	RetryCost           time.Duration
	PickAndLock         time.Duration
	WaitReady           time.Duration
	InitRuntime         time.Duration
	CSIMount            time.Duration
	SecurityToken       time.Duration
	TrafficToken        time.Duration
	LockType            LockType
	LastError           error
	PickSandboxFailures []PickSandboxFailure
}

// PickSandboxFailure describes a group of claim attempts that failed with the same picked sandbox and reason.
type PickSandboxFailure struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

type LockType string

const (
	LockTypeCreate    = LockType("create")
	LockTypeUpdate    = LockType("update")
	LockTypeSpeculate = LockType("speculate")
)

func (m *ClaimMetrics) String() string {
	var lastErrStr string
	if m.LastError != nil {
		// Replace newlines and control characters to ensure single-line output
		lastErrStr = sanitizeErrorMessage(m.LastError)
	}
	return fmt.Sprintf("ClaimMetrics{Retries: %d, Total: %v, Wait: %v, RetryCost: %v, PickAndLock: %v, LockType: %v, WaitReady: %v, InitRuntime: %v, CSIMount: %v, SecurityToken: %v, TrafficToken: %v, LastError: %v}",
		m.Retries, m.Total, m.Wait, m.RetryCost, m.PickAndLock, m.LockType, m.WaitReady, m.InitRuntime, m.CSIMount, m.SecurityToken, m.TrafficToken, lastErrStr)
}

// RecordPickSandboxFailure records one failed claim attempt and aggregates repeated key/reason pairs.
func (m *ClaimMetrics) RecordPickSandboxFailure(key string, err error) {
	if err == nil {
		return
	}
	m.mergePickSandboxFailure(PickSandboxFailure{
		Key:    key,
		Reason: sanitizeErrorMessage(err),
		Count:  1,
	})
}

// MergePickSandboxFailures merges pre-aggregated pick failure records into the metrics.
func (m *ClaimMetrics) MergePickSandboxFailures(failures []PickSandboxFailure) {
	for _, failure := range failures {
		m.mergePickSandboxFailure(failure)
	}
}

func (m *ClaimMetrics) mergePickSandboxFailure(failure PickSandboxFailure) {
	if failure.Count <= 0 {
		failure.Count = 1
	}
	for i := range m.PickSandboxFailures {
		if m.PickSandboxFailures[i].Key == failure.Key && m.PickSandboxFailures[i].Reason == failure.Reason {
			m.PickSandboxFailures[i].Count += failure.Count
			return
		}
	}
	m.PickSandboxFailures = append(m.PickSandboxFailures, failure)
}

func sanitizeErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	replacer := strings.NewReplacer("\n", " ", "\r", " ", "\t", " ")
	return replacer.Replace(err.Error())
}

type CloneMetrics struct {
	Retries       int
	Wait          time.Duration
	GetTemplate   time.Duration
	CreateSandbox time.Duration
	WaitReady     time.Duration
	InitRuntime   time.Duration
	SecurityToken time.Duration
	CSIMount      time.Duration
	Total         time.Duration
	LastError     error
}

func (m *CloneMetrics) String() string {
	var lastErrStr string
	if m.LastError != nil {
		lastErrStr = sanitizeErrorMessage(m.LastError)
	}
	return fmt.Sprintf("CloneMetrics{Retries: %d, Wait: %v, GetTemplate: %v, CreateSandbox: %v, WaitReady: %v, InitRuntime: %v, SecurityToken: %v, CSIMount: %v, Total: %v, LastError: %v}",
		m.Retries, m.Wait, m.GetTemplate, m.CreateSandbox, m.WaitReady, m.InitRuntime, m.SecurityToken, m.CSIMount, m.Total, lastErrStr)
}

// Merge accumulates per-attempt durations from src into m. Retries and
// LastError are maintained by the outer retry loop, not derived from per-attempt
// metrics, so they are intentionally not summed here.
func (m *CloneMetrics) Merge(src CloneMetrics) {
	m.Wait += src.Wait
	m.GetTemplate += src.GetTemplate
	m.CreateSandbox += src.CreateSandbox
	m.WaitReady += src.WaitReady
	m.InitRuntime += src.InitRuntime
	m.SecurityToken += src.SecurityToken
	m.CSIMount += src.CSIMount
	m.Total += src.Total
}
