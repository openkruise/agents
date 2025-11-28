package infra

import (
	"context"
	"net/http"
	"time"

	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type SandboxSelectorOptions struct {
	TemplateName  string
	WantPaused    bool
	WantRunning   bool
	WantAvailable bool
	Labels        map[string]string
}

type SandboxResource struct {
	CPUMilli int64
	MemoryMB int64
}

type Infrastructure interface {
	Run(ctx context.Context) error                                 // Starts the infrastructure
	Stop()                                                         // Stops the infrastructure
	GetPoolByObject(sbx metav1.Object) (pool SandboxPool, ok bool) // Get the SandboxPool for the given object
	GetPoolByTemplate(name string) (pool SandboxPool, ok bool)     // Get the SandboxPool for the given template name
	NewPool(name, namespace string) SandboxPool                    // Create a new SandboxPool from a SandboxSet
	AddPool(name string, pool SandboxPool)                         // Add a SandboxPool to the pool
	LoadDebugInfo() map[string]any
	SelectSandboxes(options SandboxSelectorOptions) ([]Sandbox, error) // Select Sandboxes based on the options provided
	GetSandbox(sandboxID string) (Sandbox, error)                      // Get a Sandbox interface by its ID
	InjectTemplateMetadata() metav1.ObjectMeta                         // Inject metadata into template
}

type SandboxPool interface {
	GetName() string
	GetAnnotations() map[string]string
	// ClaimSandbox claims a Sandbox from the SandboxPool. Note: the claimed Sandbox is immutable
	ClaimSandbox(ctx context.Context, user string, modifier func(sbx Sandbox)) (Sandbox, error)
}

type Sandbox interface {
	metav1.Object                                                    // For K8s object metadata access
	Pause(ctx context.Context) error                                 // Pause a Sandbox (not available for K8sInfra)
	Resume(ctx context.Context) error                                // Resume a paused Sandbox
	GetIP() string                                                   // Get IP address of the Sandbox
	GetRouteHeader() map[string]string                               // What returned will be added to the headers of all requests to the Sandbox
	GetState() string                                                // Get Sandbox State (pending, running, paused, killing, etc.)
	SetState(ctx context.Context, state string) error                // Set the state of the Sandbox
	GetTemplate() string                                             // Get the template name of the Sandbox
	GetResource() SandboxResource                                    // Get the CPU / Memory requirements of the Sandbox
	GetOwnerUser() string                                            // Get the Owner of the Sandbox
	PatchLabels(ctx context.Context, labels map[string]string) error // Patch some labels to the Sandbox Resource
	// SaveTimer persists a timer to the Sandbox resource
	SaveTimer(ctx context.Context, afterSeconds int, event consts.EventType, triggered bool, result string) error
	// LoadTimers loads all persisted timers from the Sandbox resource and calls the callback for each timer
	LoadTimers(callback func(after time.Duration, eventType consts.EventType)) error
	Kill(ctx context.Context) error                                         // Delete the Sandbox resource
	InplaceRefresh(deepcopy bool) error                                     // Update the Sandbox resource object to the latest
	Request(r *http.Request, path string, port int) (*http.Response, error) // Make a request to the Sandbox
}
