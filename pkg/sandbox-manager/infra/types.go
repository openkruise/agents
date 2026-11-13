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

	claim "github.com/openkruise/agents/pkg/lifecycle/claim"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/utils/runtime"
	"github.com/openkruise/agents/pkg/utils/timeout"
)

// The neutral claim request/metrics/admission types are defined once in
// pkg/lifecycle/claim so the claim engine and the independent Sandbox
// controller can share them without depending on sandbox-manager. These aliases
// keep the established infra.* names for sandbox-manager callers. Note that
// ClaimSandboxOptions.Modifier therefore takes a claim.Handle (the narrow
// mutation seam) rather than the full infra.Sandbox interface.
type (
	SandboxAdmission      = claim.SandboxAdmission
	ClaimSandboxOptions   = claim.Options
	ClaimMetrics          = claim.Metrics
	PickSandboxFailure    = claim.PickSandboxFailure
	LockType              = claim.LockType
	SandboxCreationResult = claim.SandboxCreationResult
)

const (
	SandboxAdmissionReleaseTimeout = claim.SandboxAdmissionReleaseTimeout
	LockTypeCreate                 = claim.LockTypeCreate
	LockTypeUpdate                 = claim.LockTypeUpdate
	LockTypeSpeculate              = claim.LockTypeSpeculate
)

type SaveTimeoutOptions struct {
	Timeout          timeout.Options
	ExtraAnnotations map[string]string
}

type CloneSandboxOptions struct {
	Namespace          string                       `json:"namespace,omitempty"`
	User               string                       `json:"user"`
	CheckPointID       string                       `json:"checkPointID"`
	LockString         string                       `json:"lockString"`
	Admission          *SandboxAdmission            `json:"-"`
	WaitReadyTimeout   time.Duration                `json:"waitReadyTimeout"`
	CloneTimeout       time.Duration                `json:"cloneTimeout"`
	CSIMount           *config.CSIMountOptions      `json:"CSIMount"`
	Modifier           func(sbx claim.Handle) error `json:"-"`
	CreateLimiter      *rate.Limiter                `json:"-"`
	SkipWaitCheckpoint bool                         `json:"skipWaitCheckpoint"`
	// MemoryPersistence overrides memory persistence for this Sandbox; nil inherits its current setting.
	MemoryPersistence *bool `json:"memoryPersistence,omitempty"`
	// See ReserveFailedSandboxFor on ClaimSandboxOptions.
	ReserveFailedSandboxFor *time.Duration `json:"reserveFailedSandboxFor"`
	// Name sets ObjectMeta.Name on the cloned sandbox (exact name).
	// Mutually exclusive with GenerateName.
	Name string `json:"name,omitempty"`
	// GenerateName sets ObjectMeta.GenerateName on the cloned sandbox (prefix).
	// Mutually exclusive with Name.
	GenerateName string `json:"generateName,omitempty"`
	// RuntimeTLSBundle is the client TLS bundle used to reach a TLS-capable
	// agent-runtime during clone post-processing (re-init handshake and CSI
	// mounts). Unconditionally overwritten by the Infrastructure implementation
	// like CreateLimiter, so a value set by an API caller is discarded; nil
	// means this process is not configured for runtime TLS.
	RuntimeTLSBundle *runtime.TLSBundle `json:"-"`
	// TrafficAccessTokenValidity is Manager-owned issuance policy. API callers
	// cannot override it because SandboxManager replaces it before delegation.
	TrafficAccessTokenValidity time.Duration `json:"-"`
	// ControlPlaneSource is the control plane that drives this clone. See the
	// field of the same name on ClaimSandboxOptions.
	ControlPlaneSource string `json:"controlPlaneSource,omitempty"`
}

type CreateCheckpointOptions struct {
	KeepRunning        *bool         `json:"keepRunning,omitempty"`
	TTL                *string       `json:"TTL,omitempty"`
	PersistentContents []string      `json:"persistentMemory"`
	WaitSuccessTimeout time.Duration `json:"waitSuccessTimeout"`
}

func sanitizeErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	replacer := strings.NewReplacer("\n", " ", "\r", " ", "\t", " ")
	return replacer.Replace(err.Error())
}

type CloneMetrics struct {
	CreationResults []SandboxCreationResult
	Retries         int
	Wait            time.Duration
	GetTemplate     time.Duration
	CreateSandbox   time.Duration
	WaitReady       time.Duration
	InitRuntime     time.Duration
	SecurityToken   time.Duration
	TrafficToken    time.Duration
	CSIMount        time.Duration
	Total           time.Duration
	LastError       error
}

func (m *CloneMetrics) String() string {
	var lastErrStr string
	if m.LastError != nil {
		lastErrStr = sanitizeErrorMessage(m.LastError)
	}
	return fmt.Sprintf("CloneMetrics{Retries: %d, Wait: %v, GetTemplate: %v, CreateSandbox: %v, WaitReady: %v, InitRuntime: %v, SecurityToken: %v, TrafficToken: %v, CSIMount: %v, Total: %v, LastError: %v}",
		m.Retries, m.Wait, m.GetTemplate, m.CreateSandbox, m.WaitReady, m.InitRuntime, m.SecurityToken, m.TrafficToken, m.CSIMount, m.Total, lastErrStr)
}

// Merge accumulates per-attempt durations from src into m. Retries and
// LastError are maintained by the outer retry loop, not derived from per-attempt
// metrics, so they are intentionally not summed here.
func (m *CloneMetrics) Merge(src CloneMetrics) {
	m.CreationResults = append(m.CreationResults, src.CreationResults...)
	m.Wait += src.Wait
	m.GetTemplate += src.GetTemplate
	m.CreateSandbox += src.CreateSandbox
	m.WaitReady += src.WaitReady
	m.InitRuntime += src.InitRuntime
	m.SecurityToken += src.SecurityToken
	m.TrafficToken += src.TrafficToken
	m.CSIMount += src.CSIMount
	m.Total += src.Total
}
