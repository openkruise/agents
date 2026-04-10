package infra

import (
	"context"
	"io"
	"net/http"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/proxy"
	cacheutils "github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr/cache/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

type Infrastructure interface {
	Run(ctx context.Context) error // Starts the infrastructure
	Stop(ctx context.Context)      // Stops the infrastructure
	HasTemplate(name string) bool
	HasCheckpoint(name string) bool
	GetCache() CacheProvider // Get the CacheProvider for the infra
	LoadDebugInfo() map[string]any
	SelectSandboxes(user string) ([]Sandbox, error)                           // Select Sandboxes based on the options provided
	GetClaimedSandbox(ctx context.Context, sandboxID string) (Sandbox, error) // Get a Sandbox interface by its ID
	SelectSucceededCheckpoints(user string) ([]CheckpointInfo, error)
	ClaimSandbox(ctx context.Context, opts ClaimSandboxOptions) (Sandbox, ClaimMetrics, error)
	CloneSandbox(ctx context.Context, opts CloneSandboxOptions) (Sandbox, CloneMetrics, error)
	DeleteCheckpoint(ctx context.Context, user string, checkpointID string) error
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
	Kill(ctx context.Context) error                                                                     // Delete the Sandbox resource
	InplaceRefresh(ctx context.Context, deepcopy bool) error                                            // Update the Sandbox resource object to the latest
	Request(ctx context.Context, method, path string, port int, body io.Reader) (*http.Response, error) // Make a request to the Sandbox
	CSIMount(ctx context.Context, driver string, request string) error                                  // request is string config for csi.NodePublishVolumeRequest
	CreateCheckpoint(ctx context.Context, opts CreateCheckpointOptions) (string, error)
}

// CacheProvider is a read-through, informer-backed local cache for all Kubernetes resources
// used by the sandbox manager. It provides low-latency lookups without hitting the API server
// on every request, and exposes async-wait primitives so callers can block until a resource
// reaches a desired state (e.g., sandbox becomes Running after Resume).
//
// All Get/List methods read exclusively from the in-process informer store; they never issue
// live API calls. Run must be called once to start the underlying informers and wait for the
// initial sync before any other method is used.
type CacheProvider interface {
	// GetPersistentVolume looks up a cluster-scoped PersistentVolume by its Kubernetes name.
	// Returns an error if the PV does not exist in the cache or the cache lookup fails.
	GetPersistentVolume(name string) (*corev1.PersistentVolume, error)

	// GetSecret looks up a namespaced Secret by namespace and name.
	// Returns an error if the Secret does not exist in the cache or the lookup fails.
	GetSecret(namespace, name string) (*corev1.Secret, error)

	// GetConfigmap looks up a namespaced ConfigMap by namespace and name.
	// Unlike other Get methods, it returns (nil, nil) when the ConfigMap does not exist,
	// so callers must perform a nil check on the returned object before using it.
	GetConfigmap(namespace, name string) (*corev1.ConfigMap, error)

	// GetClaimedSandbox retrieves the Sandbox CRD object for an already-claimed sandbox,
	// identified by its logical sandbox ID (the value of the sandbox-id label/annotation),
	// not the Kubernetes resource name.
	// Returns an error if no sandbox with that ID is found, or if multiple sandboxes share
	// the same ID (which indicates a data inconsistency).
	GetClaimedSandbox(sandboxID string) (*agentsv1alpha1.Sandbox, error)

	// GetCheckpoint retrieves the Checkpoint CRD object by its logical checkpoint ID
	// (stored in Status.CheckpointId), not the Kubernetes resource name.
	// Returns an error if the checkpoint is not found or multiple checkpoints share the same ID.
	GetCheckpoint(checkpointID string) (*agentsv1alpha1.Checkpoint, error)

	// GetSandboxSet retrieves the SandboxSet CRD object by its Kubernetes resource name.
	// A SandboxSet represents a pool of pre-warmed, idle sandboxes backed by a specific template.
	// Returns an error if no SandboxSet with that name exists in the cache.
	GetSandboxSet(name string) (*agentsv1alpha1.SandboxSet, error)

	// GetSandboxTemplate retrieves the SandboxTemplate CRD object by namespace and name.
	// A SandboxTemplate holds a reusable pod spec (image, resources, volumes, etc.) referenced
	// by Sandboxes and SandboxSets via TemplateRef.
	// Returns an error if the template is not found in the cache.
	GetSandboxTemplate(namespace, name string) (*agentsv1alpha1.SandboxTemplate, error)

	// ListSandboxWithUser returns all Sandbox CRD objects owned by the given user.
	// Ownership is determined by the AnnotationOwner annotation on the Sandbox resource.
	// The returned slice may be empty if the user has no sandboxes.
	// Used to enumerate a user's active sandbox instances for listing or quota enforcement.
	ListSandboxWithUser(user string) ([]*agentsv1alpha1.Sandbox, error)

	// ListCheckpointsWithUser returns all Checkpoint CRD objects owned by the given user.
	// Ownership is determined by the AnnotationOwner annotation on the Checkpoint resource.
	// The returned slice may be empty if the user has no checkpoints.
	ListCheckpointsWithUser(user string) ([]*agentsv1alpha1.Checkpoint, error)

	// ListSandboxesInPool returns all Sandbox CRD objects that belong to the pool identified
	// by the given template name. Only sandboxes in Available state (or Creating state when
	// controlled by a SandboxSet) are indexed under a pool, so this method effectively returns
	// the set of idle, claimable sandboxes for a given template.
	// Concurrent calls with the same pool name are deduplicated via singleflight.
	ListSandboxesInPool(pool string) ([]*agentsv1alpha1.Sandbox, error)

	// ListAllSandboxes returns every Sandbox CRD object currently held in the informer store,
	// regardless of state, owner, or template. The returned slice is a snapshot at call time.
	// Used for global views such as metrics collection, debug endpoints, or bulk reconciliation.
	ListAllSandboxes() []*agentsv1alpha1.Sandbox
	ListSandboxSets(namespace string) ([]*agentsv1alpha1.SandboxSet, error)

	// WaitForSandboxSatisfied blocks until the given Sandbox satisfies the condition defined by
	// satisfiedFunc, or until the timeout elapses (or ctx is canceled).
	// The action parameter (Resume, Pause, WaitReady, Checkpoint) identifies the in-progress
	// operation; only one action can be waited on per sandbox at a time — a conflict returns an
	// error immediately.
	// The method registers an informer-driven hook so it is notified as soon as the sandbox
	// transitions rather than polling, minimizing latency. On timeout or context cancellation it
	// performs a final "double-check" against the latest cached state before returning an error.
	// Used after mutating a sandbox (e.g., issuing a Resume/Pause API call) to synchronously
	// wait for the async Kubernetes reconciliation to complete within an SLA.
	WaitForSandboxSatisfied(ctx context.Context, sbx *agentsv1alpha1.Sandbox, action cacheutils.WaitAction,
		satisfiedFunc cacheutils.CheckFunc[*agentsv1alpha1.Sandbox], timeout time.Duration) error

	// WaitForCheckpointSatisfied blocks until the given Checkpoint satisfies the condition
	// defined by satisfiedFunc, using the same informer-driven mechanism as WaitForSandboxSatisfied.
	// On success, it returns the latest version of the Checkpoint object from the cache, allowing
	// callers to read the final checkpoint ID or phase without an extra lookup.
	// Returns (nil, error) if the timeout elapses, ctx is canceled, or the condition errors out.
	// Used after issuing a CreateCheckpoint call to wait for the checkpoint operation to complete
	// and retrieve the resulting checkpoint ID.
	WaitForCheckpointSatisfied(ctx context.Context, checkpoint *agentsv1alpha1.Checkpoint, action cacheutils.WaitAction,
		satisfiedFunc cacheutils.CheckFunc[*agentsv1alpha1.Checkpoint], timeout time.Duration) (*agentsv1alpha1.Checkpoint, error)

	// Run must be called once before any other CacheProvider method is invoked.
	// Returns an error if any informer fails to start or if the cache sync times out.
	Run(ctx context.Context) error

	// Stop shuts down all underlying informers and releases associated resources.
	// After Stop returns, no further cache lookups or wait operations should be performed.
	Stop(ctx context.Context)

	// GetClient returns the underlying controller-runtime client.Client for direct
	// Kubernetes API operations. This should be used sparingly, preferring the
	// cache-backed methods above for read operations to avoid API server load.
	// The returned client is the same instance used internally by the CacheProvider.
	GetClient() client.Client

	// GetAPIReader returns a client.Reader that bypasses the cache and reads
	// directly from the API server. Use this when you need the latest state
	// and can tolerate the additional latency of an API server round-trip.
	// Prefer GetClient() for most operations; use this only when cache staleness
	// is unacceptable (e.g., validating critical state transitions).
	GetAPIReader() client.Reader
}

type CheckpointInfo struct {
	Name              string
	Namespace         string
	Phase             string
	SandboxID         string
	CheckpointID      string
	CreationTimestamp string
}
