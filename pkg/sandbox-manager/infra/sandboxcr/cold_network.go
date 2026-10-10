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
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

const coldNetworkSchedulingGate = "agents.kruise.io/network-policy"

func buildColdTrafficPolicy(sbx *v1alpha1.Sandbox, input *infra.EgressPolicy) *v1alpha1.TrafficPolicy {
	action := func(value string) v1alpha1.RuleAction {
		if value == "allow" {
			return v1alpha1.RuleActionAllow
		}
		return v1alpha1.RuleActionReject
	}
	// DNS is infrastructure traffic, just as the source executor exempts its
	// resolver. Keep this exception confined to the cluster DNS Service and port.
	rules := []v1alpha1.TrafficPolicyRule{{Action: v1alpha1.RuleActionAllow,
		To:    []v1alpha1.TrafficPolicyPeer{{Service: &v1alpha1.TrafficPolicyServiceRef{Name: "kube-dns", Namespace: "kube-system"}}},
		Ports: []v1alpha1.TrafficPolicyPort{{Protocol: "TCP", Port: ptr.To(int32(53))}, {Protocol: "UDP", Port: ptr.To(int32(53))}},
	}}
	for _, r := range input.Rules {
		rules = append(rules, v1alpha1.TrafficPolicyRule{Action: action(r.Action), To: []v1alpha1.TrafficPolicyPeer{{CIDR: r.CIDR, FQDN: r.FQDN}}})
	}
	// Explicit peers avoid relying on backend-dependent empty-peer semantics.
	rules = append(rules, v1alpha1.TrafficPolicyRule{Action: action(input.DefaultAction), To: []v1alpha1.TrafficPolicyPeer{{CIDR: "0.0.0.0/0"}, {CIDR: "::/0"}}})
	return &v1alpha1.TrafficPolicy{ObjectMeta: metav1.ObjectMeta{
		Name: "network-" + sbx.Name, Namespace: sbx.Namespace,
		OwnerReferences: []metav1.OwnerReference{sandboxOwnerRef(sbx)},
	}, Spec: v1alpha1.TrafficPolicySpec{Priority: e2bPerSandboxTrafficPolicyPriority,
		Selector: metav1.LabelSelector{MatchLabels: map[string]string{coldNetworkSchedulingGate: sbx.Spec.Template.Labels[coldNetworkSchedulingGate]}},
		Egress:   &v1alpha1.TrafficPolicyDirection{Rules: rules},
	}}
}

// prepareColdNetwork publishes the owned policy while the Pod is unschedulable.
// Scheduling is released only for a runtime profile with a native proxy startup
// probe; a readiness probe alone cannot stop application containers from starting.
func (s *Sandbox) prepareColdNetwork(ctx context.Context, input *infra.EgressPolicy, timeout time.Duration) error {
	c := s.Cache.GetClient()
	policy := buildColdTrafficPolicy(s.Sandbox, input)
	if err := c.Create(ctx, policy); err != nil {
		return fmt.Errorf("create cold egress policy: %w", err)
	}
	err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, timeout, true, func(ctx context.Context) (bool, error) {
		var pod corev1.Pod
		if err := c.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: s.Name}, &pod); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		owned := slices.ContainsFunc(pod.OwnerReferences, func(ref metav1.OwnerReference) bool { return ref.UID == s.UID })
		if !owned {
			return false, fmt.Errorf("network startup gate found a Pod owned by another Sandbox")
		}
		proxy := slices.IndexFunc(pod.Spec.InitContainers, func(c corev1.Container) bool { return c.Name == v1alpha1.RuntimeConfigForInjectTrafficProxy })
		if proxy < 0 {
			return false, fmt.Errorf("traffic-proxy runtime was not injected")
		}
		sidecar := pod.Spec.InitContainers[proxy]
		probe := sidecar.StartupProbe
		if sidecar.RestartPolicy == nil || *sidecar.RestartPolicy != corev1.ContainerRestartPolicyAlways || probe == nil || probe.HTTPGet == nil || probe.HTTPGet.Path != "/healthz/ready" || probe.HTTPGet.Port.IntValue() != 15021 {
			return false, fmt.Errorf("traffic-proxy runtime requires a native sidecar startupProbe on /healthz/ready:15021")
		}
		index := slices.IndexFunc(pod.Spec.SchedulingGates, func(g corev1.PodSchedulingGate) bool { return g.Name == coldNetworkSchedulingGate })
		if index < 0 {
			return false, fmt.Errorf("cold network scheduling gate is missing")
		}
		// Keep this one-time scheduling gate out of future Pod generations. The
		// policy already exists, and the current Pod remains gated until the
		// patch below. Restarts retain the native proxy startup barrier.
		var box v1alpha1.Sandbox
		if err := c.Get(ctx, client.ObjectKeyFromObject(s.Sandbox), &box); err != nil {
			return false, err
		}
		if box.UID != s.UID || box.Spec.Template == nil {
			return false, fmt.Errorf("cold network Sandbox identity changed")
		}
		gates := box.Spec.Template.Spec.SchedulingGates
		i := slices.IndexFunc(gates, func(g corev1.PodSchedulingGate) bool { return g.Name == coldNetworkSchedulingGate })
		if i >= 0 {
			base := box.DeepCopy()
			box.Spec.Template.Spec.SchedulingGates = slices.Delete(gates, i, i+1)
			if err := c.Patch(ctx, &box, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				if apierrors.IsConflict(err) {
					return false, nil
				}
				return false, err
			}
		}
		before := pod.DeepCopy()
		pod.Spec.SchedulingGates = slices.Delete(pod.Spec.SchedulingGates, index, index+1)
		err := c.Patch(ctx, &pod, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
		if apierrors.IsConflict(err) {
			return false, nil
		}
		return err == nil, err
	})
	if err != nil {
		return fmt.Errorf("prepare cold network before scheduling: %w", err)
	}
	return nil
}
