package k8s

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/events"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/utils"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

type GroupedSandboxes struct {
	Creating []*corev1.Pod // 容器实例正在创建中的 Sandbox
	Pending  []*corev1.Pod // 容器实例已经就绪，但是未被消费的池化 Sandbox
	Running  []*corev1.Pod // 已经被分配给用户的 Sandbox
	Paused   []*corev1.Pod // 已经被分配给用户的休眠 Sandbox
	Failed   []*corev1.Pod // 由于各种原因需要被删除的 Sandbox，包含删除中的对象
}

type Pool struct {
	total    atomic.Int32 // total pods in deployment
	pending  atomic.Int32 // pods available to be claimed
	running  atomic.Int32 // claimed running sandboxes
	paused   atomic.Int32 // claimed paused sandboxes
	creating atomic.Int32
	initOnce sync.Once

	// Should init fields
	eventer  *events.Eventer
	template *infra.SandboxTemplate
	client   kubernetes.Interface
	cache    *Cache
}

func (p *Pool) GetTemplate() *infra.SandboxTemplate {
	if p.template == nil {
		return &infra.SandboxTemplate{}
	}
	return p.template
}

func (p *Pool) SyncWithCluster(ctx context.Context) error {
	log := klog.FromContext(ctx)
	t := p.template
	expect := ParseTemplateAsDeployment(t)

	start := time.Now()
	for {
		if time.Since(start) > time.Minute {
			return fmt.Errorf("sync with deployment %s/%s timeout", t.Namespace, t.Name)
		}
		got, err := p.client.AppsV1().Deployments(t.Namespace).Get(ctx, t.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			log.Info("will create cluster pool", "deployment", klog.KObj(t))
			got, err = p.client.AppsV1().Deployments(t.Namespace).Create(ctx, expect, metav1.CreateOptions{})
		}
		if err != nil {
			return err
		}
		if got.Labels[consts.LabelSandboxPool] != expect.Labels[consts.LabelSandboxPool] {
			log.Info("non sandbox pool deployment already exists", "deployment", klog.KObj(t),
				"gotLabel", got.Labels[consts.LabelSandboxPool], "expectLabel", expect.Labels[consts.LabelSandboxPool])
			return fmt.Errorf("non sandbox pool deployment %s/%s already exists", t.Namespace, t.Name)
		}
		expect.Spec.Replicas = got.Spec.Replicas
		if *expect.Spec.Replicas < t.Spec.MinPoolSize {
			expect.Spec.Replicas = &t.Spec.MinPoolSize
		}
		if *expect.Spec.Replicas > t.Spec.MaxPoolSize {
			expect.Spec.Replicas = &t.Spec.MaxPoolSize
		}
		log.Info("will update sandbox pool to latest", "deployment", klog.KObj(t))
		_, err = p.client.AppsV1().Deployments(t.Namespace).Update(ctx, expect, metav1.UpdateOptions{})
		if err != nil {
			log.Error(err, "failed to update sandbox pool, will retry", "deployment", klog.KObj(t))
			time.Sleep(time.Second)
			continue
		}
		log.Info("sandbox pool updated, check it in cache", "deployment", klog.KObj(t))
		_, err = p.cache.GetDeployment(got.Name)
		if err != nil {
			log.Error(err, "failed to get deployment from cache, will retry", "deployment", klog.KObj(t))
			time.Sleep(time.Second)
			continue
		}
		break
	}
	return p.Refresh(ctx)
}

