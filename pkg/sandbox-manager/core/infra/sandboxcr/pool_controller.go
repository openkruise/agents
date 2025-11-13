package sandboxcr

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/klog/v2"
)

// EnqueueRequest 模拟 controller-runtime 的 work queue，用于触发 Reconcile
func (p *Pool) EnqueueRequest(ctx context.Context) {
	log := klog.FromContext(ctx).WithValues("pool", p.template.Name).V(consts.DebugLogLevel)
	if p.stopped.Load() {
		klog.FromContext(ctx).Info("reconcile loop is stopped", "pool", p.template.Name)
		return
	}
	select {
	case p.reconcileQueue <- ctx:
		log.Info("reconcile loop triggered")
	default:
		log.Info("reconcile queue is full, skip this reconcile loop")
	}
}

func (p *Pool) Run() {
	klog.InfoS("start reconcile loop", "pool", p.template.Name)
	for ctx := range p.reconcileQueue {
		log := klog.FromContext(ctx).WithValues("pool", p.template.Name)
		log.Info("start reconcile sandbox pool")
		err := p.Reconcile(ctx)
		if err != nil {
			log.Error(err, "reconcile failed")
		}
	}
	klog.InfoS("reconcile loop stopped")
}

func (p *Pool) Stop() {
	p.stopped.Store(true)
	time.Sleep(100 * time.Millisecond)
	close(p.reconcileQueue)
}

// Reconcile 在内存中模拟 SandboxPool 的调协，实现弹性伸缩
func (p *Pool) Reconcile(ctx context.Context) error {
	log := klog.FromContext(ctx).WithValues("pool", p.template.Name)
	sandboxes, err := p.cache.SelectSandboxes(consts.LabelSandboxPool, p.template.Name)
	if err != nil {
		return err
	}
	groups, err := p.GroupAllSandboxes(ctx, sandboxes)
	if err != nil {
		log.Error(err, "failed to group sandboxes")
		return err
	}

	// save status
	actualReplicas := p.SaveStatusFromGroup(groups)
	log.Info("status saved", "actualReplicas", actualReplicas,
		"total", p.Status.total.Load(), "pending", p.Status.pending.Load(),
		"claimed", p.Status.claimed.Load(), "creating", p.Status.creating.Load())
	expectReplicas := p.Spec.Replicas.Load()

	// 并发执行伸缩和 GC，两者之间没有依赖关系
	var wg sync.WaitGroup
	wg.Add(2)
	var scaleErr error
	go func(ctx context.Context) {
		defer wg.Done()
		log := klog.FromContext(ctx).WithValues("pool", p.template.Name)
		if scaleErr = p.performScale(ctx, groups, int(expectReplicas), actualReplicas); scaleErr != nil {
			log.Error(scaleErr, "failed to perform scale")
		}
		log.V(consts.DebugLogLevel).Info("scale finished")
	}(ctx)
	var gcError error
	go func(ctx context.Context) {
		defer wg.Done()
		log := klog.FromContext(ctx).WithValues("pool", p.template.Name)
		if gcError = p.performGC(ctx, groups); gcError != nil {
			log.Error(gcError, "failed to perform gc")
		}
		log.V(consts.DebugLogLevel).Info("gc finished")
	}(ctx)
	wg.Wait()
	return errors.Join(scaleErr, gcError)
}

func (p *Pool) SaveStatusFromGroup(groups GroupedSandboxes) (actualReplicas int) {
	creatingReplicas := len(groups.Creating)
	pendingReplicas := len(groups.Pending)
	claimedReplicas := len(groups.Claimed)
	availableReplicas := pendingReplicas + claimedReplicas
	actualReplicas = creatingReplicas + availableReplicas
	p.Status.claimed.Store(int32(claimedReplicas))
	p.Status.pending.Store(int32(pendingReplicas))
	p.Status.creating.Store(int32(creatingReplicas))
	p.Status.total.Store(int32(actualReplicas))
	return actualReplicas
}

