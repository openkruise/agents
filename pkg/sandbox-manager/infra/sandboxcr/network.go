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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils/network"
)

// sandboxOwnerRef returns an OwnerReference that points to the given Sandbox CR.
// Setting this on TrafficPolicy CRs ensures they are garbage-collected
// when the owning Sandbox is deleted (including timeout-driven deletion by the controller).
func sandboxOwnerRef(owner *agentsv1alpha1.Sandbox) metav1.OwnerReference {
	controller := true
	blockOwnerDeletion := false
	return metav1.OwnerReference{
		APIVersion:         agentsv1alpha1.GroupVersion.String(),
		Kind:               "Sandbox",
		Name:               owner.Name,
		UID:                owner.UID,
		Controller:         &controller,
		BlockOwnerDeletion: &blockOwnerDeletion,
	}
}

// buildTrafficPolicy builds a TrafficPolicy CR that encodes both CIDR/IP and
// domain rules. Domain entries use the FQDN peer field
func buildTrafficPolicy(allowOutCIDRs, allowOutDomains, denyOut []string, namespace, sandboxID string, sandbox *agentsv1alpha1.Sandbox) *agentsv1alpha1.TrafficPolicy {
	if len(allowOutCIDRs) == 0 && len(allowOutDomains) == 0 && len(denyOut) == 0 {
		return nil
	}

	hasAllowOut := len(allowOutCIDRs) > 0 || len(allowOutDomains) > 0
	rules := make([]agentsv1alpha1.TrafficPolicyRule, 0, 2)

	if hasAllowOut {
		// Whitelist mode: allow CIDR/IP and FQDN entries, then explicit deny
		allowPeers := make([]agentsv1alpha1.TrafficPolicyPeer, 0, len(allowOutCIDRs)+len(allowOutDomains))
		for _, cidr := range allowOutCIDRs {
			allowPeers = append(allowPeers, agentsv1alpha1.TrafficPolicyPeer{CIDR: cidr})
		}
		for _, fqdn := range allowOutDomains {
			allowPeers = append(allowPeers, agentsv1alpha1.TrafficPolicyPeer{FQDN: fqdn})
		}
		rules = append(rules, agentsv1alpha1.TrafficPolicyRule{
			Action: agentsv1alpha1.RuleActionAllow,
			To:     allowPeers,
		})
		// Explicit deny rules (preserved for round-trip fidelity)
		if len(denyOut) > 0 {
			denyPeers := make([]agentsv1alpha1.TrafficPolicyPeer, 0, len(denyOut))
			for _, entry := range denyOut {
				denyPeers = append(denyPeers, agentsv1alpha1.TrafficPolicyPeer{CIDR: network.NormalizeToCIDR(entry)})
			}
			rules = append(rules, agentsv1alpha1.TrafficPolicyRule{
				Action: agentsv1alpha1.RuleActionReject,
				To:     denyPeers,
			})
		}
	} else {
		// Blacklist mode: reject denyOut entries only
		denyPeers := make([]agentsv1alpha1.TrafficPolicyPeer, 0, len(denyOut))
		for _, entry := range denyOut {
			denyPeers = append(denyPeers, agentsv1alpha1.TrafficPolicyPeer{CIDR: network.NormalizeToCIDR(entry)})
		}
		rules = append(rules, agentsv1alpha1.TrafficPolicyRule{
			Action: agentsv1alpha1.RuleActionReject,
			To:     denyPeers,
		})
	}

	return &agentsv1alpha1.TrafficPolicy{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "tp-",
			Namespace:    namespace,
			Annotations: map[string]string{
				agentsv1alpha1.AnnotationSandboxID: sandboxID,
			},
			OwnerReferences: []metav1.OwnerReference{sandboxOwnerRef(sandbox)},
		},
		Spec: agentsv1alpha1.TrafficPolicySpec{
			Priority: 1000,
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					agentsv1alpha1.LabelSandboxName: sandbox.Name,
				},
			},
			Egress: &agentsv1alpha1.TrafficPolicyDirection{
				Rules: rules,
			},
		},
	}
}

// CreateNetworkPolicy creates a TrafficPolicy CR for the sandbox.
func (s *Sandbox) CreateNetworkPolicy(ctx context.Context, netConfig infra.SandboxNetworkConfig) error {
	if len(netConfig.AllowOut) == 0 && len(netConfig.DenyOut) == 0 {
		return nil
	}
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(s))
	k8sClient := s.Cache.GetClient()
	sandboxID := s.GetSandboxID()
	namespace := s.GetNamespace()

	allowCIDRs, allowDomains := network.SplitAllowOut(netConfig.AllowOut)

	tp := buildTrafficPolicy(allowCIDRs, allowDomains, netConfig.DenyOut, namespace, sandboxID, s.Sandbox)
	if tp != nil {
		if err := k8sClient.Create(ctx, tp); err != nil {
			log.Error(err, "failed to create TrafficPolicy for sandbox")
			return fmt.Errorf("failed to create TrafficPolicy: %w", err)
		}
		log.Info("TrafficPolicy created", "name", tp.Name)
	}

	return nil
}

