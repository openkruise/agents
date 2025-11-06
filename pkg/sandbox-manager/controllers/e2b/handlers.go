package e2b

import (
	"github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/events"
	"k8s.io/klog/v2"
)

func (sc *Controller) registerHandlers() {
	if sc.debug {
		sc.manager.RegisterHandler(consts.SandboxReady, "DebugOn", sc.debugCreateIngress, nil)
	}
}

func (sc *Controller) debugCreateIngress(evt events.Event) error {
	sandbox := evt.Sandbox
	if err := sc.createServiceAndIngressForPod(evt.Context, sandbox); err != nil {
		return err
	}
	klog.InfoS("created service and ingress for sandbox", "sandbox", klog.KObj(sandbox))
	return nil
}
