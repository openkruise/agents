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
	"testing"
	"time"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestColdNetworkStartupGate(t *testing.T) {
	for _, tt := range []struct {
		name               string
		probe, owner, gate bool
	}{
		{"ordered policy before scheduling", true, true, true},
		{"readiness alone is insufficient", false, true, true},
		{"foreign Pod cannot be released", true, false, true},
		{"missing gate fails closed", true, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			backend, c := NewTestInfra(t)
			sbx := &v1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "cold", Namespace: "default", UID: "sandbox-uid"}}
			sbx.Spec.Template = &corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{coldNetworkSchedulingGate: "creation-uid"}}, Spec: corev1.PodSpec{SchedulingGates: []corev1.PodSchedulingGate{{Name: coldNetworkSchedulingGate}}}}
			require.NoError(t, c.Create(t.Context(), sbx))
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: sbx.Name, Namespace: sbx.Namespace}, Spec: corev1.PodSpec{InitContainers: []corev1.Container{{Name: "traffic-proxy", RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways)}}, SchedulingGates: []corev1.PodSchedulingGate{{Name: "example.com/another-gate"}}}}
			if tt.owner {
				pod.OwnerReferences = []metav1.OwnerReference{sandboxOwnerRef(sbx)}
			}
			if tt.gate {
				pod.Spec.SchedulingGates = append(pod.Spec.SchedulingGates, corev1.PodSchedulingGate{Name: coldNetworkSchedulingGate})
			}
			if tt.probe {
				pod.Spec.InitContainers[0].StartupProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz/ready", Port: intstr.FromInt32(15021)}}}
			}
			require.NoError(t, c.Create(t.Context(), pod))
			input := &infra.EgressPolicy{DefaultAction: "allow", Rules: []infra.EgressRule{{Action: "deny", CIDR: "192.0.2.1/32"}, {Action: "allow", CIDR: "192.0.2.0/24"}}}
			err := AsSandbox(sbx, backend.Cache).prepareColdNetwork(t.Context(), input, time.Second)
			var policy v1alpha1.TrafficPolicy
			require.NoError(t, c.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "network-cold"}, &policy))
			require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(pod), pod))
			if !tt.probe || !tt.owner || !tt.gate {
				require.Error(t, err)
				if tt.gate {
					assert.Len(t, pod.Spec.SchedulingGates, 2)
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, []corev1.PodSchedulingGate{{Name: "example.com/another-gate"}}, pod.Spec.SchedulingGates)
			require.Len(t, policy.Spec.Egress.Rules, 4)
			assert.Equal(t, v1alpha1.RuleActionReject, policy.Spec.Egress.Rules[1].Action)
			assert.Equal(t, v1alpha1.RuleActionAllow, policy.Spec.Egress.Rules[2].Action)
			assert.Equal(t, v1alpha1.RuleActionAllow, policy.Spec.Egress.Rules[3].Action)
			assert.Equal(t, sbx.UID, policy.OwnerReferences[0].UID)
			assert.Equal(t, "creation-uid", policy.Spec.Selector.MatchLabels[coldNetworkSchedulingGate])
			require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(sbx), sbx))
			assert.Empty(t, sbx.Spec.Template.Spec.SchedulingGates)
		})
	}
}

func TestColdNetworkTemplateStartsGated(t *testing.T) {
	backend, _ := NewTestInfra(t)
	sbx, _, err := newColdSandbox(t.Context(), infra.ClaimSandboxOptions{Namespace: "default", ColdStart: &infra.ColdStartOptions{Image: "python:3.11", EgressPolicy: &infra.EgressPolicy{DefaultAction: "deny"}}}, backend.Cache)
	require.NoError(t, err)
	assert.Equal(t, []corev1.PodSchedulingGate{{Name: coldNetworkSchedulingGate}}, sbx.Spec.Template.Spec.SchedulingGates)
	assert.Contains(t, sbx.Spec.Runtimes, v1alpha1.RuntimeConfig{Name: v1alpha1.RuntimeConfigForInjectTrafficProxy})
	policy := buildColdTrafficPolicy(sbx.Sandbox, &infra.EgressPolicy{DefaultAction: "deny"})
	require.Len(t, policy.Spec.Egress.Rules, 2)
	assert.Equal(t, v1alpha1.RuleActionReject, policy.Spec.Egress.Rules[1].Action)
}
