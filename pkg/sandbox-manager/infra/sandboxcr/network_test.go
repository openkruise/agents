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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/network"
)

func TestBuildTrafficPolicy(t *testing.T) {
	owner := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", UID: "test-uid"},
	}
	tests := []struct {
		name            string
		allowOutCIDRs   []string
		allowOutDomains []string
		denyOut         []string
		expectNil       bool
		expectRuleCount int
		// ruleChecks: slice of actions to verify rule ordering
		ruleChecks []agentsv1alpha1.RuleAction
		// peerChecks: slice of slice of strings per rule — each string is either a CIDR or FQDN
		peerChecks [][]string
		// fqdnChecks: slice of slice of FQDNs per rule (empty if no FQDN in that rule)
		fqdnChecks [][]string
	}{
		{
			name:            "whitelist CIDR only — allow + default deny",
			allowOutCIDRs:   []string{"1.2.3.4/32"},
			allowOutDomains: nil,
			denyOut:         nil,
			expectNil:       false,
			expectRuleCount: 2,
			ruleChecks:      []agentsv1alpha1.RuleAction{agentsv1alpha1.RuleActionAllow, agentsv1alpha1.RuleActionReject},
			peerChecks:      [][]string{{"1.2.3.4/32"}, {network.AllTrafficCIDR}},
			fqdnChecks:      [][]string{nil, nil},
		},
		{
			name:            "whitelist + denyOut — allow + explicit deny + default deny",
			allowOutCIDRs:   []string{"1.2.3.4/32"},
			allowOutDomains: nil,
			denyOut:         []string{"10.0.0.0/8", "172.16.0.0/12"},
			expectNil:       false,
			expectRuleCount: 3,
			ruleChecks: []agentsv1alpha1.RuleAction{
				agentsv1alpha1.RuleActionAllow,
				agentsv1alpha1.RuleActionReject,
				agentsv1alpha1.RuleActionReject,
			},
			peerChecks: [][]string{
				{"1.2.3.4/32"},
				{"10.0.0.0/8", "172.16.0.0/12"},
				{network.AllTrafficCIDR},
			},
			fqdnChecks: [][]string{nil, nil, nil},
		},
		{
			name:            "whitelist FQDN only — auto-inject DNS CIDR + default deny",
			allowOutCIDRs:   nil,
			allowOutDomains: []string{"api.example.com", "*.github.com"},
			denyOut:         nil,
			expectNil:       false,
			expectRuleCount: 2,
			ruleChecks:      []agentsv1alpha1.RuleAction{agentsv1alpha1.RuleActionAllow, agentsv1alpha1.RuleActionReject},
			peerChecks:      [][]string{{network.DNSServerCIDR}, {network.AllTrafficCIDR}},
			fqdnChecks:      [][]string{{"api.example.com", "*.github.com"}, nil},
		},
		{
			name:            "whitelist FQDN with explicit DNS CIDR — no duplicate",
			allowOutCIDRs:   []string{"8.8.8.8/32"},
			allowOutDomains: []string{"api.example.com"},
			denyOut:         nil,
			expectNil:       false,
			expectRuleCount: 2,
			ruleChecks:      []agentsv1alpha1.RuleAction{agentsv1alpha1.RuleActionAllow, agentsv1alpha1.RuleActionReject},
			peerChecks:      [][]string{{"8.8.8.8/32"}, {network.AllTrafficCIDR}},
			fqdnChecks:      [][]string{{"api.example.com"}, nil},
		},
		{
			name:            "whitelist CIDR + FQDN + denyOut — allow (mixed peers) + explicit deny + default deny",
			allowOutCIDRs:   []string{"1.2.3.4/32"},
			allowOutDomains: []string{"api.example.com"},
			denyOut:         []string{"10.0.0.0/8"},
			expectNil:       false,
			expectRuleCount: 3,
			ruleChecks: []agentsv1alpha1.RuleAction{
				agentsv1alpha1.RuleActionAllow,
				agentsv1alpha1.RuleActionReject,
				agentsv1alpha1.RuleActionReject,
			},
			peerChecks: [][]string{
				{"1.2.3.4/32", network.DNSServerCIDR},
				{"10.0.0.0/8"},
				{network.AllTrafficCIDR},
			},
			fqdnChecks: [][]string{
				{"api.example.com"},
				nil,
				nil,
			},
		},
		{
			name:            "blacklist only — reject denyOut entries",
			allowOutCIDRs:   nil,
			allowOutDomains: nil,
			denyOut:         []string{"10.0.0.0/8"},
			expectNil:       false,
			expectRuleCount: 1,
			ruleChecks:      []agentsv1alpha1.RuleAction{agentsv1alpha1.RuleActionReject},
			peerChecks:      [][]string{{"10.0.0.0/8"}},
			fqdnChecks:      [][]string{nil},
		},
		{
			name:            "empty config returns nil",
			allowOutCIDRs:   nil,
			allowOutDomains: nil,
			denyOut:         nil,
			expectNil:       true,
			expectRuleCount: 0,
		},
		{
			name:            "denyOut with bare IP gets normalized to CIDR",
			allowOutCIDRs:   []string{"8.8.8.8/32"},
			allowOutDomains: nil,
			denyOut:         []string{"8.8.4.4"},
			expectNil:       false,
			expectRuleCount: 3,
			ruleChecks: []agentsv1alpha1.RuleAction{
				agentsv1alpha1.RuleActionAllow,
				agentsv1alpha1.RuleActionReject,
				agentsv1alpha1.RuleActionReject,
			},
			peerChecks: [][]string{
				{"8.8.8.8/32"},
				{"8.8.4.4/32"},
				{network.AllTrafficCIDR},
			},
			fqdnChecks: [][]string{nil, nil, nil},
		},
		{
			name:            "allowOut contains 0.0.0.0/0 — no default deny",
			allowOutCIDRs:   []string{"0.0.0.0/0"},
			allowOutDomains: nil,
			denyOut:         nil,
			expectNil:       false,
			expectRuleCount: 1,
			ruleChecks:      []agentsv1alpha1.RuleAction{agentsv1alpha1.RuleActionAllow},
			peerChecks:      [][]string{{"0.0.0.0/0"}},
			fqdnChecks:      [][]string{nil},
		},
		{
			name:            "allowOut contains 0.0.0.0/0 + denyOut — no default deny",
			allowOutCIDRs:   []string{"0.0.0.0/0"},
			allowOutDomains: nil,
			denyOut:         []string{"10.0.0.0/8"},
			expectNil:       false,
			expectRuleCount: 2,
			ruleChecks: []agentsv1alpha1.RuleAction{
				agentsv1alpha1.RuleActionAllow,
				agentsv1alpha1.RuleActionReject,
			},
			peerChecks: [][]string{
				{"0.0.0.0/0"},
				{"10.0.0.0/8"},
			},
			fqdnChecks: [][]string{nil, nil},
		},
		{
			name:            "allowOut contains ::/0 (IPv6 all-traffic) — no default deny",
			allowOutCIDRs:   []string{"::/0"},
			allowOutDomains: nil,
			denyOut:         nil,
			expectNil:       false,
			expectRuleCount: 1,
			ruleChecks:      []agentsv1alpha1.RuleAction{agentsv1alpha1.RuleActionAllow},
			peerChecks:      [][]string{{"::/0"}},
			fqdnChecks:      [][]string{nil},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tp := buildTrafficPolicy(tt.allowOutCIDRs, tt.allowOutDomains, tt.denyOut, "default", "test-sandbox-id", owner)
			if tt.expectNil {
				assert.Nil(t, tp)
				return
			}
			require.NotNil(t, tp)
			require.NotNil(t, tp.Spec.Egress)
			rules := tp.Spec.Egress.Rules
			assert.Len(t, rules, tt.expectRuleCount)

			for i, expectedAction := range tt.ruleChecks {
				require.Less(t, i, len(rules), "fewer rules than expected")
				assert.Equal(t, expectedAction, rules[i].Action, "rule %d action mismatch", i)
				if i < len(tt.peerChecks) {
					var gotCIDRs []string
					for _, peer := range rules[i].To {
						if peer.CIDR != "" {
							gotCIDRs = append(gotCIDRs, peer.CIDR)
						}
					}
					assert.Equal(t, tt.peerChecks[i], gotCIDRs, "rule %d peer CIDRs mismatch", i)
				}
				if i < len(tt.fqdnChecks) {
					var gotFQDNs []string
					for _, peer := range rules[i].To {
						if peer.FQDN != "" {
							gotFQDNs = append(gotFQDNs, peer.FQDN)
						}
					}
					assert.Equal(t, tt.fqdnChecks[i], gotFQDNs, "rule %d peer FQDNs mismatch", i)
				}
			}

			// Verify metadata
			assert.Equal(t, "tp-", tp.GenerateName)
			assert.Equal(t, "default", tp.Namespace)
			assert.Equal(t, "test-uid", tp.Spec.Selector.MatchLabels[agentsv1alpha1.LabelSandboxUID])
			assert.Equal(t, int32(1000), tp.Spec.Priority)
			// Verify OwnerReference is set
			require.Len(t, tp.OwnerReferences, 1)
			assert.Equal(t, "Sandbox", tp.OwnerReferences[0].Kind)
			assert.Equal(t, "test-sandbox", tp.OwnerReferences[0].Name)
			assert.Equal(t, "test-uid", string(tp.OwnerReferences[0].UID))
		})
	}
}

