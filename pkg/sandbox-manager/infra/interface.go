package infra

import (
	"context"
	"io"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/proxy"
)

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
	Run(ctx context.Context) error // Starts the infrastructure
	Stop()                         // Stops the infrastructure
	HasTemplate(name string) bool
	GetCache() CacheProvider // Get the CacheProvider for the infra
	LoadDebugInfo() map[string]any
	SelectSandboxes(user string, limit int, filter func(sandbox Sandbox) bool) ([]Sandbox, error) // Select Sandboxes based on the options provided
	GetSandbox(ctx context.Context, sandboxID string) (Sandbox, error)                            // Get a Sandbox interface by its ID
	ClaimSandbox(ctx context.Context, opts ClaimSandboxOptions) (Sandbox, ClaimMetrics, error)
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
	Kill(ctx context.Context) error                                                                     // Delete the Sandbox resource
	InplaceRefresh(ctx context.Context, deepcopy bool) error                                            // Update the Sandbox resource object to the latest
	Request(ctx context.Context, method, path string, port int, body io.Reader) (*http.Response, error) // Make a request to the Sandbox
	CSIMount(ctx context.Context, driver string, request string) error                                  // request is string config for csi.NodePublishVolumeRequest
	GetRuntimeURL() string
	GetAccessToken() string
}
type CacheProvider interface {
	GetPersistentVolume(name string) (*corev1.PersistentVolume, error)
	GetSecret(namespace, name string) (*corev1.Secret, error)
	GetSandbox(sandboxID string) (*agentsv1alpha1.Sandbox, error)
	ListSandboxWithUser(user string) ([]*agentsv1alpha1.Sandbox, error)
	ListAvailableSandboxes(pool string) ([]*agentsv1alpha1.Sandbox, error)
}
