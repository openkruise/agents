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
	"github.com/openkruise/agents/pkg/utils/timeout"
)

type SandboxResource struct {
	CPUMilli   int64
	MemoryMB   int64
	DiskSizeMB int64
}

type TimeoutUpdateResult struct {
	Updated bool
}

type PauseOptions struct {
	Timeout *timeout.Options
}

// ResumeOptions reserves the type for future extensions.
type ResumeOptions struct{}

type HasTemplateOptions struct {
	Namespace string
	Name      string
}

type HasCheckpointOptions struct {
	Namespace    string
	CheckpointID string
}

type GetClaimedSandboxOptions struct {
	Namespace string
	SandboxID string
}

type SelectSandboxesOptions struct {
	Namespace string
	User      string
}

type SelectSucceededCheckpointsOptions struct {
	Namespace string
	User      string
}

type DeleteCheckpointOptions struct {
	Namespace    string
	CheckpointID string
	// User requesting deletion. If non-empty, infra will verify
	// the checkpoint's AnnotationOwner matches before proceeding with deletion.
	User string
}

type Builder interface {
	Build() Infrastructure
}

type Infrastructure interface {
	Run(ctx context.Context) error // Starts the infrastructure
	Stop(ctx context.Context)      // Stops the infrastructure
	HasTemplate(ctx context.Context, opts HasTemplateOptions) bool
	HasCheckpoint(ctx context.Context, opts HasCheckpointOptions) bool
	GetCache() cache.Provider // Get the CacheProvider for the infra
	LoadDebugInfo() map[string]any
	SelectSandboxes(ctx context.Context, opts SelectSandboxesOptions) ([]Sandbox, error)
	GetClaimedSandbox(ctx context.Context, opts GetClaimedSandboxOptions) (Sandbox, error)
	SelectSucceededCheckpoints(ctx context.Context, opts SelectSucceededCheckpointsOptions) ([]CheckpointInfo, error)
	ClaimSandbox(ctx context.Context, opts ClaimSandboxOptions) (Sandbox, ClaimMetrics, error)
	CloneSandbox(ctx context.Context, opts CloneSandboxOptions) (Sandbox, CloneMetrics, error)
	DeleteCheckpoint(ctx context.Context, opts DeleteCheckpointOptions) error
}

type Sandbox interface {
	metav1.Object                                         // For K8s object metadata access
	Pause(ctx context.Context, opts PauseOptions) error   // Pause a Sandbox
	Resume(ctx context.Context, opts ResumeOptions) error // Resume a paused Sandbox
	GetSandboxID() string
	GetRoute() proxy.Route
	GetState() (string, string)   // Get Sandbox State (pending, running, paused, killing, etc.)
	GetTemplate() string          // Get the template name of the Sandbox
	GetResource() SandboxResource // Get the CPU / Memory requirements of the Sandbox
	SetImage(image string)
	GetImage() string
	SetPodLabels(labels map[string]string)
	GetPodLabels() map[string]string
	SetTimeout(opts timeout.Options)
	SaveTimeoutWithPolicy(ctx context.Context, opts timeout.Options, policy timeout.UpdatePolicy) (TimeoutUpdateResult, error)
	GetTimeout() timeout.Options
	GetClaimTime() (time.Time, error)
	Kill(ctx context.Context) error                                                                     // Delete the Sandbox resource
	InplaceRefresh(ctx context.Context, deepcopy bool) error                                            // Update the Sandbox resource object to the latest
	Request(ctx context.Context, method, path string, port int, body io.Reader) (*http.Response, error) // Make a request to the Sandbox
	CSIMount(ctx context.Context, driver string, request string) error                                  // request is string config for csi.NodePublishVolumeRequest
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
