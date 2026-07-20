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

package sandboxcr

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/api/v1alpha1"
	infracache "github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/runtime"
)

var (
	DefaultPostProcessClonedSandbox = postProcessClonedSandbox
	DefaultCreateSandboxTemplate    = createSandboxTemplate
	DefaultCreateCheckpoint         = createCheckpoint
)

func ValidateAndInitCloneOptions(opts infra.CloneSandboxOptions) (infra.CloneSandboxOptions, error) {
	if opts.User == "" {
		return infra.CloneSandboxOptions{}, fmt.Errorf("user is required")
	}
	if opts.CheckPointID == "" {
		return infra.CloneSandboxOptions{}, fmt.Errorf("checkpoint id is required")
	}
	if opts.Name != "" && opts.GenerateName != "" {
		return infra.CloneSandboxOptions{}, fmt.Errorf("name and generateName are mutually exclusive")
	}
	if opts.WaitReadyTimeout <= 0 {
		opts.WaitReadyTimeout = consts.DefaultWaitReadyTimeout
	}
	if opts.CloneTimeout <= 0 {
		opts.CloneTimeout = DefaultCloneTimeout
	}
	if opts.ReserveFailedSandboxFor == nil {
		opts.ReserveFailedSandboxFor = ptr.To(DefaultReserveFailedSandboxFor)
	}
	return opts, nil
}

func ValidateAndInitCheckpointOptions(opts infra.CreateCheckpointOptions) infra.CreateCheckpointOptions {
	if opts.WaitSuccessTimeout <= 0 {
		opts.WaitSuccessTimeout = consts.DefaultWaitCheckpointTimeout
	}
	return opts
}

