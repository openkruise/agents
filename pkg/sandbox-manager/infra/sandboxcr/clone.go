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
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	checkpointUtils "github.com/openkruise/agents/pkg/utils/checkpoint"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/runtime"
	stateutils "github.com/openkruise/agents/pkg/utils/sandboxutils"
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
	if opts.WaitReadyTimeout <= 0 {
		opts.WaitReadyTimeout = consts.DefaultWaitReadyTimeout
	}
	if opts.CloneTimeout <= 0 {
		opts.CloneTimeout = DefaultCloneTimeout
	}
	return opts, nil
}

func ValidateAndInitCheckpointOptions(opts infra.CreateCheckpointOptions) infra.CreateCheckpointOptions {
	if opts.WaitSuccessTimeout <= 0 {
		opts.WaitSuccessTimeout = consts.DefaultWaitCheckpointTimeout
	}
	return opts
}

func CloneSandbox(ctx context.Context, opts infra.CloneSandboxOptions, cache cache.Provider) (infra.Sandbox, infra.CloneMetrics, error) {
	if opts.CloneTimeout > 0 {
		var cancel func()
		ctx, cancel = context.WithTimeout(ctx, opts.CloneTimeout)
		defer cancel()
	}
	log := klog.FromContext(ctx).WithValues("checkpoint", opts.CheckPointID)
	metrics := infra.CloneMetrics{}

	select {
	case <-ctx.Done():
		return nil, metrics, ctx.Err()
	default:
	}

	if opts.CreateLimiter != nil {
		log.Info("waiting for create sandbox limiter")
		waitStart := time.Now()
		err := opts.CreateLimiter.Wait(ctx)
		if err != nil {
			log.Error(err, "failed to wait create sandbox limiter")
			return nil, metrics, err
		}
		metrics.Wait = time.Since(waitStart)
		metrics.Total += metrics.Wait
		log.Info("create sandbox limiter waited", "cost", metrics.Wait)
	}

	// Step 1: get checkpoint and template from cache or API server
	tmpl, cp, metrics, err := findCheckpointAndTemplateById(ctx, opts, cache, metrics)
	if err != nil {
		return nil, metrics, err
	}

	// Step 2: create new sandbox from checkpoint
	sbx, initRuntimeOpts, metrics, err := createSandboxFromCheckpoint(ctx, opts, tmpl, cp, cache, metrics)
	if err != nil {
		return nil, metrics, err
	}

	// Step 3: wait for sandbox ready
	if metrics, err = cloneWaitSandboxReady(ctx, sbx, opts, cache, metrics); err != nil {
		return nil, metrics, err
	}

	// Step 4: re-init runtime
	if metrics, err = cloneReInitRuntime(ctx, sbx, opts, initRuntimeOpts, metrics); err != nil {
		return nil, metrics, err
	}

	// Step 5: csi mount
	// If opts.CSIMount is not provided from request, try to resolve mount options from sandbox annotation.
	if opts.CSIMount == nil {
		var resolveErr error
		opts.CSIMount, resolveErr = runtime.ResolveCSIMountFromAnnotation(ctx, sbx.Sandbox, sbx.Cache.GetClient(), sbx.Cache, sbx.storageRegistry)
		if resolveErr != nil {
			return nil, metrics, resolveErr
		}
	}
	if opts.CSIMount != nil {
		log.Info("starting to perform csi mount")
		metrics.CSIMount, err = runtime.ProcessCSIMounts(ctx, sbx.Sandbox, *opts.CSIMount)
		if err != nil {
			log.Error(err, "failed to perform csi mount")
			return nil, metrics, fmt.Errorf("failed to perform csi mount: %s", err)
		}
		metrics.Total += metrics.CSIMount
		log.Info("csi mount completed", "cost", metrics.CSIMount)
	}

	return sbx, metrics, nil
}

