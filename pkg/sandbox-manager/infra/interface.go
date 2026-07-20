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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/utils/timeout"
)

const bytesPerMiB = int64(1024 * 1024)

type ResourceList struct {
	CPUMilli   int64
	MemoryMB   int64
	DiskSizeMB int64
}

type SandboxResource struct {
	Requests ResourceList
	Limits   ResourceList
}

type QuotaSandboxSourceProvider interface {
	GetQuotaSandboxSource() QuotaSandboxSource
}

type QuotaSandboxSource interface {
	ListLiveQuotaSandboxesByOwner(context.Context, string) ([]QuotaSandboxSnapshot, error)
	Subscribe(context.Context, func(QuotaSandboxEvent)) (QuotaSandboxSubscription, error)
	Healthy() bool
}

type QuotaSandboxSnapshot struct {
	Owner      string
	LockString string
	Resource   SandboxResource
	Live       bool
	Running    bool
}

type QuotaSandboxEvent struct {
	Snapshot QuotaSandboxSnapshot
	Deleted  bool
}

type QuotaSandboxSubscription interface {
	Remove() error
}

func memoryBytesToFloorMiB(q resource.Quantity) int64 {
	return q.Value() / bytesPerMiB
}

func memoryBytesToCeilMiB(q resource.Quantity) int64 {
	bytes := q.Value()
	if bytes <= 0 {
		return 0
	}
	return (bytes + bytesPerMiB - 1) / bytesPerMiB
}

func calculateResourceList(
	containers []corev1.Container,
	pick func(corev1.ResourceRequirements) corev1.ResourceList,
	memoryToMiB func(resource.Quantity) int64,
) ResourceList {
	out := ResourceList{}
	for _, container := range containers {
		resources := pick(container.Resources)
		if resources == nil {
			continue
		}
		if cpu, ok := resources[corev1.ResourceCPU]; ok {
			out.CPUMilli += cpu.MilliValue()
		}
		if memory, ok := resources[corev1.ResourceMemory]; ok {
			out.MemoryMB += memoryToMiB(memory)
		}
		if disk, ok := resources[corev1.ResourceEphemeralStorage]; ok {
			out.DiskSizeMB += disk.Value() / bytesPerMiB
		}
	}
	return out
}

// CalculateResourceFromContainers sums resource requests and limits from a list of containers.
func CalculateResourceFromContainers(containers []corev1.Container) SandboxResource {
	requests := calculateResourceList(containers, func(r corev1.ResourceRequirements) corev1.ResourceList {
		return r.Requests
	}, memoryBytesToFloorMiB)
	limits := calculateResourceList(containers, func(r corev1.ResourceRequirements) corev1.ResourceList {
		return r.Limits
	}, memoryBytesToCeilMiB)
	return SandboxResource{
		Requests: requests,
		Limits:   limits,
	}
}

type TimeoutUpdateResult struct {
	Updated bool
}

type PauseOptions struct {
	Timeout          *timeout.Options
	ExtraAnnotations map[string]string
}

// ResumeOptions configures a Resume operation.
//
// Timeout, when non-nil, is written atomically with Spec.Paused=false so
// the controller's auto-pause action cannot fire on the stale PauseTime
// between Resume returning and the caller writing the real business
// timeout. Pass nil to skip the atomic write (the caller accepts that
// PauseTime may remain stale until the next write).
type ResumeOptions struct {
	Timeout *timeout.Options
}

type HasTemplateOptions struct {
	Namespace string
	Name      string
}

type HasCheckpointOptions struct {
	Namespace    string
	CheckpointID string
}