func CloneSandbox(ctx context.Context, opts infra.CloneSandboxOptions, cache infracache.Provider) (cloned infra.Sandbox, metrics infra.CloneMetrics, err error) {
	log := klog.FromContext(ctx).WithValues("checkpoint", opts.CheckPointID)
	opts.LockString = chooseLockString(opts.Admission, opts.LockString)
	admitted := false

	select {
	case <-ctx.Done():
		err = ctx.Err()
		return
	default:
	}

	// Step 1: get checkpoint and template from cache or API server
	tmpl, cp, metrics, err := findCheckpointAndTemplateById(ctx, opts, cache, metrics)
	if err != nil {
		return nil, metrics, err
	}

	// Step 2: block on the create rate limiter so a single Infra.createLimiter
	// gates both claim and clone create traffic. Unlike newSandboxFromSandboxSet
	// (which uses Allow() and fails fast with a retriable error), clone blocks
	// here because the lookup above already proved this attempt is worth doing.
	if metrics, err = waitCloneCreateLimiter(ctx, opts, metrics); err != nil {
		return nil, metrics, err
	}

	// Step 3: create new sandbox from checkpoint.
	// Create failures are retriable by product choice: callers prefer a delayed
	// success over a 500. The trade-off is orphan amplification — if a Create
	// timed out on the client side after apiserver had already persisted the
	// CR, the retry produces a second CR that this code path will not see
	// (GenerateName, no IsAlreadyExists signal). Such orphans must be reaped
	// out-of-band (e.g. by a janitor reconciler).
	sbx, initRuntimeOpts, err := prepareSandboxFromCheckpoint(ctx, opts, tmpl, cp, cache)
	if err != nil {
		return nil, metrics, err
	}
	if opts.Admission != nil && opts.Admission.Acquire != nil {
		if err = opts.Admission.Acquire(ctx, opts.LockString, sbx.GetResource()); err != nil {
			log.Error(err, "failed to acquire sandbox admission", "lockString", opts.LockString)
			return nil, metrics, err
		}
		admitted = true
	}
	sbx, metrics, err = createPreparedSandbox(ctx, opts, sbx, cache, metrics)
	if err != nil {
		if admitted && shouldReleaseAdmissionAfterLockError(err) {
			releaseAdmission(ctx, opts.Admission, opts.LockString)
		}
		if managererrors.GetErrCode(err) == managererrors.ErrorQuotaExceeded {
			return nil, metrics, err
		}
		if !wait.Interrupted(err) {
			// When the user explicitly specified a name and the sandbox already
			// exists, retrying is pointless — the name collision is deterministic.
			// Return the raw AlreadyExists error so upper layers (api.go) can map
			// it to ErrorConflict instead of ErrorInternal.
			if opts.Name != "" && apierrors.IsAlreadyExists(err) {
				return nil, metrics, err
			}
			err = classifyCreateError(err, "failed to create sandbox from checkpoint")
		}
		return nil, metrics, err
	}
	created := sbx
	defer func() {
		clearFailedSandbox(ctx, created, err, opts.ReserveFailedSandboxFor, opts.Admission, opts.LockString)
	}()

	// Step 4: wait for sandbox ready
	if metrics, err = cloneWaitSandboxReady(ctx, sbx, opts, cache, metrics); err != nil {
		// Preserve context cancellation / deadline so the outer retry loop can
		// stop instead of treating it as a retriable wrap.
		if !wait.Interrupted(err) {
			err = retriableError{Message: fmt.Sprintf("failed to wait for sandbox ready: %s", err)}
		}
		return
	}

	// Step 5: re-init runtime
	if metrics, err = cloneReInitRuntime(ctx, sbx, opts, initRuntimeOpts, metrics); err != nil {
		if !wait.Interrupted(err) {
			err = retriableError{Message: fmt.Sprintf("failed to init runtime: %s", err)}
		}
		return
	}

	// Step 6: process security token
	// Issue and propagate the identity-provider security token before performing
	// CSI mounts, mirroring the claim flow ordering.
	if identity.IsIDTokenRequested(sbx.Sandbox) {
		metrics.SecurityToken, err = identity.ProcessSandboxToken(ctx, cache.GetClient(), sbx.Sandbox)
		if err != nil {
			if !wait.Interrupted(err) {
				err = retriableError{Message: fmt.Sprintf("security token processing failed: %s", err)}
			}
			return
		}
		metrics.Total += metrics.SecurityToken
	}

	// Step 7: csi mount
	// If opts.CSIMount is not provided from request, try to resolve mount options from sandbox annotation.
	if opts.CSIMount == nil {
		var resolveErr error
		opts.CSIMount, resolveErr = runtime.ResolveCSIMountFromAnnotation(ctx, sbx.Sandbox, sbx.Cache.GetClient(), sbx.Cache, sbx.storageRegistry)
		if resolveErr != nil {
			err = resolveErr
			return
		}
	}
	if opts.CSIMount != nil {
		log.Info("starting to perform csi mount")
		metrics.CSIMount, err = runtime.ProcessCSIMounts(ctx, sbx.Sandbox, *opts.CSIMount)
		metrics.Total += metrics.CSIMount
		if err != nil {
			log.Error(err, "failed to perform csi mount")
			err = fmt.Errorf("failed to perform csi mount: %s", err)
			return
		}
		log.Info("csi mount completed", "cost", metrics.CSIMount)
	}

	cloned = sbx
	return
}