func (p *Pool) Refresh(ctx context.Context) error {
	start := time.Now()
	log := klog.FromContext(ctx).V(consts.DebugLogLevel)
	log.Info("refreshing sandbox pool", "pool", klog.KObj(p.template))
	t := p.template
	if t == nil {
		return errors.New("pool template is not set")
	}
	deploy, err := p.cache.GetDeployment(t.Name)
	if err != nil {
		return err
	}
	total := deploy.Status.Replicas
	p.total.Store(total)

	pods, err := p.cache.SelectPods(consts.LabelSandboxPool, t.Name)
	if err != nil {
		log.Error(err, "failed to select pods for sandbox pool")
	}

	groups, err := p.GroupAllSandboxes(ctx, pods)
	if err != nil {
		log.Error(err, "failed to group sandboxes")
		return err
	}

	pending := int32(len(groups.Pending))
	running := int32(len(groups.Running))
	paused := int32(len(groups.Paused))
	creating := int32(len(groups.Creating))

	p.pending.Store(pending)
	p.running.Store(running)
	p.paused.Store(paused)
	p.creating.Store(creating)

	p.initOnce.Do(func() {
		managedPods := make([]*corev1.Pod, 0, pending+running+paused+creating)
		managedPods = append(managedPods, groups.Pending...)
		managedPods = append(managedPods, groups.Running...)
		managedPods = append(managedPods, groups.Paused...)
		managedPods = append(managedPods, groups.Creating...)
		for _, pod := range managedPods {
			log.Info("will retrigger managed pod", "pod", klog.KObj(pod))
			switch pod.Status.Phase {
			case corev1.PodRunning:
				go p.eventer.Trigger(events.Event{
					Type:    consts.SandboxCreated,
					Sandbox: p.AsSandbox(pod),
					Source:  "ResourcePoolInitRefresh",
					Message: "Retrigger SandboxCreated event only once when starting",
				})
			default:
				log.Info("unexpected pod phase", "phase", pod.Status.Phase)
			}
		}
	})
	log.Info("sandbox pool refreshed", "pool", t.Name, "total", total, "cost", time.Since(start),
		"pending", pending, "running", running, "paused", paused)
	return p.gcFinishedPods(ctx, groups.Failed)
}

func (p *Pool) ClaimSandbox(ctx context.Context, user string, modifier func(sbx infra.Sandbox)) (infra.Sandbox, error) {
	if p.pending.Load() == 0 {
		return nil, fmt.Errorf("no pending sandboxes for template %s", p.template.Name)
	}
	lock := uuid.New().String()
	for i := 0; i < 10; i++ {
		pods, err := p.cache.SelectPods(consts.LabelSandboxState, consts.SandboxStatePending,
			consts.LabelSandboxPool, p.template.Name)
		if err != nil {
			return nil, err
		}
		if len(pods) == 0 {
			return nil, fmt.Errorf("cannot find pending sandboxes for template %s", p.template.Name)
		}
		var pod *corev1.Pod
		for _, pod = range pods {
			if pod.Status.Phase == corev1.PodRunning && pod.Annotations[consts.AnnotationLock] == "" {
				break
			}
		}
		if pod == nil || pod.Annotations[consts.AnnotationLock] != "" {
			return nil, fmt.Errorf("all sandboxes are locked")
		}

		// Go to Sandbox interface
		sbx := p.AsSandbox(pod.DeepCopy())
		if modifier != nil {
			modifier(sbx)
		}
		sbx.Labels[consts.LabelSandboxState] = consts.SandboxStateRunning
		sbx.Annotations[consts.AnnotationLock] = lock
		sbx.Annotations[consts.AnnotationPodDeletionCost] = "100"
		sbx.Annotations[consts.AnnotationOwner] = user
		updated, err := p.client.CoreV1().Pods(sbx.Namespace).Update(ctx, sbx.Pod, metav1.UpdateOptions{})
		if err == nil {
			go func() {
				p.addState(consts.SandboxStatePending, -1)
				p.addState(consts.SandboxStateRunning, 1)
				if err := p.Scale(ctx); err != nil {
					klog.ErrorS(err, "failed to scale pool after pod update", "pool", klog.KObj(p.template))
				}
			}()
			sbx.Pod = updated
			return sbx, nil
		}
		klog.ErrorS(err, "failed to acquire optimistic lock of pod", "pool", klog.KObj(p.template), "retries", i+1)
	}
	return nil, fmt.Errorf("failed to acquire optimistic lock of pod after max retries")
}

func (p *Pool) onDeploymentStatusUpdate(deploy *appsv1.Deployment) {
	p.total.Store(deploy.Status.Replicas)
}

func (p *Pool) gcFinishedPods(ctx context.Context, pods []*corev1.Pod) error {
	if len(pods) == 0 {
		return nil
	}
	log := klog.FromContext(ctx).V(consts.DebugLogLevel)
	var errList error
	for _, pod := range pods {
		err := p.client.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
		log.Info("GC pod done", "pod", klog.KObj(pod), "err", err)
		errList = errors.Join(errList, err)
	}
	return errList
}