func (p *Pool) GroupAllSandboxes(ctx context.Context, sandboxes []*v1alpha1.Sandbox) (GroupedSandboxes, error) {
	log := klog.FromContext(ctx).WithValues("pool", p.template.Name)
	templateHash := p.template.Labels[consts.LabelTemplateHash]
	groups := GroupedSandboxes{}
	for _, s := range sandboxes {
		debugLog := log.V(consts.DebugLogLevel).WithValues("sandbox", s.Name)
		group, reason := FindSandboxGroup(s, templateHash)
		sbx := p.AsSandbox(s)
		switch group {
		case GroupCreating:
			groups.Creating = append(groups.Creating, sbx)
		case GroupPending:
			groups.Pending = append(groups.Pending, sbx)
		case GroupClaimed:
			groups.Claimed = append(groups.Claimed, sbx)
		case GroupFailed:
			groups.Failed = append(groups.Failed, sbx)
		default: // unknown
			return GroupedSandboxes{}, fmt.Errorf("cannot find group for sandbox %s", sbx.Name)
		}
		debugLog.Info("sandbox is grouped", "group", group, "reason", reason)
	}
	log.Info("sandbox group done", "total", len(sandboxes),
		"creating", len(groups.Creating), "pending", len(groups.Pending),
		"claimed", len(groups.Claimed), "failed", len(groups.Failed))
	return groups, nil
}

func (p *Pool) performScale(ctx context.Context, groups GroupedSandboxes, expectReplicas, actualReplicas int) error {
	log := klog.FromContext(ctx).WithValues("pool", p.template.Name).V(consts.DebugLogLevel)
	if offset := expectReplicas - actualReplicas; offset > 0 {
		// 执行扩容
		for offset > 0 {
			created, err := p.createSandbox(ctx)
			if err != nil {
				log.Error(err, "failed to create sandbox")
				return err
			}
			log.Info("sandbox created", "sandbox", klog.KObj(created))
			offset--
		}
	} else {
		// 执行缩容，只缩未分配与创建中的 Sandbox
		for _, sbx := range append(groups.Pending, groups.Creating...) {
			if offset >= 0 {
				break
			}
			err := p.client.ApiV1alpha1().Sandboxes(sbx.Namespace).Delete(ctx, sbx.Name, metav1.DeleteOptions{})
			if err != nil {
				log.Error(err, "failed to delete sandbox")
				return err
			}
			log.Info("sandbox deleted", "sandbox", klog.KObj(sbx))
			offset++
		}
	}
	return nil
}

func (p *Pool) performGC(ctx context.Context, groups GroupedSandboxes) error {
	log := klog.FromContext(ctx).WithValues("pool", p.template.Name).V(consts.DebugLogLevel)
	failNum := 0
	for _, sbx := range groups.Failed {
		if sbx.DeletionTimestamp != nil {
			continue
		}
		err := p.client.ApiV1alpha1().Sandboxes(sbx.Namespace).Delete(ctx, sbx.Name, metav1.DeleteOptions{})
		if err != nil {
			log.Error(err, "failed to delete sandbox")
			failNum++
		}
		log.Info("sandbox deleted", "sandbox", klog.KObj(sbx))
	}
	if failNum > 0 {
		return fmt.Errorf("failed to delete %d sandboxes", failNum)
	}
	return nil
}

func (p *Pool) createSandbox(ctx context.Context) (*v1alpha1.Sandbox, error) {
	var err error
	var sbx *v1alpha1.Sandbox
	for i := 0; i < 10; i++ {
		generatedName := fmt.Sprintf("%s-%s-%s",
			p.template.Name, p.template.Labels[consts.LabelTemplateHash], rand.String(5))
		instance := &v1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:        generatedName,
				Namespace:   p.template.Namespace,
				Labels:      p.template.Spec.Template.Labels,
				Annotations: p.template.Spec.Template.Annotations,
			},
			Spec: v1alpha1.SandboxSpec{
				Template: p.template.Spec.Template,
			},
		}
		sbx, err = p.client.ApiV1alpha1().Sandboxes(p.template.Namespace).Create(ctx, instance, metav1.CreateOptions{})
		if err == nil || !apierrors.IsAlreadyExists(err) {
			break
		}
	}
	return sbx, err
}