// findCheckpointAndTemplateById gets checkpoint and template from cache, fallback to API server if not found
func findCheckpointAndTemplateById(ctx context.Context, opts infra.CloneSandboxOptions, cache infracache.Provider, metrics infra.CloneMetrics) (*v1alpha1.SandboxTemplate, *v1alpha1.Checkpoint, infra.CloneMetrics, error) {
	log := klog.FromContext(ctx).WithValues("checkpoint", opts.CheckPointID, "step", "1.findCheckpointAndTemplate")
	start := time.Now()

	// Try to get checkpoint from cache first
	var checkpoint *v1alpha1.Checkpoint
	retryFunc := utils.RetryIfContextNotCanceled(ctx)
	err := retry.OnError(utils.CacheBackoff, func(err error) bool {
		return !opts.SkipWaitCheckpoint && retryFunc(err)
	}, func() error {
		cp, err := cache.GetCheckpoint(ctx, infracache.GetCheckpointOptions{Namespace: opts.Namespace, CheckpointID: opts.CheckPointID})
		if err != nil {
			return err
		}
		checkpoint = cp
		return nil
	})
	if err != nil {
		log.Error(err, "checkpoint not found in cache")
		return nil, nil, metrics, err
	}

	// Try to get template from cache first

	key := client.ObjectKey{Namespace: checkpoint.Namespace, Name: checkpoint.Name}
	template := &v1alpha1.SandboxTemplate{}
	err = utils.GetFromInformerOrApiServer(ctx, template, key, cache.GetClient(), cache.GetAPIReader())
	if err != nil {
		log.Error(err, "failed to get sandbox template", "key", key)
		return nil, nil, metrics, err
	}

	metrics.GetTemplate = time.Since(start)
	metrics.Total += metrics.GetTemplate
	log.Info("checkpoint and template found", "cost", metrics.GetTemplate)
	return template, checkpoint, metrics, nil
}

// waitCloneCreateLimiter blocks on opts.CreateLimiter (when set) and records
// the wait cost in metrics so callers see it under metrics.Wait / metrics.Total.
// The limiter itself is the same Infra.createLimiter used by
// newSandboxFromSandboxSet, so claim and clone share one rate-limit choke
// point; the gating style differs (blocking Wait here vs non-blocking Allow
// there) and callers should not assume identical back-pressure semantics.
func waitCloneCreateLimiter(ctx context.Context, opts infra.CloneSandboxOptions, metrics infra.CloneMetrics) (infra.CloneMetrics, error) {
	if opts.CreateLimiter == nil {
		return metrics, nil
	}
	log := klog.FromContext(ctx).WithValues("checkpoint", opts.CheckPointID, "step", "2.waitCreateLimiter")
	log.Info("waiting for create sandbox limiter")
	start := time.Now()
	waitErr := opts.CreateLimiter.Wait(ctx)
	cost := time.Since(start)
	metrics.Wait += cost
	metrics.Total += cost
	if waitErr != nil {
		// rate.Limiter.Wait can return its own "rate: Wait(n=1) would exceed
		// context deadline" sentinel when, at the moment Wait inspected the
		// context, the wall-clock had already passed ctx.Deadline() but
		// ctx.Done() was not yet observable. That is the same outcome as a
		// plain deadline expiry, just surfaced through a different branch in
		// the limiter. Normalize it so callers (and wait.Interrupted in the
		// outer retry loop) always see the canonical context error and tests
		// do not depend on which branch the runtime happens to pick.
		if !errors.Is(waitErr, context.Canceled) && !errors.Is(waitErr, context.DeadlineExceeded) {
			if ctxErr := ctx.Err(); ctxErr != nil {
				waitErr = ctxErr
			} else if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
				waitErr = context.DeadlineExceeded
			}
		}
		log.Error(waitErr, "failed to wait create sandbox limiter")
		return metrics, waitErr
	}
	log.Info("create sandbox limiter waited", "cost", cost)
	return metrics, nil
}

