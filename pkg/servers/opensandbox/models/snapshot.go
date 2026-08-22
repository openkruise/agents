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

package models

// SnapshotState is the OpenSandbox snapshot lifecycle state.
type SnapshotState string

const (
	SnapshotStateCreating SnapshotState = "Creating"
	SnapshotStateDeleting SnapshotState = "Deleting"
	SnapshotStateReady    SnapshotState = "Ready"
	SnapshotStateFailed   SnapshotState = "Failed"
)

// CreateSnapshotRequest is the POST /v1/sandboxes/{sandboxId}/snapshots request body.
type CreateSnapshotRequest struct {
	Name string `json:"name,omitempty"`
}

// SnapshotStatus is the nested status object on Snapshot.
type SnapshotStatus struct {
	State            SnapshotState `json:"state"`
	Reason           string        `json:"reason,omitempty"`
	Message          string        `json:"message,omitempty"`
	LastTransitionAt string        `json:"lastTransitionAt,omitempty"`
}

// Snapshot is the OpenSandbox snapshot representation.
type Snapshot struct {
	ID        string         `json:"id"`
	SandboxID string         `json:"sandboxId"`
	Name      string         `json:"name,omitempty"`
	Status    SnapshotStatus `json:"status"`
	CreatedAt string         `json:"createdAt"`
}

// ListSnapshotsResponse is the GET /v1/snapshots response body.
type ListSnapshotsResponse struct {
	Items      []*Snapshot    `json:"items"`
	Pagination PaginationInfo `json:"pagination"`
}