// Scale scales the pool according to the expected usage.
func (p *Pool) Scale(ctx context.Context) error {
	if p.template == nil {
		return errors.New("pool template is not set")
	}
	log := klog.FromContext(ctx).V(consts.DebugLogLevel).WithValues("pool", p.template.Name)
	total, pending, creating := p.total.Load(), p.pending.Load(), p.creating.Load()
	expectTotal, err := utils.CalculateExpectPoolSize(ctx, total-creating, pending, p.template)
	if err != nil {
		log.Error(err, "calculate expect pool size failed")
		return err
	}
	if expectTotal == total {
		log.Info("no need to scale pool", "total", total)
		return nil
	}
	log.Info("will scale pool", "total", total, "expectTotal", expectTotal)
	patchData := []byte(fmt.Sprintf(`{"spec":{"replicas":%d}}`, expectTotal))
	_, err = p.client.AppsV1().Deployments(p.template.Namespace).Patch(ctx, p.template.Name,
		types.StrategicMergePatchType, patchData, metav1.PatchOptions{})
	return err
}

func (p *Pool) onPodUpdate(ctx context.Context, oldPod, newPod *corev1.Pod) {
	oldState := oldPod.Labels[consts.LabelSandboxState]
	newState := newPod.Labels[consts.LabelSandboxState]
	if oldState != "" && newState != "" && oldState == newState {
		return
	}
	if newState == consts.SandboxStatePending || oldState == consts.SandboxStatePending {
		klog.InfoS("on pod update scale")
		_ = p.Scale(ctx)
	}
	newCond, _ := GetPodCondition(newPod, corev1.PodReady)
	oldCond, _ := GetPodCondition(oldPod, corev1.PodReady)
	if newCond.Status == corev1.ConditionTrue && oldCond.Status != corev1.ConditionTrue {
		klog.InfoS("pod ready, will trigger SandboxCreated event", "pod", klog.KObj(newPod))
		go p.eventer.Trigger(events.Event{
			Type:    consts.SandboxCreated,
			Sandbox: p.AsSandbox(newPod),
			Source:  "ResourcePoolOnPodUpdate",
			Message: "SandboxCreated event",
		})
	}
	if oldState != newState {
		klog.InfoS("pod state changed, will refresh pool", "pod", klog.KObj(newPod), "oldState", oldState, "newState", newState)
		if err := p.Refresh(ctx); err != nil {
			klog.ErrorS(err, "Failed to refresh pool after pod update", "pool", klog.KObj(p.template))
		}
	}
}

func (p *Pool) onPodDelete(pod *corev1.Pod) {
	go p.eventer.Trigger(events.Event{
		Type:    consts.SandboxKill,
		Sandbox: p.AsSandbox(pod),
		Source:  "SandboxPool",
		Message: "Pod deletion detected",
	})
	p.addState(pod.Labels[consts.LabelSandboxState], -1)
}

func (p *Pool) addState(state string, delta int32) {
	switch state {
	case consts.SandboxStatePending:
		p.pending.Add(delta)
	case consts.SandboxStateRunning:
		p.running.Add(delta)
	case consts.SandboxStatePaused:
		p.paused.Add(delta)
	default:
		return
	}
}

func (p *Pool) AsSandbox(pod *corev1.Pod) *Sandbox {
	return &Sandbox{
		Pod:    pod,
		Client: p.client,
		Cache:  p.cache,
	}
}

func (p *Pool) GroupAllSandboxes(ctx context.Context, pods []*corev1.Pod) (GroupedSandboxes, error) {
	log := klog.FromContext(ctx).WithValues("pool", p.template.Name)
	groups := GroupedSandboxes{}
	var unknownSandboxes []*Sandbox
	for _, pod := range pods {
		debugLog := log.V(consts.DebugLogLevel).WithValues("pod", pod.Name)
		group, reason := FindSandboxGroup(pod)
		switch group {
		case GroupCreating:
			groups.Creating = append(groups.Creating, pod)
		case GroupPending:
			groups.Pending = append(groups.Pending, pod)
		case GroupRunning:
			groups.Running = append(groups.Running, pod)
		case GroupPaused:
			groups.Paused = append(groups.Paused, pod)
		case GroupFailed:
			groups.Failed = append(groups.Failed, pod)
		default: // unknown
			return GroupedSandboxes{}, fmt.Errorf("cannot find group for pod %s", pod.Name)
		}
		debugLog.Info("sandbox is grouped", "group", group, "reason", reason)
	}
	log.Info("sandbox group done", "total", len(pods),
		"creating", len(groups.Creating), "pending", len(groups.Pending),
		"running", len(groups.Running), "paused", len(groups.Paused), "failed", len(groups.Failed), "unknown", len(unknownSandboxes))
	return groups, nil
}
