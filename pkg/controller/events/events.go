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

// Package events defines Kubernetes Event Reason constants used across all controllers.
package events

// Sandbox lifecycle event reasons represent state transitions and operations on individual Sandbox resources.
const (
	SandboxPodCreated      = "SandboxPodCreated"
	SandboxPodCreateFailed = "SandboxPodCreateFailed"
	SandboxReady           = "SandboxReady"
	SandboxPausing         = "SandboxPausing"
	SandboxPaused          = "SandboxPaused"
	SandboxResuming        = "SandboxResuming"
	SandboxResumed         = "SandboxResumed"
	SandboxUpgrading       = "SandboxUpgrading"
	SandboxUpgraded        = "SandboxUpgraded"
	SandboxFailed          = "SandboxFailed"
	SandboxDeleting        = "SandboxDeleting"
)

// SandboxSet lifecycle event reasons represent operations on SandboxSet resources including scaling and rolling updates.
const (
	SandboxCreated                   = "SandboxCreated"
	CreateSandboxFailed              = "CreateSandboxFailed"
	SandboxScaledDown                = "SandboxScaledDown"
	FailedSandboxDeleted             = "FailedSandboxDeleted"
	SandboxSetScalingUp              = "SandboxSetScalingUp"
	SandboxSetScalingDown            = "SandboxSetScalingDown"
	SandboxSetRollingUpdate          = "SandboxSetRollingUpdate"
	SandboxSetRollingUpdateCompleted = "SandboxSetRollingUpdateCompleted"
)

// SandboxClaim lifecycle event reasons represent claim binding, completion, and error conditions.
const (
	SandboxClaimBinding   = "SandboxClaimBinding"
	SandboxClaimCompleted = "ClaimCompleted"
	SandboxClaimed        = "SandboxClaimed"
	NoAvailableSandboxes  = "NoAvailableSandboxes"
	SandboxClaimTTLDelete = "SandboxClaimTTLDelete"
	FeatureGateDisabled   = "FeatureGateDisabled"
	UnknownPhase          = "UnknownPhase"
)

// SandboxUpdateOps lifecycle event reasons represent in-place update operation progress.
const (
	UpdateOpsStarted     = "UpdateOpsStarted"
	UpdateOpsProgressing = "UpdateOpsProgressing"
	UpdateOpsCompleted   = "UpdateOpsCompleted"
	UpdateOpsFailed      = "UpdateOpsFailed"
)
