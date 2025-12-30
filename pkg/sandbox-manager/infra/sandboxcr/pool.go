package sandboxcr

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/openkruise/agents/api/v1alpha1"
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	sandboxclient "github.com/openkruise/agents/client/clientset/versioned"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	utils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	stateutils "github.com/openkruise/agents/pkg/utils/sandboxutils"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Pool struct {
	Name        string
	Namespace   string
	Annotations map[string]string

	// Should init fields
	client sandboxclient.Interface
	cache  *Cache

	pickCache sync.Map
}

type retriableError struct {
	Message string
}

func (e retriableError) Error() string {
	return e.Message
}

func (e retriableError) Is(target error) bool {
	as := retriableError{}
	if !errors.As(target, &as) {
		return false
	}
	return as.Message == e.Message
}

func NoAvailableError(template, reason string) error {
	return retriableError{Message: fmt.Sprintf("no available sandboxes for template %s (%s)", template, reason)}
}

// ClaimSandbox claims a Sandbox CR as Sandbox from SandboxSet
func (p *Pool) ClaimSandbox(ctx context.Context, user string, candidateCounts int, opts infra.ClaimSandboxOptions) (infra.Sandbox, error) {
	lock := uuid.New().String()
	log := klog.FromContext(ctx).WithValues("pool", p.Namespace+"/"+p.Name)
	r := rand.New(rand.NewSource(time.Now().UnixNano()))

	retries := -1
	var claimedSandbox infra.Sandbox
	return claimedSandbox, retry.OnError(wait.Backoff{
		Steps:    int(LockTimeout / RetryInterval),
		Duration: RetryInterval,
		Cap:      LockTimeout,
		Factor:   LockBackoffFactor,
		Jitter:   LockJitter,
	}, func(err error) bool {
		// Conflict: optimistic locking failed; retriableError: retriable error
		return apierrors.IsConflict(err) || errors.As(err, &retriableError{})
	}, func() error {
		retries++
		log.Info("try to claim sandbox", "retries", retries)

		sbx, err := p.pickAnAvailableSandbox(ctx, candidateCounts, r)
		if err != nil {
			log.Error(err, "failed to select available sandbox")
			return err
		}
		defer p.pickCache.Delete(getPickKey(sbx.Sandbox))

		claimLog := log.WithValues("sandbox", klog.KObj(sbx.Sandbox))
		claimLog.Info("sandbox picked")

		if err = p.modifyPickedSandbox(ctx, sbx, opts); err != nil {
			claimLog.Error(err, "failed to modify picked sandbox")
			return retriableError{Message: fmt.Sprintf("failed to modify picked sandbox: %s", err)}
		}

		if err = p.lockSandbox(ctx, sbx, lock, user); err != nil {
			claimLog.Error(err, "failed to lock sandbox")
			return err
		}
		utils.ResourceVersionExpectationExpect(sbx)
		claimLog.Info("sandbox locked")

		if opts.Image != "" {
			updateStart := time.Now()
			claimLog.Info("waiting for inplace update", "oldImage", sbx.GetImage(), "newImage", opts.Image)
			if err = p.waitForInplaceUpdate(ctx, sbx, InplaceUpdateTimeout); err != nil {
				claimLog.Error(err, "failed to wait for inplace update")
				return err
			}
			claimLog.Info("inplace update completed", "cost", time.Since(updateStart))
		}
		claimedSandbox = sbx
		return nil
	})
}

func getPickKey(sbx *v1alpha1.Sandbox) string {
	return client.ObjectKeyFromObject(sbx).String()
}

