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

package handlers

import (
	"github.com/openkruise/agents/pkg/traffic-extension/model"
	"github.com/openkruise/agents/pkg/traffic-extension/plugins/bypass"
	"github.com/openkruise/agents/pkg/traffic-extension/util/auditlog"
	"k8s.io/apimachinery/pkg/types"
)

const (
	outcomePassthrough = "passthrough"
	outcomeMutated     = "mutated"
	outcomeBlocked     = "blocked"
	outcomeBypassed    = "bypassed"
	outcomeError       = "error"
)

// auditCollector accumulates the per-request audit signals produced during
// plugin dispatch and translates them into a single auditlog.Entry on
// HandleRequestHeaders return.
//
// The collector is intentionally request-scoped (not goroutine-safe): the
// handler always touches it from one goroutine, and the audit logger does
// its own buffering.
type auditCollector struct {
	actions  []string
	skipped  map[string]int
	recorded map[string]recordedRule
	outcome  string
	err      string
}

// recordedRule keeps just enough context to build an action ID once a
// recorded plugin's Finalize commits a real mutation or short-circuit.
type recordedRule struct {
	profile *model.SecurityProfile
	rule    *model.SecurityRule
}

func newAuditCollector() *auditCollector {
	return &auditCollector{
		skipped:  map[string]int{},
		recorded: map[string]recordedRule{},
	}
}

// noteMutate records a plugin that produced a header mutation during scan.
func (c *auditCollector) noteMutate(plugin string, profile *model.SecurityProfile, rule *model.SecurityRule) {
	c.actions = append(c.actions, actionID(plugin, profile, rule))
	c.setOutcome(outcomeMutated)
}

// noteImmediate records a plugin that short-circuited the chain. Outcome
// is derived from the plugin name: bypass produces "bypassed", anything
// else (today only block) is treated as "blocked".
func (c *auditCollector) noteImmediate(plugin string, profile *model.SecurityProfile, rule *model.SecurityRule) {
	c.actions = append(c.actions, actionID(plugin, profile, rule))
	c.setOutcome(immediateOutcome(plugin))
}

// noteRecord stashes the (profile, rule) pair for a plugin that claimed a
// rule via ActionRecord. The orchestrator later calls commit*/finalizeContinued
// when the scan and Finalize pass complete.
func (c *auditCollector) noteRecord(plugin string, profile *model.SecurityProfile, rule *model.SecurityRule) {
	c.recorded[plugin] = recordedRule{profile: profile, rule: rule}
}

// commitRecordedAsMutate promotes a previously recorded plugin to the
// actions list with outcome=mutated. Called when Finalize returned
// ActionMutate.
func (c *auditCollector) commitRecordedAsMutate(plugin string) {
	rec, ok := c.recorded[plugin]
	if !ok {
		return
	}
	delete(c.recorded, plugin)
	c.actions = append(c.actions, actionID(plugin, rec.profile, rec.rule))
	c.setOutcome(outcomeMutated)
}

// commitRecordedAsImmediate handles the unusual case of a Finalize that
// itself returns ActionImmediate. Outcome is derived from the plugin name
// the same way noteImmediate does.
func (c *auditCollector) commitRecordedAsImmediate(plugin string) {
	rec, ok := c.recorded[plugin]
	if !ok {
		return
	}
	delete(c.recorded, plugin)
	c.actions = append(c.actions, actionID(plugin, rec.profile, rec.rule))
	c.setOutcome(immediateOutcome(plugin))
}

// finalizeContinued marks a recorded plugin as skipped (Finalize returned
// ActionContinue, e.g. a deferred upstream call swallowed an error under
// a permissive failure mode). The optional err lets the plugin surface
// why it gave up.
func (c *auditCollector) finalizeContinued(plugin string, err error) {
	delete(c.recorded, plugin)
	c.skipped[plugin]++
	if err != nil {
		c.err = err.Error()
	}
}

// preemptRecorded accounts for any plugins still in the recorded state when
// the request resolved via a terminal action or an error. Each such plugin
// is counted under skipped because it claimed a rule but never produced a
// mutation.
func (c *auditCollector) preemptRecorded() {
	for plugin := range c.recorded {
		c.skipped[plugin]++
	}
	c.recorded = map[string]recordedRule{}
}

// noteError marks the request as failed and records the gRPC error
// message. The latest non-nil error wins so the audit reflects the actual
// failure even if an earlier Allow path swallowed one.
func (c *auditCollector) noteError(err error) {
	c.setOutcome(outcomeError)
	if err != nil {
		c.err = err.Error()
	}
}

// buildEntry assembles the audit payload for submission. profileCount is
// passed in from the orchestrator because the collector doesn't track it
// directly.
func (c *auditCollector) buildEntry(podNN types.NamespacedName, info model.RequestInfo, profileCount int) auditlog.Entry {
	return auditlog.Entry{
		Pod:      podNN,
		Method:   info.Method,
		Host:     info.Host,
		Path:     info.Path,
		Profiles: profileCount,
		Outcome:  c.derivedOutcome(),
		Actions:  c.actions,
		Skipped:  c.skipped,
		Error:    c.err,
	}
}

// derivedOutcome returns the recorded outcome string, defaulting to
// passthrough when no plugin acted.
func (c *auditCollector) derivedOutcome() string {
	if c.outcome == "" {
		return outcomePassthrough
	}
	return c.outcome
}

// setOutcome promotes c.outcome to o only if o has equal or higher
// priority. Precedence (high → low):
// error > bypassed > blocked > mutated > passthrough.
func (c *auditCollector) setOutcome(o string) {
	if outcomeRank(o) >= outcomeRank(c.outcome) {
		c.outcome = o
	}
}

func outcomeRank(o string) int {
	switch o {
	case outcomeError:
		return 5
	case outcomeBypassed:
		return 4
	case outcomeBlocked:
		return 3
	case outcomeMutated:
		return 2
	case outcomePassthrough:
		return 1
	default:
		return 0
	}
}

// immediateOutcome maps a plugin name to the outcome it produces when it
// returns ActionImmediate. bypass is the only plugin that produces a
// non-terminal-from-client's-perspective passthrough; everything else is
// reported as a block from an audit viewpoint.
func immediateOutcome(plugin string) string {
	if plugin == bypass.PluginName {
		return outcomeBypassed
	}
	return outcomeBlocked
}

// actionID returns the canonical "<plugin>:<ns>/<profile>/<rule>" identifier
// used in the audit entry's Actions slice. Missing profile metadata is
// rendered as an empty segment so the prefix stays parseable.
func actionID(plugin string, profile *model.SecurityProfile, rule *model.SecurityRule) string {
	var ns, name, ruleName string
	if profile != nil && profile.Profile != nil {
		ns = profile.Profile.Namespace
		name = profile.Profile.Name
	}
	if rule != nil {
		ruleName = rule.Name
	}
	return plugin + ":" + ns + "/" + name + "/" + ruleName
}