type GetSandboxOptions struct {
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

type CreateVolumeOptions struct {
	Namespace        string
	Name             string
	UserID           string
	StorageSize      resource.Quantity
	StorageClass     string
	AccessMode       string
	WaitBoundTimeout time.Duration
}

type ListVolumesOptions struct {
	Namespace string
	UserID    string
}

type GetVolumeOptions struct {
	Namespace string
	VolumeID  string
	UserID    string
}

type DeleteVolumeOptions struct {
	Namespace string
	VolumeID  string
	UserID    string
}

type VolumeInfo struct {
	Name     string `json:"name,omitempty"`
	VolumeID string `json:"volumeID,omitempty"`
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
	GetSandbox(ctx context.Context, opts GetSandboxOptions) (Sandbox, error)
	SelectSucceededCheckpoints(ctx context.Context, opts SelectSucceededCheckpointsOptions) ([]CheckpointInfo, error)
	ClaimSandbox(ctx context.Context, opts ClaimSandboxOptions) (Sandbox, ClaimMetrics, error)
	CloneSandbox(ctx context.Context, opts CloneSandboxOptions) (Sandbox, CloneMetrics, error)
	DeleteCheckpoint(ctx context.Context, opts DeleteCheckpointOptions) error
	CreateVolume(ctx context.Context, opts CreateVolumeOptions) (*VolumeInfo, error)
	ListVolumes(ctx context.Context, opts ListVolumesOptions) ([]*VolumeInfo, error)
	GetVolume(ctx context.Context, opts GetVolumeOptions) (*VolumeInfo, error)
	DeleteVolume(ctx context.Context, opts DeleteVolumeOptions) error
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
	// GetTrafficAccessToken returns the access token minted for accessing this
	// sandbox through the sandbox gateway. It is a transient, per-claim value
	// carried in memory (never persisted to the CR); it is empty unless the
	// sandbox opted into access-token issuance during claim.
	GetTrafficAccessToken() string
	// GetTrafficAccessTokenExpiration returns the expiration time (RFC3339) of
	// the traffic access token minted during claim. Like GetTrafficAccessToken
	// it is a transient, per-claim value carried in memory; it is empty unless
	// the sandbox opted into access-token issuance during claim.
	GetTrafficAccessTokenExpiration() string
	SetImage(image string)
	GetImage() string
	SetPodLabels(labels map[string]string)
	GetPodLabels() map[string]string
	SetTimeout(opts timeout.Options)
	SaveTimeoutWithPolicy(ctx context.Context, opts SaveTimeoutOptions, policy timeout.UpdatePolicy) (TimeoutUpdateResult, error)
	GetTimeout() timeout.Options
	GetClaimTime() (time.Time, error)
	Kill(ctx context.Context) error                                                                     // Delete the Sandbox resource
	TriggerRecycle(ctx context.Context) error                                                           // Trigger sandbox recycle flow instead of deletion
	IsRecycleEnabled() bool                                                                             // Whether the sandbox supports recycle
	Phase() string                                                                                      // Get the current sandbox phase
	InplaceRefresh(ctx context.Context, deepcopy bool) error                                            // Update the Sandbox resource object to the latest
	Request(ctx context.Context, method, path string, port int, body io.Reader) (*http.Response, error) // Make a request to the Sandbox
	CSIMount(ctx context.Context, driver string, request string) error                                  // request is string config for csi.NodePublishVolumeRequest
	CreateCheckpoint(ctx context.Context, opts CreateCheckpointOptions) (string, error)
}

// MergePodLabels merges the given labels into the sandbox's pod template labels.
// Existing labels with the same key are overwritten. The sandbox's pod template
// is initialized if necessary.
func MergePodLabels(sbx Sandbox, labels map[string]string) {
	if len(labels) == 0 {
		return
	}
	existing := sbx.GetPodLabels()
	if existing == nil {
		existing = make(map[string]string, len(labels))
	}
	for k, v := range labels {
		existing[k] = v
	}
	sbx.SetPodLabels(existing)
}

type CheckpointInfo struct {
	Name              string
	Namespace         string
	Phase             string
	SandboxID         string
	CheckpointID      string
	CreationTimestamp string
}
