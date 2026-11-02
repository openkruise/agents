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

package claim

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/tracing"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/expectations"
	"github.com/openkruise/agents/pkg/utils/runtime"
	"github.com/openkruise/agents/pkg/utils/timeout"
)

// ModifierFunc mutates the sandbox CR and decides whether the caller should
// persist it. It returns:
//   - changed: true when the provided sandbox was modified and should be
//     written; false when no update should be issued.
//   - err:     non-nil to abort the retry loop immediately.
type ModifierFunc func(sbx *v1alpha1.Sandbox) (bool, error)

// Sandbox is the neutral, CR-backed handle the claim engine builds and mutates.
// It carries only the state the engine needs: the Sandbox CR, the cache used to
// read/write it, and the transient traffic access token minted during claim
// post-processing. Backend-specific lifecycle surface (pause/resume, network
// policy, checkpointing) lives on the concrete backend sandbox, which delegates
// its shared primitives to the free functions in this file.
type Sandbox struct {
	*v1alpha1.Sandbox
	Cache cache.Provider
	// trafficToken holds the access token response minted for accessing this
	// sandbox through the sandbox gateway. It is transient and per-operation: set
	// in memory during claim and never persisted to the CR, so InplaceRefresh /
	// RetryUpdate reassigning s.Sandbox do not clear it.
	trafficToken *identity.TokenResponse
}

// NewSandbox wraps a Sandbox CR with the cache the engine reads and writes
// through.
func NewSandbox(sbx *v1alpha1.Sandbox, c cache.Provider) *Sandbox {
	return &Sandbox{Sandbox: sbx, Cache: c}
}

// TrafficToken returns the transient traffic access token minted during claim
// post-processing, or nil when none was issued.
func (s *Sandbox) TrafficToken() *identity.TokenResponse {
	return s.trafficToken
}

// SetTrafficToken stores the transient traffic access token on the handle.
func (s *Sandbox) SetTrafficToken(token *identity.TokenResponse) {
	s.trafficToken = token
}

var _ Handle = (*Sandbox)(nil)

// InplaceRefresh reloads the sandbox from the informer cache, falling back to
// the APIReader when the cache is stale, and reassigns the wrapped CR when a
// strictly newer resourceVersion is observed.
func (s *Sandbox) InplaceRefresh(ctx context.Context, deepcopy bool) error {
	cr, err := InplaceRefresh(ctx, s.Cache, s.Sandbox, deepcopy)
	if err != nil {
		return err
	}
	s.Sandbox = cr
	return nil
}

// RefreshFunc returns a runtime.RefreshFunc that refreshes this sandbox and
// hands back the latest CR, so runtime.InitRuntime can re-read state without
// depending on this package's concrete type.
func (s *Sandbox) RefreshFunc() runtime.RefreshFunc {
	return func(ctx context.Context) (*v1alpha1.Sandbox, error) {
		if err := s.InplaceRefresh(ctx, false); err != nil {
			return nil, err
		}
		return s.Sandbox, nil
	}
}

// RetryUpdate loads the latest sandbox from the informer first, applies the
// modifier, and retries on conflict. See InplaceRefresh for the cache fallback.
func (s *Sandbox) RetryUpdate(ctx context.Context, modifier ModifierFunc) (bool, error) {
	cr, updated, err := RetryUpdate(ctx, s.Cache, s.Sandbox, modifier)
	if err != nil {
		return false, err
	}
	s.Sandbox = cr
	return updated, nil
}

// Kill deletes the sandbox CR, injecting trace context first when tracing is
// active. It is a no-op when the sandbox is already terminating.
func (s *Sandbox) Kill(ctx context.Context) error {
	return Kill(ctx, s.Cache, s.Sandbox)
}

func (s *Sandbox) SetTimeout(opts timeout.Options) { ApplyTimeout(s.Sandbox, opts) }

func (s *Sandbox) GetTimeout() timeout.Options { return timeout.GetTimeoutFromSandbox(s.Sandbox) }

func (s *Sandbox) EnableWakeOnIngressTraffic(pauseTimeout time.Duration) {
	EnableWakeOnIngressTraffic(s.Sandbox, pauseTimeout)
}

func (s *Sandbox) ClearWakeOnIngressTraffic() { ClearWakeOnIngressTraffic(s.Sandbox) }

func (s *Sandbox) GetPodLabels() map[string]string { return GetPodLabels(s.Sandbox) }

func (s *Sandbox) SetPodLabels(labels map[string]string) { SetPodLabels(s.Sandbox, labels) }