// findCheckpointAndTemplateById gets checkpoint and template from cache, fallback to API server if not found
func findCheckpointAndTemplateById(ctx context.Context, opts infra.CloneSandboxOptions, cache cache.Provider, metrics infra.CloneMetrics) (*v1alpha1.SandboxTemplate, *v1alpha1.Checkpoint, infra.CloneMetrics, error) {
	log := klog.FromContext(ctx).WithValues("checkpoint", opts.CheckPointID, "step", "1.findCheckpointAndTemplate")
	start := time.Now()

	// Try to get checkpoint from cache first
	var checkpoint *v1alpha1.Checkpoint
	retryFunc := utils.RetryIfContextNotCanceled(ctx)
	err := retry.OnError(utils.CacheBackoff, func(err error) bool {
		return !opts.SkipWaitCheckpoint && retryFunc(err)
	}, func() error {
		cp, err := cache.GetCheckpoint(ctx, opts.CheckPointID)
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

// createSandboxFromCheckpoint creates a new sandbox from checkpoint
func createSandboxFromCheckpoint(ctx context.Context, opts infra.CloneSandboxOptions, tmpl *v1alpha1.SandboxTemplate, cp *v1alpha1.Checkpoint, cache cache.Provider, metrics infra.CloneMetrics) (*Sandbox, *config.InitRuntimeOptions, infra.CloneMetrics, error) {
	log := klog.FromContext(ctx).WithValues("checkpoint", opts.CheckPointID, "step", "2.createSandboxFromCheckpoint")
	start := time.Now()
	initRuntimeOpts, err := runtime.GetInitRuntimeRequest(cp)
	if err != nil {
		log.Error(err, "failed to get init runtime request")
		return nil, nil, metrics, err
	}
	sbx := newSandboxFromTemplate(opts, tmpl, cache)
	if initRuntimeOpts != nil {
		sbx.Annotations[v1alpha1.AnnotationRuntimeAccessToken] = initRuntimeOpts.AccessToken
		sbx.Annotations[v1alpha1.AnnotationInitRuntimeRequest] = cp.Annotations[v1alpha1.AnnotationInitRuntimeRequest]
	}
	// e.g., copy csi mount config from checkpoint to sandbox obj
	checkpointUtils.RestoreAnnotationsFromCheckpoint(cp, sbx.Sandbox)
	DefaultPostProcessClonedSandbox(sbx.Sandbox)
	log.Info("creating new sandbox from checkpoint")
	sbx.Sandbox, err = DefaultCreateSandbox(ctx, sbx.Sandbox, cache.GetClient())
	if err != nil {
		log.Error(err, "failed to create sandbox")
		return nil, nil, metrics, err
	}
	log = log.WithValues("sandbox", klog.KObj(sbx))
	metrics.CreateSandbox = time.Since(start)
	metrics.Total += metrics.CreateSandbox
	log.Info("sandbox created, waiting it ready", "cost", metrics.CreateSandbox)
	return sbx, initRuntimeOpts, metrics, nil
}

// cloneWaitSandboxReady waits for the sandbox to be ready
func cloneWaitSandboxReady(ctx context.Context, sbx *Sandbox, opts infra.CloneSandboxOptions, cache cache.Provider, metrics infra.CloneMetrics) (infra.CloneMetrics, error) {
	log := klog.FromContext(ctx).WithValues("checkpoint", opts.CheckPointID, "step", "3.waitSandboxReady")
	var err error
	metrics.WaitReady, err = waitForSandboxReady(ctx, sbx, infra.ClaimSandboxOptions{
		WaitReadyTimeout: opts.WaitReadyTimeout,
	}, cache)
	if err != nil {
		log.Error(err, "failed to wait sandbox ready", "cost", metrics.WaitReady)
		return metrics, err
	}
	metrics.Total += metrics.WaitReady
	return metrics, nil
}

// cloneReInitRuntime re-initializes the runtime if needed
func cloneReInitRuntime(ctx context.Context, sbx *Sandbox, opts infra.CloneSandboxOptions, initRuntimeOpts *config.InitRuntimeOptions, metrics infra.CloneMetrics) (infra.CloneMetrics, error) {
	log := klog.FromContext(ctx).WithValues("checkpoint", opts.CheckPointID, "step", "4.reInitRuntime")
	if initRuntimeOpts == nil {
		return metrics, nil
	}
	initRuntimeOpts.ReInit = true
	log.Info("re-init runtime")
	var err error
	metrics.InitRuntime, err = runtime.InitRuntime(ctx, sbx.Sandbox, *initRuntimeOpts, sbx.refreshFunc())
	if err != nil {
		log.Error(err, "failed to init runtime")
		return metrics, fmt.Errorf("failed to init runtime: %w", err)
	}
	metrics.Total += metrics.InitRuntime
	return metrics, nil
}

// newSandboxFromTemplate returns a Sandbox object whose annotations / labels are not nil
func newSandboxFromTemplate(opts infra.CloneSandboxOptions, tmpl *v1alpha1.SandboxTemplate, cache cache.Provider) *Sandbox {
	tmplCopy := tmpl.DeepCopy()
	sbx := AsSandbox(&v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			// Use checkpoint id as the prefix to avoid name length explosion caused by repeated checkpoints.
			GenerateName: opts.CheckPointID + "-",
			Namespace:    tmplCopy.Namespace,
			Labels:       map[string]string{},
			Annotations:  map[string]string{},
		},
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

func CreateCheckpoint(ctx context.Context, sbx *v1alpha1.Sandbox, cache cache.Provider, opts infra.CreateCheckpointOptions) (string, error) {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx))
	log.Info("creating sandbox template")
	tmpl := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: sbx.Name + "-",
			Namespace:    sbx.Namespace,
		},
		Spec: v1alpha1.SandboxTemplateSpec{
			PersistentContents:   sbx.Spec.PersistentContents,
			Template:             sbx.Spec.Template,
			VolumeClaimTemplates: sbx.Spec.VolumeClaimTemplates,
			Runtimes:             sbx.Spec.Runtimes,
		},
	}
	tmpl, err := DefaultCreateSandboxTemplate(ctx, cache.GetClient(), tmpl)
	if err != nil {
		log.Error(err, "failed to create sandbox template")
		return "", fmt.Errorf("failed to create sandbox template: %w", err)
	}
	log = log.WithValues("template", klog.KObj(tmpl))
	log.Info("template created")
	cp := &v1alpha1.Checkpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tmpl.Name,
			Namespace: sbx.Namespace,
			Annotations: map[string]string{
				v1alpha1.AnnotationInitRuntimeRequest: sbx.Annotations[v1alpha1.AnnotationInitRuntimeRequest],
				v1alpha1.AnnotationOwner:              sbx.Annotations[v1alpha1.AnnotationOwner],
				v1alpha1.AnnotationSandboxID:          stateutils.GetSandboxID(sbx),
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         v1alpha1.SandboxTemplateControllerKind.GroupVersion().String(),
					Kind:               v1alpha1.SandboxTemplateControllerKind.Kind,
					Name:               tmpl.Name,
					UID:                tmpl.UID,
					Controller:         ptr.To(true),
					BlockOwnerDeletion: ptr.To(true),
				},
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
		for _, pc := range tmpl.Spec.PersistentContents {
			if pc == v1alpha1.CheckpointPersistentContentFilesystem || pc == v1alpha1.CheckpointPersistentContentMemory {
				cp.Spec.PersistentContents = append(cp.Spec.PersistentContents, pc)
			}
		}
	}
	// to make sure the sandbox annotations are propagated to the checkpoint
	checkpointUtils.PropagateAnnotationsToCheckpoint(sbx, cp)
	cp, err = DefaultCreateCheckpoint(ctx, cache.GetClient(), cp)
	if err != nil {
		log.Error(err, "failed to create checkpoint")
		return "", fmt.Errorf("failed to create checkpoint: %w", err)
	}
	log = log.WithValues("checkpoint", klog.KObj(cp))
	log.Info("checkpoint creating")
	if err = cache.NewCheckpointTask(ctx, cp).Wait(opts.WaitSuccessTimeout); err != nil {
		log.Error(err, "failed to wait checkpoint ready")
		return "", fmt.Errorf("failed to wait checkpoint ready: %w", err)
	}
	fresh := &v1alpha1.Checkpoint{}
	if err = cache.GetClient().Get(ctx, client.ObjectKeyFromObject(cp), fresh); err != nil {
		log.Error(err, "failed to refresh checkpoint after wait")
		return "", fmt.Errorf("failed to refresh checkpoint: %w", err)
	}
	log.Info("checkpoint created")
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
