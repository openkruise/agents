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

package consts

import (
	"time"

	"github.com/openkruise/agents/pkg/lifecycle/claim"
)

const (
	OwnerManagerScaleDown = "__manager_scale_down"

	// Neutral pool/claim defaults now live in pkg/lifecycle/claim (shared with
	// the SandboxClaim controller); these aliases are kept for manager-internal
	// callers during the transition.
	DefaultPoolingCandidateCounts = claim.DefaultPoolingCandidateCounts
	DefaultWaitReadyTimeout       = claim.DefaultWaitReadyTimeout
	DefaultWaitCheckpointTimeout  = 60 * time.Second
	DefaultWaitBoundPVCTimeout    = 60 * time.Second
	DefaultClaimWorkers           = claim.DefaultClaimWorkers
	DefaultCreateQPS              = claim.DefaultCreateQPS
)

const (
	ExtProcPort               = 9002
	DefaultExtProcConcurrency = 1000
	ShutdownTimeout           = 90 * time.Second
)

const (
	DefaultQuotaRedisOperationTimeout = 50 * time.Millisecond
	DefaultQuotaRedisBreakerN         = 3
	DefaultQuotaRedisBreakerD         = 30 * time.Second
	DefaultQuotaAntiDriftInterval     = 5 * time.Minute
	DefaultQuotaAntiDriftGrace        = 10 * time.Minute
)

// Sentinel values for ReserveFailedSandboxFor. The neutral definitions live in
// pkg/lifecycle/claim; these aliases are kept for manager-internal callers.
const (
	// ReserveFailedSandboxNever instructs the backend to delete a failed
	// sandbox immediately instead of keeping it around.
	ReserveFailedSandboxNever = claim.ReserveFailedSandboxNever
	// ReserveFailedSandboxForever instructs the backend to keep a failed
	// sandbox indefinitely for debugging.
	ReserveFailedSandboxForever = claim.ReserveFailedSandboxForever
)
