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
	"io"
	"net/http"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/proxy"
)

type SandboxResource struct {
	CPUMilli   int64
	MemoryMB   int64
	DiskSizeMB int64
}

// TimeoutOptions is the time when Sandbox will be shut down or paused. Zero means never.
type TimeoutOptions struct {
	ShutdownTime time.Time
	PauseTime    time.Time
}

type PauseOptions struct {
	Timeout *TimeoutOptions
}

type Builder interface {
	Build() Infrastructure
}

// SandboxEventHandler defines the interface for handling sandbox lifecycle events
type SandboxEventHandler interface {
	OnSandboxAdd(sessionID, sandboxID, userID, accessToken, state string)
	OnSandboxDelete(sessionID string)
	OnSandboxUpdate(sessionID, sandboxID, userID, accessToken, state string)
}

type Infrastructure interface {
	Run(ctx context.Context) error // Starts the infrastructure
	Stop(ctx context.Context)      // Stops the infrastructure
	HasTemplate(ctx context.Context, name string) bool
	HasCheckpoint(ctx context.Context, name string) bool
	GetCache() cache.Provider // Get the CacheProvider for the infra
	LoadDebugInfo() map[string]any
	SelectSandboxes(ctx context.Context, user string) ([]Sandbox, error)      // Select Sandboxes based on the options provided
	GetClaimedSandbox(ctx context.Context, sandboxID string) (Sandbox, error) // Get a Sandbox interface by its ID
	SelectSucceededCheckpoints(ctx context.Context, user string) ([]CheckpointInfo, error)
	ClaimSandbox(ctx context.Context, opts ClaimSandboxOptions) (Sandbox, ClaimMetrics, error)
	CloneSandbox(ctx context.Context, opts CloneSandboxOptions) (Sandbox, CloneMetrics, error)
	DeleteCheckpoint(ctx context.Context, user string, checkpointID string) error
	SetSandboxEventHandler(handler SandboxEventHandler)
}

type Sandbox interface {
	metav1.Object                                       // For K8s object metadata access
	Pause(ctx context.Context, opts PauseOptions) error // Pause a Sandbox
	Resume(ctx context.Context) error                   // Resume a paused Sandbox
	GetSandboxID() string
	GetRoute() proxy.Route
	GetState() (string, string)   // Get Sandbox State (pending, running, paused, killing, etc.)
	GetTemplate() string          // Get the template name of the Sandbox
	GetResource() SandboxResource // Get the CPU / Memory requirements of the Sandbox
	SetImage(image string)
	GetImage() string
	SetPodLabels(labels map[string]string)
	GetPodLabels() map[string]string
	SetTimeout(opts TimeoutOptions)
	SaveTimeout(ctx context.Context, opts TimeoutOptions) error
	GetTimeout() TimeoutOptions
	GetClaimTime() (time.Time, error)
	Kill(ctx context.Context) error                                                                                          // Delete the Sandbox resource
	InplaceRefresh(ctx context.Context, deepcopy bool) error                                                                 // Update the Sandbox resource object to the latest
	Request(ctx context.Context, method, path string, port int, body io.Reader, headers http.Header) (*http.Response, error) // Make a request to the Sandbox
	CSIMount(ctx context.Context, driver string, request string) error                                                       // request is string config for csi.NodePublishVolumeRequest
	CreateCheckpoint(ctx context.Context, opts CreateCheckpointOptions) (string, error)
}

type CheckpointInfo struct {
	Name              string
	Namespace         string
	Phase             string
	SandboxID         string
	CheckpointID      string
	CreationTimestamp string
}
