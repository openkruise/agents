package infra

import (
	"context"
	"net/http"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/pkg/proxy"
)

type ClaimSandboxOptions struct {
	Modifier func(sandbox Sandbox)
	Image    string
}

type SandboxResource struct {
	CPUMilli   int64
	MemoryMB   int64
	DiskSizeMB int64
}

type TimeoutOptions struct {
	ShutdownTime time.Time
	PauseTime    time.Time
}

type PauseOptions struct {
	Timeout *TimeoutOptions
}

type Infrastructure interface {
	Run(ctx context.Context) error                                             // Starts the infrastructure
	Stop()                                                                     // Stops the infrastructure
	GetPoolByObject(sbx metav1.Object) (pool SandboxPool, ok bool)             // Get the SandboxPool for the given object
	GetPoolByTemplate(name string) (pool SandboxPool, ok bool)                 // Get the SandboxPool for the given template name
	NewPool(name, namespace string, annotations map[string]string) SandboxPool // Create a new SandboxPool from a SandboxSet
	AddPool(name string, pool SandboxPool)                                     // Add a SandboxPool to the pool
	LoadDebugInfo() map[string]any
	SelectSandboxes(user string, limit int, filter func(sandbox Sandbox) bool) ([]Sandbox, error) // Select Sandboxes based on the options provided
	GetSandbox(ctx context.Context, sandboxID string) (Sandbox, error)                            // Get a Sandbox interface by its ID
}

type SandboxPool interface {
	GetName() string
	GetAnnotations() map[string]string
	// ClaimSandbox claims a Sandbox from the SandboxPool. Note: the claimed Sandbox is immutable
	ClaimSandbox(ctx context.Context, user string, maxCandidates int, opts ClaimSandboxOptions) (Sandbox, error)
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
	SetTimeout(opts TimeoutOptions)
	SaveTimeout(ctx context.Context, opts TimeoutOptions) error
	GetTimeout() TimeoutOptions
	GetClaimTime() (time.Time, error)
	Kill(ctx context.Context) error                                         // Delete the Sandbox resource
	InplaceRefresh(ctx context.Context, deepcopy bool) error                // Update the Sandbox resource object to the latest
	Request(r *http.Request, path string, port int) (*http.Response, error) // Make a request to the Sandbox
	CSIMount(ctx context.Context, driver string, request string) error      // request is base64 encoded csi.NodePublishVolumeRequest
}
