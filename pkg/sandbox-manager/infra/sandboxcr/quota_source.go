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

	toolscache "k8s.io/client-go/tools/cache"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils"
)

func (i *Infra) GetQuotaSandboxSource() infra.QuotaSandboxSource {
	return i
}

func (i *Infra) ListLiveQuotaSandboxesByOwner(ctx context.Context, owner string) ([]infra.QuotaSandboxSnapshot, error) {
	sandboxes, err := i.Cache.ListLiveSandboxesByOwner(ctx, owner)
	if err != nil {
		return nil, err
	}
	out := make([]infra.QuotaSandboxSnapshot, 0, len(sandboxes))
	for _, sbx := range sandboxes {
		snapshot, ok := quotaSnapshotFromSandbox(sbx)
		if ok && snapshot.Live {
			out = append(out, snapshot)
		}
	}
	return out, nil
}

func (i *Infra) Subscribe(ctx context.Context, fn func(infra.QuotaSandboxEvent)) (infra.QuotaSandboxSubscription, error) {
	reg, err := i.Cache.AddSandboxEventHandler(ctx, toolscache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if event, ok := quotaEventFromObject(obj, false); ok {
				fn(event)
			}
		},
		UpdateFunc: func(oldObj, newObj any) {
			if event, ok := quotaEventFromUpdateObject(oldObj, newObj); ok {
				fn(event)
			}
		},
		DeleteFunc: func(obj any) {
			if event, ok := quotaEventFromObject(obj, true); ok {
				fn(event)
			}
		},
	})
	if err != nil {
		return nil, err
	}
	return quotaSandboxSubscription{reg: reg}, nil
}

func (i *Infra) Healthy() bool {
	return i.Cache.SandboxInformerHealthy()
}

type quotaSandboxSubscription struct {
	reg cache.SandboxEventHandlerRegistration
}

func (s quotaSandboxSubscription) Remove() error {
	if s.reg == nil {
		return nil
	}
	return s.reg.Remove()
}

func quotaEventFromObject(obj any, deleted bool) (infra.QuotaSandboxEvent, bool) {
	sbx := sandboxFromQuotaEvent(obj, deleted)
	snapshot, ok := quotaSnapshotFromSandbox(sbx)
	if !ok {
		return infra.QuotaSandboxEvent{}, false
	}
	return infra.QuotaSandboxEvent{Snapshot: snapshot, Deleted: deleted}, true
}

func quotaEventFromUpdateObject(oldObj, newObj any) (infra.QuotaSandboxEvent, bool) {
	event, ok := quotaEventFromObject(newObj, false)
	if !ok {
		return infra.QuotaSandboxEvent{}, false
	}
	oldSnapshot, oldOK := quotaSnapshotFromSandbox(sandboxFromQuotaEvent(oldObj, false))
	if oldOK && oldSnapshot == event.Snapshot {
		return infra.QuotaSandboxEvent{}, false
	}
	return event, true
}

func sandboxFromQuotaEvent(obj any, deleted bool) *v1alpha1.Sandbox {
	switch v := obj.(type) {
	case *v1alpha1.Sandbox:
		return v
	case toolscache.DeletedFinalStateUnknown:
		sbx, ok := v.Obj.(*v1alpha1.Sandbox)
		if !ok && deleted {
			quotaSourceEventDropTotal.WithLabelValues("invalid_tombstone").Inc()
		}
		return sbx
	default:
		return nil
	}
}

func quotaSnapshotFromSandbox(sbx *v1alpha1.Sandbox) (infra.QuotaSandboxSnapshot, bool) {
	if sbx == nil {
		return infra.QuotaSandboxSnapshot{}, false
	}
	live := utils.IsLiveForQuota(sbx)
	return infra.QuotaSandboxSnapshot{
		Owner:      sbx.GetAnnotations()[v1alpha1.AnnotationOwner],
		LockString: quotaLockStringOf(sbx),
		Resource:   quotaSandboxResourceOf(sbx),
		Live:       live,
		// Do not change this predicate during the refactor.
		Running: live && !sbx.Spec.Paused,
	}, true
}

func quotaLockStringOf(sbx *v1alpha1.Sandbox) string {
	if sbx == nil {
		return ""
	}
	return sbx.GetAnnotations()[v1alpha1.AnnotationLock]
}

func quotaSandboxResourceOf(sbx *v1alpha1.Sandbox) infra.SandboxResource {
	if sbx == nil || sbx.Spec.Template == nil {
		return infra.SandboxResource{}
	}
	return infra.CalculateResourceFromContainers(sbx.Spec.Template.Spec.Containers)
}
