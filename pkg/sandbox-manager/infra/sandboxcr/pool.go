package sandboxcr

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/openkruise/agents/api/v1alpha1"
	sandboxclient "github.com/openkruise/agents/client/clientset/versioned"
	"github.com/openkruise/agents/pkg/sandbox-manager/events"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils/sandbox-manager"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

type Pool struct {
	stopped     atomic.Bool
	initOnce    sync.Once
	Name        string
	Namespace   string
	Annotations map[string]string

	// Should init fields
	eventer *events.Eventer
	client  sandboxclient.Interface
	cache   Cache[*v1alpha1.Sandbox]
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
			DeleteFunc:    p.client.ApiV1alpha1().Sandboxes(p.Namespace).Delete,
			SetCondition:  SetSandboxCondition,
			GetConditions: ListSandboxConditions,
			DeepCopy:      DeepCopy,
		},
		Sandbox: sbx,
	}
}

func (p *Pool) ClaimSandbox(ctx context.Context, user string, modifier func(sbx infra.Sandbox)) (infra.Sandbox, error) {
	lock := uuid.New().String()
	log := klog.FromContext(ctx).WithValues("pool", p.Namespace+"/"+p.Name)
	for i := 0; i < 10; i++ {
		objects, err := p.cache.SelectSandboxes(v1alpha1.LabelSandboxState, v1alpha1.SandboxStateAvailable,
			v1alpha1.LabelSandboxPool, p.Name)
		if err != nil {
			return nil, err
		}
		if len(objects) == 0 {
			return nil, fmt.Errorf("no available sandboxes for template %s", p.Name)
		}
		var obj *v1alpha1.Sandbox
		for _, obj = range objects {
			if obj.Status.Phase == v1alpha1.SandboxRunning && obj.Annotations[v1alpha1.AnnotationLock] == "" {
				break
			}
		}
		if obj == nil || obj.Annotations[v1alpha1.AnnotationLock] != "" {
			return nil, fmt.Errorf("all sandboxes are locked")
		}

		// Go to Sandbox interface
		sbx := p.AsSandbox(obj.DeepCopy())
		if modifier != nil {
			modifier(sbx)
		}
		sbx.Labels[v1alpha1.LabelSandboxState] = v1alpha1.SandboxStateRunning
		err = p.LockSandbox(ctx, sbx, lock, user)
		if err == nil {
			return sbx, nil
		}
		if !apierrors.IsConflict(err) {
			log.Error(err, "failed to update pod")
			return nil, err
		}
		log.Error(err, "failed to acquire optimistic lock of pod", "retries", i+1)
	}
	return nil, fmt.Errorf("failed to acquire optimistic lock of pod after max retries")
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
