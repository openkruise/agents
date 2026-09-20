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

package v1alpha1

const (
	// LabelSandboxPool identifies which SandboxSet generated the sandbox.
	// Used by the recycle flow to find the origin SandboxSet.
	LabelSandboxPool = InternalPrefix + "sandbox-pool"
	// LabelSandboxTemplate identifies which template generated the sandbox
	LabelSandboxTemplate = InternalPrefix + "sandbox-template"
	// LabelSandboxIsClaimed indicates whether the sandbox has been claimed by user
	LabelSandboxIsClaimed = InternalPrefix + "sandbox-claimed"
	// LabelSandboxClaimName indicates the name of the SandboxClaim that claimed this sandbox
	LabelSandboxClaimName = InternalPrefix + "claim-name"
	LabelTemplateHash     = InternalPrefix + "template-hash"
	// LabelSandboxReservedFailed marks a failed sandbox retained for debugging.
	LabelSandboxReservedFailed = InternalPrefix + "reserved-failed-sandbox"
	// LabelSandboxName is the label key used by TrafficPolicy Spec.Selector to select the sandbox pod.
	LabelSandboxName = InternalPrefix + "sandbox-name"
	// LabelAllowInternetAccess indicates whether the sandbox is allowed internet access.
	// Default is "true"; set to "false" when the user explicitly disables internet access.
	// GlobalTrafficPolicy uses this label to select pods and apply egress rules.
	LabelAllowInternetAccess = InternalPrefix + "allow-internet-access"
	// LabelSandboxID stores the authoritative ID of a Sandbox when it has one.
	LabelSandboxID = InternalPrefix + "sandbox-id"

	// LabelSandboxUpdateOps marks which SandboxUpdateOps is operating on this sandbox.
	LabelSandboxUpdateOps = InternalPrefix + "update-ops"
	// LabelSandboxUpgradeFailed marks a sandbox whose upgrade has failed.
	// The controller sets it when the Upgrading condition reports a failure reason
	// and removes it once the sandbox is no longer in a failed state.
	LabelSandboxUpgradeFailed = InternalPrefix + "upgrade-failed"

	// PodLabelTemplateHash is pod template hash
	PodLabelTemplateHash = "pod-template-hash"

	// CheckpointLabelSandboxName is the checkpointed sandbox name, written only
	// when the name fits the 63-character label-value limit. It exists to keep
	// selectors written against earlier versions working; new consumers should
	// select by CheckpointLabelSandboxUID, which is always present.
	CheckpointLabelSandboxName = InternalPrefix + "sandbox-name"
	// CheckpointLabelSandboxUID is the checkpointed sandbox's metadata.uid. A UID
	// is always 36 characters of [0-9a-f-] and therefore always a valid label
	// value, so unlike CheckpointLabelSandboxName it is written on every
	// Checkpoint. For a human-readable link to the sandbox, use spec.sandboxName
	// or spec.podName, which carry no length limit.
	CheckpointLabelSandboxUID = InternalPrefix + "sandbox-uid"

	// CheckpointLabelType is the checkpoint type label key
	CheckpointLabelType = InternalPrefix + "checkpoint-type"
	// CheckpointLabelID is the checkpoint ID label key
	CheckpointLabelID = InternalPrefix + "checkpoint-id"

	True  = "true"
	False = "false"
)
