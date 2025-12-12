package sandboxcr

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/google/uuid"
	"github.com/openkruise/agents/api/v1alpha1"
	sandboxclient "github.com/openkruise/agents/client/clientset/versioned"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	utils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// AsSandbox converts the given sbx object to a Sandbox interface
// NOTE: If the sbx object is about to be updated, you may have to DeepCopy it like `s := p.AsSandbox(sbx.DeepCopy())`
func (p *Pool) AsSandbox(sbx *v1alpha1.Sandbox) *Sandbox {
	if sbx.Annotations == nil {
		sbx.Annotations = make(map[string]string)
	}
	if sbx.Labels == nil {
		sbx.Labels = make(map[string]string)
	}
	return &Sandbox{
		BaseSandbox: BaseSandbox[*v1alpha1.Sandbox]{
			Sandbox:       sbx,
			Cache:         p.cache,
			PatchSandbox:  p.client.ApiV1alpha1().Sandboxes(p.Namespace).Patch,
			UpdateStatus:  p.client.ApiV1alpha1().Sandboxes(p.Namespace).UpdateStatus,
			Update:        p.client.ApiV1alpha1().Sandboxes(p.Namespace).Update,
			DeleteFunc:    p.client.ApiV1alpha1().Sandboxes(p.Namespace).Delete,
			SetCondition:  SetSandboxCondition,
			GetConditions: ListSandboxConditions,
			DeepCopy:      DeepCopy,
		},
		Sandbox: sbx,
	}
}

var claimTimeout = 5 * time.Second

func (p *Pool) ClaimSandbox(ctx context.Context, user string, candidateCounts int, modifier func(sbx infra.Sandbox)) (infra.Sandbox, error) {
	lock := uuid.New().String()
	log := klog.FromContext(ctx).WithValues("pool", p.Namespace+"/"+p.Name)
	start := time.Now()
	r := rand.New(rand.NewSource(start.UnixNano()))
	for i := 0; ; i++ {
		objects, err := p.cache.ListAvailableSandboxes(p.Name)
		if err != nil {
			return nil, err
		}
		if len(objects) == 0 {
			return nil, fmt.Errorf("no available sandboxes for template %s (no stock)", p.Name)
		}
		var obj *v1alpha1.Sandbox
		candidates := make([]*v1alpha1.Sandbox, 0, candidateCounts)
		for _, obj = range objects {
			if !utils.ResourceVersionExpectationSatisfied(obj) {
				log.V(consts.DebugLogLevel).Info("skip out-dated sandbox cache", "sandbox", klog.KObj(obj))
				continue
			}
			if obj.Status.Phase == v1alpha1.SandboxRunning && obj.Annotations[v1alpha1.AnnotationLock] == "" {
				candidates = append(candidates, obj)
				if len(candidates) >= candidateCounts {
					break
				}
			}
		}

		if len(candidates) == 0 {
			return nil, fmt.Errorf("no available sandboxes for template %s (no candidate)", p.Name)
		}

		obj = candidates[r.Intn(len(candidates))]

		// Go to Sandbox interface
		sbx := p.AsSandbox(obj.DeepCopy())
		if modifier != nil {
			modifier(sbx)
		}
		sbx.Annotations[v1alpha1.AnnotationClaimTime] = time.Now().Format(time.RFC3339)
		if err = p.LockSandbox(ctx, sbx, lock, user); err == nil {
			utils.ResourceVersionExpectationExpect(sbx)
			log.Info("acquired optimistic lock of sandbox", "cost", time.Since(start), "retries", i)
			return sbx, nil
		}
		if !apierrors.IsConflict(err) {
			log.Error(err, "failed to update sandbox")
			return nil, err
		}
		log.Error(err, "failed to acquire optimistic lock of sandbox", "retries", i+1)
		if time.Since(start) > claimTimeout {
			break
		}
	}
	return nil, fmt.Errorf("no available sandboxes for template %s (failed to acquire optimistic lock of pod after max retries)", p.Name)
}

func (p *Pool) LockSandbox(ctx context.Context, sbx *Sandbox, lock string, owner string) error {
	utils.LockSandbox(sbx.Sandbox, lock, owner)
	updated, err := p.client.ApiV1alpha1().Sandboxes(sbx.Namespace).Update(ctx, sbx.Sandbox, metav1.UpdateOptions{})
	if err == nil {
		sbx.Sandbox = updated
		return nil
	}
	return err
}

func (p *Pool) GetAnnotations() map[string]string {
	return p.Annotations
}

func (p *Pool) GetName() string {
	return p.Name
}