// TestCreateSelectNetworkPolicy_RoundTrip verifies that network config
// written via CreateNetworkPolicy can be fully read back via
// SelectNetworkPolicy, including denyOut entries in whitelist mode
// and FQDN domain entries.
func TestCreateSelectNetworkPolicy_RoundTrip(t *testing.T) {
	tests := []struct {
		name           string
		network        infra.SandboxNetworkConfig
		expectAllowOut []string
		expectDenyOut  []string
	}{
		{
			name: "whitelist + denyOut round-trip preserves both",
			network: infra.SandboxNetworkConfig{
				AllowOut: []string{"1.2.3.4", "api.example.com"},
				DenyOut:  []string{"10.0.0.0/8", "172.16.0.0/12"},
			},
			expectAllowOut: []string{"1.2.3.4/32", "api.example.com"},
			expectDenyOut:  []string{"10.0.0.0/8", "172.16.0.0/12"},
		},
		{
			name: "whitelist only round-trip",
			network: infra.SandboxNetworkConfig{
				AllowOut: []string{"1.2.3.4"},
			},
			expectAllowOut: []string{"1.2.3.4/32"},
			expectDenyOut:  nil,
		},
		{
			name: "blacklist only round-trip",
			network: infra.SandboxNetworkConfig{
				DenyOut: []string{"8.8.8.8/32"},
			},
			expectAllowOut: nil,
			expectDenyOut:  []string{"8.8.8.8/32"},
		},
		{
			name: "whitelist + bare IP denyOut gets normalized",
			network: infra.SandboxNetworkConfig{
				AllowOut: []string{"1.1.1.1"},
				DenyOut:  []string{"8.8.4.4"},
			},
			expectAllowOut: []string{"1.1.1.1/32"},
			expectDenyOut:  []string{"8.8.4.4/32"},
		},
		{
			name: "FQDN only round-trip preserves domains",
			network: infra.SandboxNetworkConfig{
				AllowOut: []string{"api.example.com", "*.github.com"},
			},
			expectAllowOut: []string{"api.example.com", "*.github.com"},
			expectDenyOut:  nil,
		},
		{
			name: "mixed CIDR + FQDN + denyOut round-trip",
			network: infra.SandboxNetworkConfig{
				AllowOut: []string{"1.2.3.4", "api.example.com"},
				DenyOut:  []string{"10.0.0.0/8"},
			},
			expectAllowOut: []string{"1.2.3.4/32", "api.example.com"},
			expectDenyOut:  []string{"10.0.0.0/8"},
		},
		{
			name: "allowOut 0.0.0.0/0 round-trip preserves allow-all",
			network: infra.SandboxNetworkConfig{
				AllowOut: []string{"0.0.0.0/0"},
			},
			expectAllowOut: []string{"0.0.0.0/0"},
			expectDenyOut:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			infraInstance, fc := NewTestInfra(t)

			sbx := createTestSandbox("network-rt-sandbox", "test-user", agentsv1alpha1.SandboxRunning, true)
			CreateSandboxWithStatus(t, fc, sbx)

			// Wait for cache to sync
			var sandbox infra.Sandbox
			require.Eventually(t, func() bool {
				var err error
				sandbox, err = infraInstance.GetSandbox(t.Context(), infra.GetSandboxOptions{
					SandboxID: utils.GetSandboxID(sbx),
					Namespace: sbx.Namespace,
				})
				return err == nil
			}, time.Second, 10*time.Millisecond)

			// Create network CRs
			require.NoError(t, sandbox.CreateNetworkPolicy(t.Context(), tt.network))

			// Read back
			result, err := sandbox.SelectNetworkPolicy(t.Context())
			require.NoError(t, err)
			require.NotNil(t, result, "SelectNetworkPolicy should return non-nil config")

			assert.ElementsMatch(t, tt.expectAllowOut, result.AllowOut)
			assert.ElementsMatch(t, tt.expectDenyOut, result.DenyOut)
		})
	}
}