func (s *Sandbox) GetPodAnnotations() map[string]string { return GetPodAnnotations(s.Sandbox) }

func (s *Sandbox) SetPodAnnotations(annotations map[string]string) {
	SetPodAnnotations(s.Sandbox, annotations)
}

func (s *Sandbox) SetImage(image string) { SetImage(s.Sandbox, image) }

func (s *Sandbox) GetImage() string { return GetImage(s.Sandbox) }

func (s *Sandbox) GetResource() SandboxResource { return GetResource(s.Sandbox) }

// SetResources applies in-place resource resize to the first container.
func (s *Sandbox) SetResources(requests, limits corev1.ResourceList) error {
	return SetResources(s.Sandbox, requests, limits)
}

// --- Shared neutral primitives ---
//
// The concrete backend sandbox (pkg/sandbox-manager/infra/sandboxcr) keeps its
// own struct shape and method signatures but delegates its bodies here so the
// subtle cache/conflict handling has a single source of truth.

// InplaceRefresh reloads cur from the informer cache, falling back to the
// APIReader when the cache is missing or lagging, and returns the CR to adopt:
// a strictly newer object (deep-copied when requested) or cur unchanged.
func InplaceRefresh(ctx context.Context, c cache.Provider, cur *v1alpha1.Sandbox, deepcopy bool) (*v1alpha1.Sandbox, error) {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(cur)).V(utils.DebugLogLevel)
	fetchFromApiServer := false
	objectKey := client.ObjectKeyFromObject(cur)
	newSbx := &v1alpha1.Sandbox{}
	err := c.GetClient().Get(ctx, objectKey, newSbx)
	if err != nil {
		log.Info("failed to get claimed sandbox from cache, fetch from api-server", "reason", err.Error())
		fetchFromApiServer = true
	} else if !expectations.ResourceVersionExpectationSatisfied(newSbx) {
		log.Info("sandbox cache is out-dated, fetch from api-server")
		fetchFromApiServer = true
	}
	if fetchFromApiServer {
		if err = c.GetAPIReader().Get(ctx, objectKey, newSbx); err != nil {
			return cur, err
		}
	}
	if expectations.IsResourceVersionReallyNewer(cur.GetResourceVersion(), newSbx.GetResourceVersion()) {
		if deepcopy {
			return newSbx.DeepCopy(), nil
		}
		return newSbx, nil
	}
	return cur, nil
}

// RetryUpdate loads the latest sandbox from the informer first, applies the
// modifier, and retries on conflict. Conflict retries refresh from the
// APIReader to avoid cleaning stale informer data. It returns the CR to adopt
// and whether a real Update was issued.
func RetryUpdate(ctx context.Context, c cache.Provider, cur *v1alpha1.Sandbox, modifier ModifierFunc) (*v1alpha1.Sandbox, bool, error) {
	cr, updated, err := retryWrite(ctx, c, cur, modifier, false)
	if err != nil {
		klog.FromContext(ctx).WithValues("sandbox", klog.KObj(cur)).Error(err, "failed to update sandbox after retries")
	}
	return cr, updated, err
}

// RetryPatch mirrors RetryUpdate but persists the mutation with a
// MergeFromWithOptimisticLock patch instead of a full Update. The patch body
// only carries the fields the modifier changed plus the resourceVersion
// precondition, so callers backed by a field-stripping informer cache cannot
// erase stripped fields such as spec.embeddedSandboxTemplate on the API server.
func RetryPatch(ctx context.Context, c cache.Provider, cur *v1alpha1.Sandbox, modifier ModifierFunc) (*v1alpha1.Sandbox, bool, error) {
	cr, updated, err := retryWrite(ctx, c, cur, modifier, true)
	if err != nil {
		klog.FromContext(ctx).WithValues("sandbox", klog.KObj(cur)).Error(err, "failed to patch sandbox after retries")
	}
	return cr, updated, err
}