func prepareSandboxFromCheckpoint(ctx context.Context, opts infra.CloneSandboxOptions, tmpl *v1alpha1.SandboxTemplate, cp *v1alpha1.Checkpoint, cache infracache.Provider) (*Sandbox, *config.InitRuntimeOptions, error) {
	log := klog.FromContext(ctx).WithValues("checkpoint", opts.CheckPointID, "step", "3.prepareSandboxFromCheckpoint")
	initRuntimeOpts, err := runtime.GetInitRuntimeRequest(cp)
	if err != nil {
		log.Error(err, "failed to get init runtime request")
		return nil, nil, err
	}
	sbx := newSandboxFromTemplate(opts, tmpl, cache)
	if initRuntimeOpts != nil {
		sbx.Annotations[v1alpha1.AnnotationRuntimeAccessToken] = initRuntimeOpts.AccessToken
		sbx.Annotations[v1alpha1.AnnotationInitRuntimeRequest] = cp.Annotations[v1alpha1.AnnotationInitRuntimeRequest]
	}
	// e.g., copy csi mount config from checkpoint to sandbox obj
	RestoreAnnotationsFromCheckpoint(cp, sbx.Sandbox)
	// When the clone request explicitly provides CSI mount configs, they take
	// precedence over the csi-volume-config restored from the checkpoint. This
	// keeps the persisted annotation consistent with the mount performed in the
	// csi-mount step, so later re-mounts (resume / in-place update / pod
	// recreation) reuse the user-provided config instead of the checkpoint one.
	if opts.CSIMount != nil && opts.CSIMount.MountOptionListRaw != "" {
		sbx.Annotations[v1alpha1.AnnotationCSIVolumeConfig] = opts.CSIMount.MountOptionListRaw
	}
	DefaultPostProcessClonedSandbox(sbx.Sandbox)
	return sbx, initRuntimeOpts, nil
}

func createPreparedSandbox(ctx context.Context, opts infra.CloneSandboxOptions, sbx *Sandbox, cache infracache.Provider, metrics infra.CloneMetrics) (*Sandbox, infra.CloneMetrics, error) {
	log := klog.FromContext(ctx).WithValues("checkpoint", opts.CheckPointID, "step", "3.createSandboxFromCheckpoint")
	start := time.Now()
	log.Info("creating new sandbox from checkpoint")
	created, err := DefaultCreateSandbox(ctx, sbx.Sandbox, cache.GetClient())
	if err != nil {
		log.Error(err, "failed to create sandbox")
		return nil, metrics, err
	}
	sbx.Sandbox = created
	log = log.WithValues("sandbox", klog.KObj(sbx))
	metrics.CreateSandbox = time.Since(start)
	metrics.Total += metrics.CreateSandbox
	log.Info("sandbox created, waiting it ready", "cost", metrics.CreateSandbox)
	return sbx, metrics, nil
}

// cloneWaitSandboxReady waits for the sandbox to be ready
func cloneWaitSandboxReady(ctx context.Context, sbx *Sandbox, opts infra.CloneSandboxOptions, cache infracache.Provider, metrics infra.CloneMetrics) (infra.CloneMetrics, error) {
	log := klog.FromContext(ctx).WithValues("checkpoint", opts.CheckPointID, "step", "4.waitSandboxReady")
	var err error
	metrics.WaitReady, err = waitForSandboxReady(ctx, sbx, infra.ClaimSandboxOptions{
		WaitReadyTimeout: opts.WaitReadyTimeout,
	}, cache)
	metrics.Total += metrics.WaitReady
	if err != nil {
		log.Error(err, "failed to wait sandbox ready", "cost", metrics.WaitReady)
		return metrics, err
	}
	return metrics, nil
}

// cloneReInitRuntime re-initializes the runtime if needed
func cloneReInitRuntime(ctx context.Context, sbx *Sandbox, opts infra.CloneSandboxOptions, initRuntimeOpts *config.InitRuntimeOptions, metrics infra.CloneMetrics) (infra.CloneMetrics, error) {
	log := klog.FromContext(ctx).WithValues("checkpoint", opts.CheckPointID, "step", "5.reInitRuntime")
	if initRuntimeOpts == nil {
		return metrics, nil
	}
	initRuntimeOpts.ReInit = true
	log.Info("re-init runtime")
	var err error
	metrics.InitRuntime, err = runtime.InitRuntime(ctx, sbx.Sandbox, *initRuntimeOpts, sbx.refreshFunc())
	metrics.Total += metrics.InitRuntime
	if err != nil {
		log.Error(err, "failed to init runtime")
		return metrics, err
	}
	return metrics, nil
}

