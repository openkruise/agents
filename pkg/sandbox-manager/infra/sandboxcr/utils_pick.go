package sandboxcr

import (
	"context"
	"math/rand"
	"sync"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	utils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func GetPickKey(sbx *v1alpha1.Sandbox) string {
	return client.ObjectKeyFromObject(sbx).String()
}

func PickAnAvailableSandbox(ctx context.Context, pool string, cnt int, r *rand.Rand, pickCache *sync.Map,
	cache *Cache, client clients.SandboxClient) (*Sandbox, error) {
	log := klog.FromContext(ctx).WithValues("pool", pool).V(consts.DebugLogLevel)
	objects, err := cache.ListAvailableSandboxes(pool)
	if err != nil {
		return nil, err
	}
	if len(objects) == 0 {
		return nil, NoAvailableError(pool, "no stock")
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
		return nil, NoAvailableError(pool, "no candidate")
	}
	start := r.Intn(len(candidates))
	i := start
	for {
		obj = candidates[i]
		key := GetPickKey(obj)
		if _, loaded := pickCache.LoadOrStore(key, struct{}{}); !loaded {
			return AsSandbox(obj, cache, client), nil
		}
		log.Info("candidate picked by another request", "key", key)
		i = (i + 1) % len(candidates)
		if i == start {
			return nil, NoAvailableError(pool, "all candidates are picked")
		}
	}
}
