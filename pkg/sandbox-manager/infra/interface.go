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
	"errors"
	"io"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/sandboxroute"
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

// SandboxRouteEvent carries one Sandbox observation or deletion. Normal
// deletions include their Kubernetes resource version; an empty deletion
// resource version is reserved for DeletedFinalStateUnknown.
type SandboxRouteEvent struct {
	Sandbox Sandbox
	Delete  *sandboxroute.Route
}

// SandboxRouteEventHandler consumes one neutral Sandbox informer event.
type SandboxRouteEventHandler func(context.Context, SandboxRouteEvent)

// SandboxRouteSource hides backend-specific informer registration. Sources
// emit only Sandboxes within their configured observation scope, leaving
// projection and route mutation in Manager. A subscription lives for the
// process lifetime.
type SandboxRouteSource interface {
	Subscribe(context.Context, SandboxRouteEventHandler) error
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

// IssueTrafficAccessTokenOptions identifies a Sandbox and carries the
// manager-resolved issuance policy. Validate is invoked against both fresh
// observations surrounding the external provider call.
type IssueTrafficAccessTokenOptions struct {
	Namespace    string
	SandboxID    string
	TokenOptions identity.TokenOptions
	Validate     func(Sandbox) error
}

// TrafficAccessToken is a transient token returned by an issuance operation.
// It must never be persisted by an Infrastructure implementation.
type TrafficAccessToken struct {
	Token      string
	Expiration time.Time
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

type SandboxNetworkConfig struct {
	AllowOut []string
	DenyOut  []string
}

type Builder interface {
	Build() Infrastructure
}

// RouteReader exposes local route state needed to detect an informer
// cache hit that lags a previously observed routing event.
type RouteReader interface {
	LoadRoute(sandboxID string) (sandboxroute.Route, bool)
}

// ErrSandboxNotFound reports that a claimed sandbox definitively does not
// exist. It is the boundary contract of Infrastructure.GetSandbox: callers
// classify a lookup failure with errors.Is against this sentinel, so an
// implementation must wrap its own not-found error with it and must not report
// an inconclusive failure (transport error, cache outage, expired context) as
// a not-found.
var ErrSandboxNotFound = errors.New("sandbox not found")

// ErrSandboxIDAmbiguous reports that more than one claimed sandbox matches an
// ID. API layers may hide the conflicting objects behind a not-found response,
// but must not classify the lookup as an infrastructure outage.
var ErrSandboxIDAmbiguous = errors.New("sandbox ID is ambiguous")

type Infrastructure interface {
	Run(ctx context.Context) error // Starts the infrastructure
	Stop(ctx context.Context)      // Stops the infrastructure
	HasTemplate(ctx context.Context, opts HasTemplateOptions) bool
	HasCheckpoint(ctx context.Context, opts HasCheckpointOptions) bool
	GetCache() cache.Provider // Get the CacheProvider for the infra
	GetSandboxRouteSource() SandboxRouteSource
	LoadDebugInfo() map[string]any
	SelectSandboxes(ctx context.Context, opts SelectSandboxesOptions) ([]Sandbox, error)
	// GetSandbox looks up a claimed sandbox. Implementations may poll or fall
	// back while ctx is live; callers must pass a context with a deadline.
	// A definitive miss must be reported as ErrSandboxNotFound; any other
	// failure keeps its own error so callers can tell "absent" from "unknown".
	GetSandbox(ctx context.Context, opts GetSandboxOptions) (Sandbox, error)
	IssueTrafficAccessToken(ctx context.Context, opts IssueTrafficAccessTokenOptions) (TrafficAccessToken, error)
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
	GetIP() string
	GetState() (string, string) // Get Sandbox State (pending, running, paused, killing, etc.)
	// GetSandboxID returns the label-aware public Sandbox ID: the short ID from
	// the sandbox-id label when assigned, otherwise the legacy namespace--name form.
	GetSandboxID() string
	// GetRoute projects this sandbox into its gateway route; it is a pure
	// delegate to sandboxroute.RouteFromSandbox.
	GetRoute() (sandboxroute.Route, error)
	GetTemplate() string          // Get the template name of the Sandbox
	GetResource() SandboxResource // Get the CPU / Memory requirements of the Sandbox
	// GetTrafficAccessToken returns the access token minted for accessing this
	// sandbox through the sandbox gateway. It is a transient, per-operation value
	// carried in memory (never persisted to the CR); it is empty unless the
	// sandbox opted into access-token issuance during claim or clone.
	GetTrafficAccessToken() string
	// GetTrafficAccessTokenExpiration returns the expiration time (RFC3339) of
	// the traffic access token minted during claim or clone. Like
	// GetTrafficAccessToken, it is a transient, per-operation value carried in
	// memory; it is empty unless the sandbox opted into access-token issuance.
	GetTrafficAccessTokenExpiration() string
	SetImage(image string)
	GetImage() string
	SetPodLabels(labels map[string]string)
	GetPodLabels() map[string]string
	SetPodAnnotations(annotations map[string]string)
	GetPodAnnotations() map[string]string
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
	CreateNetworkPolicy(ctx context.Context, network SandboxNetworkConfig) error // Create TrafficPolicy CR for the sandbox
	UpdateNetworkPolicy(ctx context.Context, network SandboxNetworkConfig) error // Update (replace) existing TrafficPolicy CR with new config
	SelectNetworkPolicy(ctx context.Context) (*SandboxNetworkConfig, error)      // Query current TrafficPolicy CR and return the effective config
}

// MergePodLabels merges the given labels into the sandbox's pod template labels.
// Existing labels with the same key are overwritten. The sandbox's pod template
// labels map is initialized if necessary; creating a missing pod template remains
// the responsibility of the Sandbox implementation.
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

// MergePodAnnotations merges the given annotations into the sandbox's pod
// template annotations. Existing annotations with the same key are overwritten.
// The annotations map is initialized if necessary; creating a missing pod
// template remains the responsibility of the Sandbox implementation.
func MergePodAnnotations(sbx Sandbox, annotations map[string]string) {
	if len(annotations) == 0 {
		return
	}
	existing := sbx.GetPodAnnotations()
	if existing == nil {
		existing = make(map[string]string, len(annotations))
	}
	for k, v := range annotations {
		existing[k] = v
	}
	sbx.SetPodAnnotations(existing)
}

type CheckpointInfo struct {
	Name              string
	Namespace         string
	Phase             string
	SandboxID         string
	CheckpointID      string
	CreationTimestamp string
}
