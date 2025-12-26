package sandboxcr

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/google/uuid"
	"github.com/openkruise/agents/api/v1alpha1"
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
)

type Pool struct {
	Name        string
	Namespace   string
	Annotations map[string]string

	// Should init fields
	client sandboxclient.Interface
	cache  *Cache
}

type noAvailableError struct {
	Template string
	Reason   string
}

func (e noAvailableError) Error() string {
	return fmt.Sprintf("no available sandboxes for template %s (%s)", e.Template, e.Reason)
}

func NoAvailableError(template, reason string) error {
	return noAvailableError{template, reason}
}

// ClaimSandbox configurations
const (
	LockMaxRetries       = 8
	LockBackoffFactor    = 2.0
	LockJitter           = 0.1
	InplaceUpdateTimeout = time.Minute
)

// ClaimSandbox claims a Sandbox CR as Sandbox from SandboxSet
func (p *Pool) ClaimSandbox(ctx context.Context, user string, candidateCounts int, opts infra.ClaimSandboxOptions) (infra.Sandbox, error) {
	lock := uuid.New().String()
	log := klog.FromContext(ctx).WithValues("pool", p.Namespace+"/"+p.Name)
	r := rand.New(rand.NewSource(time.Now().UnixNano()))

	retries := -1
	var claimedSandbox infra.Sandbox
	return claimedSandbox, retry.OnError(wait.Backoff{
		Steps:    LockMaxRetries,
		Duration: 0,
		Factor:   LockBackoffFactor,
		Jitter:   LockJitter,
	}, func(err error) bool {
		// Conflict: optimistic locking failed; noAvailableError: retriable error
		return apierrors.IsConflict(err) || errors.As(err, &noAvailableError{})
	}, func() error {
		retries++
		log.Info("try to claim sandbox", "retries", retries)

		sbx, err := p.pickAnAvailableSandbox(ctx, candidateCounts, r)
		if err != nil {
			log.Error(err, "failed to select available sandbox")
			return err
		}
		log.Info("sandbox picked", "sandbox", klog.KObj(sbx.Sandbox))

		p.modifyPickedSandbox(sbx, opts)

		if err = p.lockSandbox(ctx, sbx, lock, user); err != nil {
			log.Error(err, "failed to lock sandbox")
			return err
		}
		utils.ResourceVersionExpectationExpect(sbx)
		log.Info("sandbox locked")

		if opts.Image != "" {
			updateStart := time.Now()
			log.Info("waiting for inplace update")
			if err = p.waitForInplaceUpdate(ctx, sbx, InplaceUpdateTimeout); err != nil {
				log.Error(err, "failed to wait for inplace update")
				return err
			}
			log.Info("inplace update completed", "cost", time.Since(updateStart))
		}
		claimedSandbox = sbx
		return nil
	})
}

func (p *Pool) pickAnAvailableSandbox(ctx context.Context, cnt int, r *rand.Rand) (*Sandbox, error) {
	log := klog.FromContext(ctx)
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
			log.V(consts.DebugLogLevel).Info("skip out-dated sandbox cache", "sandbox", klog.KObj(obj))
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
	obj = candidates[r.Intn(len(candidates))]
	return AsSandboxDeepCopy(obj, p.cache, p.client), nil
}

func (p *Pool) modifyPickedSandbox(sbx *Sandbox, opts infra.ClaimSandboxOptions) {
	if opts.Modifier != nil {
		opts.Modifier(sbx)
	}
	if opts.Image != "" {
		// should perform an inplace update
		sbx.SetImage(opts.Image)
	}
	sbx.SetOwnerReferences([]metav1.OwnerReference{}) // make SandboxSet scale up
	sbx.Annotations[v1alpha1.AnnotationClaimTime] = time.Now().Format(time.RFC3339)
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
	log := klog.FromContext(ctx).V(consts.DebugLogLevel)
	return p.cache.WaitForSandboxSatisfied(ctx, sbx.Sandbox, WaitActionInplaceUpdate, func(sbx *v1alpha1.Sandbox) (bool, error) {
		if sbx.Status.ObservedGeneration != sbx.Generation {
			log.Info("watched sandbox not updated", "generation", sbx.Generation, "observedGeneration", sbx.Status.ObservedGeneration)
			return false, nil
		}
		cond := GetSandboxCondition(sbx, v1alpha1.SandboxConditionReady)
		if cond.Reason == v1alpha1.SandboxReadyReasonInplaceUpdateFailed {
			err := fmt.Errorf("sandbox inplace update failed: %s", cond.Message)
			log.Error(err, "sandbox inplace update failed")
			return false, err // stop early
		}
		state, reason := stateutils.GetSandboxState(sbx)
		log.Info("sandbox update watched", "state", state, "reason", reason)
		return state == v1alpha1.SandboxStateRunning, nil
	}, timeout)
}

func (p *Pool) GetAnnotations() map[string]string {
	return p.Annotations
}

func (p *Pool) GetName() string {
	return p.Name
}