// newSandboxFromTemplate returns a Sandbox object whose annotations / labels are not nil
func newSandboxFromTemplate(opts infra.CloneSandboxOptions, tmpl *v1alpha1.SandboxTemplate, cache infracache.Provider) *Sandbox {
	tmplCopy := tmpl.DeepCopy()
	meta := metav1.ObjectMeta{
		Namespace:   tmplCopy.Namespace,
		Labels:      map[string]string{},
		Annotations: map[string]string{},
	}
	switch {
	case opts.Name != "":
		meta.Name = opts.Name
	case opts.GenerateName != "":
		meta.GenerateName = opts.GenerateName
	default:
		// Use checkpoint id as the prefix to avoid name length explosion caused by repeated checkpoints.
		meta.GenerateName = opts.CheckPointID + "-"
	}
	sbx := AsSandbox(&v1alpha1.Sandbox{
		ObjectMeta: meta,
		Spec: v1alpha1.SandboxSpec{
			PersistentContents: tmplCopy.Spec.PersistentContents,
			Runtimes:           tmplCopy.Spec.Runtimes,
			EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
				Template:             tmplCopy.Spec.Template,
				VolumeClaimTemplates: tmplCopy.Spec.VolumeClaimTemplates,
			},
		},
	}, cache)
	if opts.Modifier != nil {
		opts.Modifier(sbx)
	}
	labels := sbx.GetLabels()
	labels[v1alpha1.LabelSandboxTemplate] = tmplCopy.Name
	labels[v1alpha1.LabelSandboxIsClaimed] = v1alpha1.True
	sbx.SetLabels(labels)

	annotations := sbx.GetAnnotations()
	annotations[v1alpha1.AnnotationOwner] = opts.User
	annotations[v1alpha1.AnnotationLock] = opts.LockString
	annotations[v1alpha1.AnnotationClaimTime] = time.Now().Format(time.RFC3339)
	annotations[v1alpha1.AnnotationRestoreFrom] = opts.CheckPointID
	sbx.SetAnnotations(annotations)

	return sbx
}

func postProcessClonedSandbox(*v1alpha1.Sandbox) {}

func createSandboxTemplate(ctx context.Context, c client.Client, tmpl *v1alpha1.SandboxTemplate) (*v1alpha1.SandboxTemplate, error) {
	if err := c.Create(ctx, tmpl); err != nil {
		return nil, err
	}
	return tmpl, nil
}

func createCheckpoint(ctx context.Context, c client.Client, cp *v1alpha1.Checkpoint) (*v1alpha1.Checkpoint, error) {
	if err := c.Create(ctx, cp); err != nil {
		return nil, err
	}
	return cp, nil
}