func (p *Pool) pickAnAvailableSandbox(ctx context.Context, cnt int, r *rand.Rand) (*Sandbox, error) {
	log := klog.FromContext(ctx).WithValues("pool", p.Namespace+"/"+p.Name).V(consts.DebugLogLevel)
	objects, err := p.cache.ListAvailableSandboxes(p.Name)
	if err != nil {
		return nil, err
	}
	if len(objects) == 0 {
		return nil, NoAvailableError(p.Name, "no stock")
	}
	var obj *v1alpha1.Sandbox
	candidates := make([]*v1alpha1.Sandbox, 0, cnt)
	for _, obj = range objects {
		if !utils.ResourceVersionExpectationSatisfied(obj) {
			log.Info("skip out-dated sandbox cache", "sandbox", klog.KObj(obj))
			continue
		}
		if obj.Status.Phase == v1alpha1.SandboxRunning && obj.Annotations[v1alpha1.AnnotationLock] == "" {
			candidates = append(candidates, obj)
			if len(candidates) >= cnt {
				break
			}
		}
	}
	if len(candidates) == 0 {
		return nil, NoAvailableError(p.Name, "no candidate")
	}
	start := r.Intn(len(candidates))
	i := start
	for {
		obj = candidates[i]
		key := getPickKey(obj)
		if _, loaded := p.pickCache.LoadOrStore(key, struct{}{}); !loaded {
			return AsSandbox(obj, p.cache, p.client), nil
		}
		log.Info("candidate picked by another request", "key", key)
		i = (i + 1) % len(candidates)
		if i == start {
			return nil, NoAvailableError(p.Name, "all candidates are picked")
		}
	}
}

func (p *Pool) modifyPickedSandbox(ctx context.Context, sbx *Sandbox, opts infra.ClaimSandboxOptions) error {
	if err := sbx.InplaceRefresh(ctx, true); err != nil {
		return err
	}
	if opts.Modifier != nil {
		opts.Modifier(sbx)
	}
	if opts.Image != "" {
		// should perform an inplace update
		sbx.SetImage(opts.Image)
	}
	// claim sandbox
	sbx.SetOwnerReferences([]metav1.OwnerReference{}) // make SandboxSet scale up
	labels := sbx.GetLabels()
	if labels == nil {
		labels = make(map[string]string, 1)
	}
	labels[agentsv1alpha1.LabelSandboxIsClaimed] = "true"
	sbx.SetLabels(labels)

	sbx.Annotations[v1alpha1.AnnotationClaimTime] = time.Now().Format(time.RFC3339)
	return nil
}

func (p *Pool) lockSandbox(ctx context.Context, sbx *Sandbox, lock string, owner string) error {
	utils.LockSandbox(sbx.Sandbox, lock, owner)
	updated, err := p.client.ApiV1alpha1().Sandboxes(sbx.Namespace).Update(ctx, sbx.Sandbox, metav1.UpdateOptions{})
	if err == nil {
		sbx.Sandbox = updated
		return nil
	}
	return err
}

func (p *Pool) waitForInplaceUpdate(ctx context.Context, sbx *Sandbox, timeout time.Duration) error {
	return p.cache.WaitForSandboxSatisfied(ctx, sbx.Sandbox, WaitActionInplaceUpdate, func(sbx *v1alpha1.Sandbox) (bool, error) {
		return p.checkSandboxInplaceUpdate(ctx, sbx)
	}, timeout)
}

func (p *Pool) checkSandboxInplaceUpdate(ctx context.Context, sbx *v1alpha1.Sandbox) (bool, error) {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx)).V(consts.DebugLogLevel)
	if sbx.Status.ObservedGeneration != sbx.Generation {
		log.Info("watched sandbox not updated", "generation", sbx.Generation, "observedGeneration", sbx.Status.ObservedGeneration)
		return false, nil
	}
	cond := GetSandboxCondition(sbx, v1alpha1.SandboxConditionReady)
	if cond.Reason == v1alpha1.SandboxReadyReasonStartContainerFailed {
		err := retriableError{Message: fmt.Sprintf("sandbox inplace update failed: %s", cond.Message)}
		log.Error(err, "sandbox inplace update failed")
		if p.Annotations[v1alpha1.AnnotationReserveFailedSandbox] != v1alpha1.True {
			go func() {
				err := p.client.ApiV1alpha1().Sandboxes(sbx.Namespace).Delete(context.Background(), sbx.Name, metav1.DeleteOptions{})
				if err != nil {
					log.Error(err, "failed to delete failed sandbox")
				} else {
					log.Info("sandbox deleted")
				}
			}()
		}
		return false, err // stop early
	}
	state, reason := stateutils.GetSandboxState(sbx)
	log.Info("sandbox update watched", "state", state, "reason", reason)
	return state == v1alpha1.SandboxStateRunning, nil
}

func (p *Pool) GetAnnotations() map[string]string {
	return p.Annotations
}

func (p *Pool) GetName() string {
	return p.Name
}
