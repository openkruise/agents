package sandboxcr

import (
	"context"
	"fmt"
	"time"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
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

func CloneSandbox(ctx context.Context, opts infra.CloneSandboxOptions, cache *Cache, client *clients.ClientSet) (infra.Sandbox, infra.CloneMetrics, error) {
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
	tmpl, cp, metrics, err := stepFindCheckpointAndTemplate(ctx, opts, cache, client, metrics)
	if err != nil {
		return nil, metrics, err
	}

	// Step 2: create new sandbox from checkpoint
	sbx, initRuntimeOpts, metrics, err := stepCreateSandboxFromCheckpoint(ctx, opts, tmpl, cp, cache, client, metrics)
	if err != nil {
		return nil, metrics, err
	}

	// Step 3: wait for sandbox ready
	if metrics, err = stepWaitSandboxReady(ctx, sbx, opts, cache, metrics); err != nil {
		return nil, metrics, err
	}

	// Step 4: re-init runtime
	if metrics, err = stepReInitRuntime(ctx, sbx, opts, initRuntimeOpts, metrics); err != nil {
		return nil, metrics, err
	}

	// Step 5: csi mount
	if opts.CSIMount != nil {
		log.Info("starting to perform csi mount")
		for _, mountConfig := range opts.CSIMount.MountOptionList {
			cost, err := csiMount(ctx, sbx, mountConfig)
			metrics.CSIMount += cost
			metrics.Total += cost
			if err != nil {
				log.Error(err, "failed to perform csi mount")
				return nil, metrics, fmt.Errorf("failed to perform csi mount: %s", err)
			}
		}
		log.Info("csi mount completed", "cost", metrics.CSIMount)
	}

	return sbx, metrics, nil
}

// stepFindCheckpointAndTemplate gets checkpoint and template from cache, fallback to API server if not found
func stepFindCheckpointAndTemplate(ctx context.Context, opts infra.CloneSandboxOptions, cache *Cache, client *clients.ClientSet, metrics infra.CloneMetrics) (*v1alpha1.SandboxTemplate, *v1alpha1.Checkpoint, infra.CloneMetrics, error) {
	log := klog.FromContext(ctx).WithValues("checkpoint", opts.CheckPointID, "step", "1.findCheckpointAndTemplate")
	start := time.Now()

	// Try to get checkpoint from cache first
	var checkpoint *v1alpha1.Checkpoint
	err := retry.OnError(utils.CacheBackoff, utils.RetryIfContextNotCanceled(ctx), func() error {
		cp, err := cache.GetCheckpoint(opts.CheckPointID)
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
	template, err := cache.GetSandboxTemplate(checkpoint.Namespace, checkpoint.Name)
	if err != nil {
		log.Info("template not found in cache, trying API server", "namespace", checkpoint.Namespace, "name", checkpoint.Name, "error", err)
		// Fallback to API server
		template, err = client.SandboxClient.ApiV1alpha1().SandboxTemplates(checkpoint.Namespace).Get(ctx, checkpoint.Name, metav1.GetOptions{})
		if err != nil {
			log.Error(err, "failed to get sandbox template from API server", "namespace", checkpoint.Namespace, "name", checkpoint.Name)
			return nil, nil, metrics, err
		}
	}

	metrics.GetTemplate = time.Since(start)
	metrics.Total += metrics.GetTemplate
	log.Info("checkpoint and template found", "cost", metrics.GetTemplate)
	return template, checkpoint, metrics, nil
}

// stepCreateSandboxFromCheckpoint creates a new sandbox from checkpoint
func stepCreateSandboxFromCheckpoint(ctx context.Context, opts infra.CloneSandboxOptions, tmpl *v1alpha1.SandboxTemplate, cp *v1alpha1.Checkpoint, cache *Cache, client *clients.ClientSet, metrics infra.CloneMetrics) (*Sandbox, *config.InitRuntimeOptions, infra.CloneMetrics, error) {
	log := klog.FromContext(ctx).WithValues("checkpoint", opts.CheckPointID, "step", "2.createSandboxFromCheckpoint")
	start := time.Now()
	initRuntimeOpts, err := getInitRuntimeRequest(cp)
	if err != nil {
		log.Error(err, "failed to get init runtime request")
		return nil, nil, metrics, err
	}
	sbx := newSandboxFromTemplate(opts, tmpl, cache, client)
	if initRuntimeOpts != nil {
		sbx.Annotations[v1alpha1.AnnotationRuntimeAccessToken] = initRuntimeOpts.AccessToken
		sbx.Annotations[v1alpha1.AnnotationInitRuntimeRequest] = cp.Annotations[v1alpha1.AnnotationInitRuntimeRequest]
	}
	DefaultPostProcessClonedSandbox(sbx.Sandbox)
	log.Info("creating new sandbox from checkpoint")
	sbx.Sandbox, err = DefaultCreateSandbox(ctx, sbx.Sandbox, client)
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

// stepWaitSandboxReady waits for the sandbox to be ready
func stepWaitSandboxReady(ctx context.Context, sbx *Sandbox, opts infra.CloneSandboxOptions, cache *Cache, metrics infra.CloneMetrics) (infra.CloneMetrics, error) {
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

// stepReInitRuntime re-initializes the runtime if needed
func stepReInitRuntime(ctx context.Context, sbx *Sandbox, opts infra.CloneSandboxOptions, initRuntimeOpts *config.InitRuntimeOptions, metrics infra.CloneMetrics) (infra.CloneMetrics, error) {
	log := klog.FromContext(ctx).WithValues("checkpoint", opts.CheckPointID, "step", "4.reInitRuntime")
	if initRuntimeOpts == nil {
		return metrics, nil
	}
	initRuntimeOpts.ReInit = true
	log.Info("re-init runtime")
	var err error
	metrics.InitRuntime, err = initRuntime(ctx, sbx, *initRuntimeOpts)
	if err != nil {
		log.Error(err, "failed to init runtime")
		return metrics, fmt.Errorf("failed to init runtime: %w", err)
	}
	metrics.Total += metrics.InitRuntime
	return metrics, nil
}

// newSandboxFromTemplate returns a Sandbox object whose annotations / labels are not nil
func newSandboxFromTemplate(opts infra.CloneSandboxOptions, tmpl *v1alpha1.SandboxTemplate, cache *Cache, client *clients.ClientSet) *Sandbox {
	sbx := AsSandbox(&v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			// Use checkpoint id as the prefix to avoid name length explosion caused by repeated checkpoints.
			GenerateName: opts.CheckPointID + "-",
			Namespace:    tmpl.Namespace,
			Labels:       map[string]string{},
			Annotations:  map[string]string{},
		},
		Spec: v1alpha1.SandboxSpec{
			PersistentContents: tmpl.Spec.PersistentContents,
			EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
				Template:             tmpl.Spec.Template,
				VolumeClaimTemplates: tmpl.Spec.VolumeClaimTemplates,
			},
		},
	}, cache, client.SandboxClient)
	if opts.Modifier != nil {
		opts.Modifier(sbx)
	}
	labels := sbx.GetLabels()
	labels[v1alpha1.LabelSandboxTemplate] = tmpl.Name
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

func createSandboxTemplate(ctx context.Context, client clients.SandboxClient, tmpl *v1alpha1.SandboxTemplate) (*v1alpha1.SandboxTemplate, error) {
	return client.ApiV1alpha1().SandboxTemplates(tmpl.Namespace).Create(ctx, tmpl, metav1.CreateOptions{})
}

func createCheckpoint(ctx context.Context, client clients.SandboxClient, cp *v1alpha1.Checkpoint) (*v1alpha1.Checkpoint, error) {
	return client.ApiV1alpha1().Checkpoints(cp.Namespace).Create(ctx, cp, metav1.CreateOptions{})
}

func CreateCheckpoint(ctx context.Context, sbx *v1alpha1.Sandbox, client clients.SandboxClient, cache *Cache, opts infra.CreateCheckpointOptions) (string, error) {
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
		},
	}
	tmpl, err := DefaultCreateSandboxTemplate(ctx, client, tmpl)
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
	cp, err = DefaultCreateCheckpoint(ctx, client, cp)
	if err != nil {
		log.Error(err, "failed to create checkpoint")
		return "", fmt.Errorf("failed to create checkpoint: %w", err)
	}
	log = log.WithValues("checkpoint", klog.KObj(cp))
	log.Info("checkpoint creating")
	if cp, err = cache.WaitForCheckpointSatisfied(ctx, cp, WaitActionCheckpoint, func(cp *v1alpha1.Checkpoint) (bool, error) {
		return checkCheckpointReady(ctx, cp)
	}, opts.WaitSuccessTimeout); err != nil {
		log.Error(err, "failed to wait checkpoint ready")
		return "", fmt.Errorf("failed to wait checkpoint ready: %w", err)
	}
	log.Info("checkpoint created")
	return cp.Status.CheckpointId, nil
}

func checkCheckpointReady(ctx context.Context, cp *v1alpha1.Checkpoint) (bool, error) {
	log := klog.FromContext(ctx).V(consts.DebugLogLevel).WithValues("checkpoint", klog.KObj(cp))
	id := cp.Status.CheckpointId
	phase := cp.Status.Phase
	msg := cp.Status.Message
	log.Info("check checkpoint status", "id", id, "phase", phase, "message", msg, "resourceVersion", cp.ResourceVersion)

	switch phase {
	case v1alpha1.CheckpointTerminating:
		fallthrough
	case v1alpha1.CheckpointFailed:
		return false, fmt.Errorf("checkpoint %s/%s failed: %s", cp.Namespace, cp.Name, msg)
	case v1alpha1.CheckpointSucceeded:
		if id == "" {
			return false, fmt.Errorf("checkpoint %s/%s has no checkpoint id", cp.Namespace, cp.Name)
		}
		return true, nil
	default:
		return false, nil
	}
}
