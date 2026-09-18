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

package autopause

import (
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// PolicyProbeNames returns the deduplicated names of the probes the policy
// rules reference, in declaration order. A nil policy references none.
func PolicyProbeNames(policy *agentsv1alpha1.AutoPausePolicy) []string {
	if policy == nil {
		return nil
	}
	var names []string
	seen := map[string]struct{}{}
	add := func(name string) {
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	if policy.Pause != nil && policy.Pause.WhenProbedIdleState != nil {
		add(policy.Pause.WhenProbedIdleState.Probe)
	}
	if policy.Resume != nil && policy.Resume.WhenProbedScheduleTime != nil {
		add(policy.Resume.WhenProbedScheduleTime.Probe)
	}
	return names
}

// RequiredProbeNames returns the probe names a pool candidate must already
// declare: the names referenced by the policy minus the ones carried by the
// claim itself, because claim probes are merged onto the picked sandbox at
// claim time. A nil policy references none.
func RequiredProbeNames(policy *agentsv1alpha1.AutoPausePolicy, claimProbes []agentsv1alpha1.Probe) []string {
	names := PolicyProbeNames(policy)
	if len(names) == 0 || len(claimProbes) == 0 {
		return names
	}
	carried := make(map[string]struct{}, len(claimProbes))
	for i := range claimProbes {
		carried[claimProbes[i].Name] = struct{}{}
	}
	var required []string
	for _, name := range names {
		if _, ok := carried[name]; !ok {
			required = append(required, name)
		}
	}
	return required
}

// MaxSandboxProbes mirrors the +kubebuilder:validation:MaxItems=16 limit on
// Sandbox.spec.probes. The merged claim + pool set must stay within it, or
// the apiserver would reject the sandbox update with a less clear error.
const MaxSandboxProbes = 16

// MergeProbes merges claim probes into pool probes by name: a claim probe
// replaces the pool probe with the same name, new names are appended in
// claim order. Every returned probe is a deep copy, so the result never
// aliases the SandboxClaim spec or the informer-cached pool spec.
func MergeProbes(poolProbes, claimProbes []agentsv1alpha1.Probe) []agentsv1alpha1.Probe {
	var merged []agentsv1alpha1.Probe
	claimByName := make(map[string]int, len(claimProbes))
	for i := range claimProbes {
		claimByName[claimProbes[i].Name] = i
	}
	for i := range poolProbes {
		if j, ok := claimByName[poolProbes[i].Name]; ok {
			merged = append(merged, *claimProbes[j].DeepCopy())
		} else {
			merged = append(merged, *poolProbes[i].DeepCopy())
		}
	}
	poolNames := make(map[string]struct{}, len(poolProbes))
	for i := range poolProbes {
		poolNames[poolProbes[i].Name] = struct{}{}
	}
	for i := range claimProbes {
		if _, ok := poolNames[claimProbes[i].Name]; !ok {
			merged = append(merged, *claimProbes[i].DeepCopy())
		}
	}
	return merged
}