func CreateCheckpoint(ctx context.Context, sbx *v1alpha1.Sandbox, cache infracache.Provider, opts infra.CreateCheckpointOptions) (string, error) {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx))

	// Step 1: Build the Checkpoint with GenerateName. The Checkpoint is the new
	// owner of the SandboxTemplate; it carries no OwnerReferences itself.
	cp := &v1alpha1.Checkpoint{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: sbx.Name + "-",
			Namespace:    sbx.Namespace,
			Annotations: map[string]string{
				v1alpha1.AnnotationInitRuntimeRequest: sbx.Annotations[v1alpha1.AnnotationInitRuntimeRequest],
				v1alpha1.AnnotationOwner:              sbx.Annotations[v1alpha1.AnnotationOwner],
				v1alpha1.AnnotationSandboxID:          utils.GetSandboxID(sbx),
			},
		},
		Spec: v1alpha1.CheckpointSpec{
			PodName:          ptr.To(sbx.Name),
			KeepRunning:      opts.KeepRunning,
			TtlAfterFinished: opts.TTL,
		},
	}
	if len(opts.PersistentContents) > 0 {
		cp.Spec.PersistentContents = opts.PersistentContents
	} else {
		// Source falls back to the Sandbox itself (the same data the
		// SandboxTemplate would copy from), filtered to the persistent-content
		// values the Checkpoint understands.
		for _, pc := range sbx.Spec.PersistentContents {
			if pc == v1alpha1.CheckpointPersistentContentFilesystem || pc == v1alpha1.CheckpointPersistentContentMemory {
				cp.Spec.PersistentContents = append(cp.Spec.PersistentContents, pc)
			}
		}
	}
	// Propagate sandbox annotations (e.g., csi mount config) to the Checkpoint
	// before creation.
	PropagateAnnotationsToCheckpoint(sbx, cp)
	log.Info("creating checkpoint")
	cp, err := DefaultCreateCheckpoint(ctx, cache.GetClient(), cp)
	if err != nil {
		log.Error(err, "failed to create checkpoint")
		return "", fmt.Errorf("failed to create checkpoint: %w", err)
	}
	log = log.WithValues("checkpoint", klog.KObj(cp))
	log.Info("checkpoint created")

	// Step 2: Build the SandboxTemplate with the Checkpoint's name and an
	// OwnerReference pointing back at the Checkpoint, so deletion of the
	// Checkpoint cascades to the SandboxTemplate via Kubernetes GC.
	tmpl := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cp.Name,
			Namespace: sbx.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         v1alpha1.CheckpointControllerKind.GroupVersion().String(),
					Kind:               v1alpha1.CheckpointControllerKind.Kind,
					Name:               cp.Name,
					UID:                cp.UID,
					Controller:         ptr.To(true),
					BlockOwnerDeletion: ptr.To(true),
				},
			},
		},
		Spec: v1alpha1.SandboxTemplateSpec{
			PersistentContents:   sbx.Spec.PersistentContents,
			Template:             sbx.Spec.Template,
			VolumeClaimTemplates: sbx.Spec.VolumeClaimTemplates,
			Runtimes:             sbx.Spec.Runtimes,
		},
	}
	log.Info("creating sandbox template")
	tmpl, err = DefaultCreateSandboxTemplate(ctx, cache.GetClient(), tmpl)
	if err != nil {
		log.Error(err, "failed to create sandbox template")
		return "", fmt.Errorf("failed to create sandbox template: %w", err)
	}
	log = log.WithValues("template", klog.KObj(tmpl))
	log.Info("template created")

	// Step 3: Wait for the Checkpoint to reach Succeeded.
	// In the future, we can delete the failed Checkpoint and retry like ClaimSandbox
	if err = cache.NewCheckpointTask(ctx, cp).Wait(opts.WaitSuccessTimeout); err != nil {
		log.Error(err, "failed to wait checkpoint ready")
		return "", fmt.Errorf("failed to wait checkpoint ready: %w", err)
	}
	fresh := &v1alpha1.Checkpoint{}
	if err = cache.GetClient().Get(ctx, client.ObjectKeyFromObject(cp), fresh); err != nil {
		log.Error(err, "failed to refresh checkpoint after wait")
		return "", fmt.Errorf("failed to refresh checkpoint: %w", err)
	}
	log.Info("checkpoint ready")
	return fresh.Status.CheckpointId, nil
}

func AsCheckpointInfo(cp *v1alpha1.Checkpoint) infra.CheckpointInfo {
	return infra.CheckpointInfo{
		Name:              cp.Name,
		Namespace:         cp.Namespace,
		Phase:             string(cp.Status.Phase),
		CheckpointID:      cp.Status.CheckpointId,
		SandboxID:         cp.Annotations[v1alpha1.AnnotationSandboxID],
		CreationTimestamp: cp.CreationTimestamp.Format(time.RFC3339),
	}
}