// TestUpdateSelectNetworkPolicy_RoundTrip verifies that UpdateNetworkPolicy
// (replace semantics) also preserves denyOut in whitelist mode and FQDN entries.
func TestUpdateSelectNetworkPolicy_RoundTrip(t *testing.T) {
	infraInstance, fc := NewTestInfra(t)

	sbx := createTestSandbox("network-update-sandbox", "test-user", agentsv1alpha1.SandboxRunning, true)
	CreateSandboxWithStatus(t, fc, sbx)

	var sandbox infra.Sandbox
	require.Eventually(t, func() bool {
		var err error
		sandbox, err = infraInstance.GetSandbox(t.Context(), infra.GetSandboxOptions{
			SandboxID: utils.GetSandboxID(sbx),
			Namespace: sbx.Namespace,
		})
		return err == nil
	}, time.Second, 10*time.Millisecond)

	// Step 1: Create with allowOut only
	require.NoError(t, sandbox.CreateNetworkPolicy(t.Context(), infra.SandboxNetworkConfig{
		AllowOut: []string{"1.2.3.4"},
	}))

	result, err := sandbox.SelectNetworkPolicy(t.Context())
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, []string{"1.2.3.4/32"}, result.AllowOut)
	assert.Empty(t, result.DenyOut)

	// Step 2: Update to allowOut + denyOut (whitelist mode with deny)
	require.NoError(t, sandbox.UpdateNetworkPolicy(t.Context(), infra.SandboxNetworkConfig{
		AllowOut: []string{"1.2.3.4"},
		DenyOut:  []string{"10.0.0.0/8"},
	}))

	result, err = sandbox.SelectNetworkPolicy(t.Context())
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, []string{"1.2.3.4/32"}, result.AllowOut)
	assert.Equal(t, []string{"10.0.0.0/8"}, result.DenyOut)

	// Step 3: Update to add FQDN entries
	require.NoError(t, sandbox.UpdateNetworkPolicy(t.Context(), infra.SandboxNetworkConfig{
		AllowOut: []string{"1.2.3.4", "api.example.com"},
		DenyOut:  []string{"10.0.0.0/8"},
	}))

	result, err = sandbox.SelectNetworkPolicy(t.Context())
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.ElementsMatch(t, []string{"1.2.3.4/32", "api.example.com"}, result.AllowOut)
	assert.Equal(t, []string{"10.0.0.0/8"}, result.DenyOut)

	// Step 4: Update to clear all (empty config)
	require.NoError(t, sandbox.UpdateNetworkPolicy(t.Context(), infra.SandboxNetworkConfig{}))

	result, err = sandbox.SelectNetworkPolicy(t.Context())
	require.NoError(t, err)
	assert.Nil(t, result, "after clearing all rules, SelectNetworkPolicy should return nil")
}