// UpdateNetworkPolicy updates the TrafficPolicy CR for the sandbox.
func (s *Sandbox) UpdateNetworkPolicy(ctx context.Context, netConfig infra.SandboxNetworkConfig) error {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(s))
	k8sClient := s.Cache.GetClient()
	sandboxID := s.GetSandboxID()
	namespace := s.GetNamespace()

	allowCIDRs, allowDomains := network.SplitAllowOut(netConfig.AllowOut)

	// --- Reconcile TrafficPolicy ---
	tpList := &agentsv1alpha1.TrafficPolicyList{}
	if err := k8sClient.List(ctx, tpList,
		client.InNamespace(namespace),
		client.MatchingFields{cache.IndexTrafficPolicySandboxID: sandboxID},
	); err != nil {
		return fmt.Errorf("failed to list TrafficPolicies: %w", err)
	}

	newTP := buildTrafficPolicy(allowCIDRs, allowDomains, netConfig.DenyOut, namespace, sandboxID, s.Sandbox)

	if newTP == nil {
		// No network rules needed, delete existing CRs
		for i := range tpList.Items {
			tp := &tpList.Items[i]
			if err := client.IgnoreNotFound(k8sClient.Delete(ctx, tp)); err != nil {
				log.Error(err, "failed to delete TrafficPolicy", "name", tp.Name)
			} else {
				log.Info("TrafficPolicy deleted", "name", tp.Name)
			}
		}
	} else if len(tpList.Items) > 0 {
		// Update existing TrafficPolicy using merge patch to preserve external annotations.
		existing := &tpList.Items[0]
		base := existing.DeepCopy()
		existing.Spec = newTP.Spec
		existing.OwnerReferences = newTP.OwnerReferences
		if existing.Annotations == nil {
			existing.Annotations = map[string]string{}
		}
		for k, v := range newTP.Annotations {
			existing.Annotations[k] = v
		}
		if err := k8sClient.Patch(ctx, existing, client.MergeFrom(base)); err != nil {
			return fmt.Errorf("failed to update TrafficPolicy %s: %w", existing.Name, err)
		}
		log.Info("TrafficPolicy updated", "name", existing.Name)
		// Delete any extra TrafficPolicies (shouldn't happen, but clean up)
		for i := 1; i < len(tpList.Items); i++ {
			tp := &tpList.Items[i]
			if err := client.IgnoreNotFound(k8sClient.Delete(ctx, tp)); err != nil {
				log.Error(err, "failed to delete extra TrafficPolicy", "name", tp.Name)
			}
		}
	} else {
		// No existing TrafficPolicy, create new one
		if err := k8sClient.Create(ctx, newTP); err != nil {
			return fmt.Errorf("failed to create TrafficPolicy: %w", err)
		}
		log.Info("TrafficPolicy created", "name", newTP.Name)
	}

	log.Info("network CRs reconciled")
	return nil
}

// SelectNetworkPolicy queries the existing TrafficPolicy CR and returns the
// all network configuration. Both CIDR and FQDN entries are read back
// from the single TrafficPolicy CR.
func (s *Sandbox) SelectNetworkPolicy(ctx context.Context) (*infra.SandboxNetworkConfig, error) {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(s))
	k8sClient := s.Cache.GetClient()
	sandboxID := s.GetSandboxID()
	namespace := s.GetNamespace()

	config := &infra.SandboxNetworkConfig{}

	// Read TrafficPolicy to extract allowOut (CIDRs + FQDNs) and denyOut (CIDRs)
	tpList := &agentsv1alpha1.TrafficPolicyList{}
	if err := k8sClient.List(ctx, tpList,
		client.InNamespace(namespace),
		client.MatchingFields{cache.IndexTrafficPolicySandboxID: sandboxID},
	); err != nil {
		return nil, fmt.Errorf("failed to list TrafficPolicies: %w", err)
	}
	if len(tpList.Items) == 0 {
		log.Info("no network CRs found for sandbox")
		return nil, nil
	}
	tp := &tpList.Items[0]
	if tp.Spec.Egress == nil {
		log.Info("no network CRs found for sandbox")
		return nil, nil
	}
	for _, rule := range tp.Spec.Egress.Rules {
		switch rule.Action {
		case agentsv1alpha1.RuleActionAllow:
			for _, peer := range rule.To {
				if peer.CIDR != "" {
					config.AllowOut = append(config.AllowOut, peer.CIDR)
				}
				if peer.FQDN != "" {
					config.AllowOut = append(config.AllowOut, peer.FQDN)
				}
			}
		case agentsv1alpha1.RuleActionReject:
			for _, peer := range rule.To {
				if peer.CIDR != "" {
					config.DenyOut = append(config.DenyOut, peer.CIDR)
				}
			}
		}
	}

	if len(config.AllowOut) == 0 && len(config.DenyOut) == 0 {
		log.Info("no network CRs found for sandbox")
		return nil, nil
	}

	return config, nil
}