// retryWrite is the shared conflict-retry loop behind RetryUpdate and
// RetryPatch. When usePatch is true it persists with an optimistic-lock merge
// patch; otherwise it issues a full Update.
func retryWrite(ctx context.Context, c cache.Provider, cur *v1alpha1.Sandbox, modifier ModifierFunc, usePatch bool) (*v1alpha1.Sandbox, bool, error) {
	adopted := cur
	objectKey := client.ObjectKeyFromObject(cur)
	updated := false
	first := true
	var attemptErr error
	err := retry.RetryOnConflict(retry.DefaultRetry, func() (err error) {
		defer func() { attemptErr = err }()
		if err = ctx.Err(); err != nil {
			return err
		}
		latest := &v1alpha1.Sandbox{}
		if first {
			err = c.GetClient().Get(ctx, objectKey, latest)
		} else {
			err = c.GetAPIReader().Get(ctx, objectKey, latest)
		}
		first = false
		if err != nil {
			return err
		}

		copied := latest.DeepCopy()
		shouldUpdate, err := modifier(copied)
		if err != nil {
			return err
		}
		if !shouldUpdate {
			// The informer view may lag behind writes made earlier in the same
			// operation; never roll the wrapper back to an older object.
			if expectations.IsResourceVersionNewer(adopted.ResourceVersion, latest.ResourceVersion) {
				adopted = latest
			}
			updated = false
			return nil
		}
		// Inject trace context into annotations before writing so the
		// sandbox-controller can establish parent-child span relationship.
		copied.Annotations = tracing.InjectTraceContext(ctx, copied.Annotations)
		if usePatch {
			patch := client.MergeFromWithOptions(latest, client.MergeFromWithOptimisticLock{})
			if err = c.GetClient().Patch(ctx, copied, patch); err != nil {
				return err
			}
		} else {
			if err = c.GetClient().Update(ctx, copied); err != nil {
				return err
			}
		}
		adopted = copied
		expectations.ResourceVersionExpectationExpect(copied)
		updated = true
		return nil
	})
	if err == nil && attemptErr != nil {
		// retry.OnError rewrites wait.Interrupted errors (context cancellation
		// included) to the last conflict, which is nil when no conflict
		// happened; restore the real cause instead of reporting success.
		err = attemptErr
	}
	if err != nil {
		return adopted, false, err
	}
	return adopted, updated, nil
}

// RefreshFromAPIReader reads the sandbox straight from the APIReader and
// returns the fresh CR.
func RefreshFromAPIReader(ctx context.Context, c cache.Provider, cur *v1alpha1.Sandbox) (*v1alpha1.Sandbox, error) {
	latest := &v1alpha1.Sandbox{}
	if err := c.GetAPIReader().Get(ctx, client.ObjectKeyFromObject(cur), latest); err != nil {
		return cur, err
	}
	return latest, nil
}

// DefaultDeleteSandbox is the delete hook used by Kill. Tests override it to
// intercept deletion.
var DefaultDeleteSandbox = deleteSandbox

func deleteSandbox(ctx context.Context, sbx *v1alpha1.Sandbox, client client.Client) error {
	return client.Delete(ctx, sbx)
}

// Kill deletes the sandbox CR unless it is already terminating. When tracing is
// active it first patches trace context onto a copy so the controller's
// terminating Reconcile can join the trace; that patch failure is non-fatal.
func Kill(ctx context.Context, c cache.Provider, cur *v1alpha1.Sandbox) (err error) {
	if cur.GetDeletionTimestamp() != nil {
		return nil
	}

	ctx, span := tracing.StartManagerSpan(ctx, tracing.SpanInfraKill,
		attribute.String(tracing.AttrSandboxName, cur.Name),
	)
	// Keep the closure: a direct defer tracing.EndSpan(ctx, span, err) would
	// evaluate err while still nil and record every failure as success.
	defer func() { tracing.EndSpan(ctx, span, err) }()

	if tracing.HasInjectableTraceContext(ctx) {
		patched := cur.DeepCopy()
		patched.Annotations = tracing.InjectTraceContext(ctx, patched.Annotations)
		if patchErr := c.GetClient().Patch(ctx, patched, client.MergeFrom(cur)); patchErr != nil {
			klog.FromContext(ctx).Error(patchErr, "failed to inject trace context before deletion")
		}
	}

	return DefaultDeleteSandbox(ctx, cur, c.GetClient())
}

// ApplyTimeout writes the pause/shutdown deadlines onto the sandbox spec. A
// zero time clears the corresponding field.
func ApplyTimeout(sbx *v1alpha1.Sandbox, opts timeout.Options) {
	if !opts.PauseTime.IsZero() {
		sbx.Spec.PauseTime = ptr.To(metav1.NewTime(timeout.NormalizeTime(opts.PauseTime)))
	} else {
		sbx.Spec.PauseTime = nil
	}
	if !opts.ShutdownTime.IsZero() {
		sbx.Spec.ShutdownTime = ptr.To(metav1.NewTime(timeout.NormalizeTime(opts.ShutdownTime)))
	} else {
		sbx.Spec.ShutdownTime = nil
	}
}

// MergeExtraAnnotations merges annotations onto the sandbox CR metadata,
// initializing the map when needed.
func MergeExtraAnnotations(sbx *v1alpha1.Sandbox, annotations map[string]string) {
	if len(annotations) == 0 {
		return
	}
	if sbx.Annotations == nil {
		sbx.Annotations = map[string]string{}
	}
	for key, value := range annotations {
		sbx.Annotations[key] = value
	}
}

// SetMemoryPersistence adds or removes the memory persistent-content entry
// according to enabled. A nil enabled leaves the spec untouched.
func SetMemoryPersistence(sbx *v1alpha1.Sandbox, enabled *bool) {
	if enabled == nil {
		return
	}
	contents := make([]string, 0, len(sbx.Spec.PersistentContents)+1)
	memoryAdded := false
	for _, content := range sbx.Spec.PersistentContents {
		if content == v1alpha1.PersistentContentMemory {
			if *enabled && !memoryAdded {
				contents = append(contents, content)
				memoryAdded = true
			}
			continue
		}
		contents = append(contents, content)
	}
	if *enabled && !memoryAdded {
		contents = append(contents, v1alpha1.PersistentContentMemory)
	}
	sbx.Spec.PersistentContents = contents
}

// EnableWakeOnIngressTraffic arms the wake-on-ingress-traffic resume rule. A
// positive pauseTimeout becomes the rule's PauseTimeout so a traffic wake
// re-arms auto-pause with it; a non-positive value leaves PauseTimeout unset.
func EnableWakeOnIngressTraffic(sbx *v1alpha1.Sandbox, pauseTimeout time.Duration) {
	policy := sbx.Spec.AutoPausePolicy
	if policy == nil {
		policy = &v1alpha1.AutoPausePolicy{}
		sbx.Spec.AutoPausePolicy = policy
	}
	if policy.Resume == nil {
		policy.Resume = &v1alpha1.ResumePolicy{}
	}
	rule := &v1alpha1.IngressTrafficRule{}
	if pauseTimeout > 0 {
		rule.PauseTimeout = &metav1.Duration{Duration: pauseTimeout}
	}
	policy.Resume.OnIngressTraffic = rule
}

// ClearWakeOnIngressTraffic removes the wake-on-ingress-traffic resume rule and
// prunes now-empty parent policy nodes so a pooled spec stays byte-identical to
// a fresh one.
func ClearWakeOnIngressTraffic(sbx *v1alpha1.Sandbox) {
	policy := sbx.Spec.AutoPausePolicy
	if policy == nil || policy.Resume == nil {
		return
	}
	policy.Resume.OnIngressTraffic = nil
	if *policy.Resume == (v1alpha1.ResumePolicy{}) {
		policy.Resume = nil
	}
	if policy.Pause == nil && policy.Resume == nil {
		sbx.Spec.AutoPausePolicy = nil
	}
}

func GetPodLabels(sbx *v1alpha1.Sandbox) map[string]string {
	if sbx.Spec.Template != nil {
		return sbx.Spec.Template.Labels
	}
	return nil
}

func SetPodLabels(sbx *v1alpha1.Sandbox, labels map[string]string) {
	if sbx.Spec.Template != nil {
		sbx.Spec.Template.Labels = labels
	}
}

func GetPodAnnotations(sbx *v1alpha1.Sandbox) map[string]string {
	if sbx.Spec.Template != nil {
		return sbx.Spec.Template.Annotations
	}
	return nil
}

func SetPodAnnotations(sbx *v1alpha1.Sandbox, annotations map[string]string) {
	if sbx.Spec.Template != nil {
		sbx.Spec.Template.Annotations = annotations
	}
}

// SetImage sets the image of the first container.
func SetImage(sbx *v1alpha1.Sandbox, image string) {
	if sbx.Spec.Template != nil {
		sbx.Spec.Template.Spec.Containers[0].Image = image
	}
}

// GetImage returns the image of the first container, or empty when there is no
// pod template.
func GetImage(sbx *v1alpha1.Sandbox) string {
	if sbx.Spec.Template != nil {
		return sbx.Spec.Template.Spec.Containers[0].Image
	}
	return ""
}

// GetResource summarizes the sandbox's CPU/memory/disk footprint from its pod
// template containers.
func GetResource(sbx *v1alpha1.Sandbox) SandboxResource {
	if sbx.Spec.Template == nil {
		return SandboxResource{}
	}
	return CalculateResourceFromContainers(sbx.Spec.Template.Spec.Containers)
}
