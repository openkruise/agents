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
	"errors"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/traffic-extension/model"
)

// auditProfile builds a model.SecurityProfile with the given identity. The
// SecurityRules slice is left empty because the collector only needs the
// profile metadata when assembling action IDs.
func auditProfile(ns, name string) *model.SecurityProfile {
	return &model.SecurityProfile{
		Profile: &v1alpha1.SecurityProfile{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		},
	}
}

func auditRule(name string) *model.SecurityRule {
	return &model.SecurityRule{Name: name}
}

func TestAuditCollector_OutcomePrecedence(t *testing.T) {
	tests := []struct {
		name        string
		apply       func(*auditCollector)
		wantOutcome string
		wantActions []string
		wantSkipped map[string]int
		wantError   string
	}{
		{
			name:        "no-op produces passthrough",
			apply:       func(*auditCollector) {},
			wantOutcome: outcomePassthrough,
			wantSkipped: map[string]int{},
		},
		{
			name: "single mutate marks outcome mutated",
			apply: func(c *auditCollector) {
				c.noteMutate("mutator", auditProfile("ns", "p"), auditRule("r"))
			},
			wantOutcome: outcomeMutated,
			wantActions: []string{"mutator:ns/p/r"},
			wantSkipped: map[string]int{},
		},
		{
			name: "bypass immediate marks bypassed",
			apply: func(c *auditCollector) {
				c.noteImmediate("bypass", auditProfile("ns", "p"), auditRule("r"))
			},
			wantOutcome: outcomeBypassed,
			wantActions: []string{"bypass:ns/p/r"},
			wantSkipped: map[string]int{},
		},
		{
			name: "block immediate after mutate marks blocked but keeps both actions",
			apply: func(c *auditCollector) {
				c.noteMutate("mutator", auditProfile("ns", "p"), auditRule("r1"))
				c.noteImmediate("block", auditProfile("ns", "p"), auditRule("r2"))
			},
			wantOutcome: outcomeBlocked,
			wantActions: []string{"mutator:ns/p/r1", "block:ns/p/r2"},
			wantSkipped: map[string]int{},
		},
		{
			name: "bypass beats block within same request",
			apply: func(c *auditCollector) {
				c.noteImmediate("block", auditProfile("ns", "p"), auditRule("r1"))
				c.noteImmediate("bypass", auditProfile("ns", "p"), auditRule("r2"))
			},
			wantOutcome: outcomeBypassed,
			wantActions: []string{"block:ns/p/r1", "bypass:ns/p/r2"},
			wantSkipped: map[string]int{},
		},
		{
			name: "noteError beats everything else",
			apply: func(c *auditCollector) {
				c.noteImmediate("bypass", auditProfile("ns", "p"), auditRule("r"))
				c.noteError(errors.New("boom"))
			},
			wantOutcome: outcomeError,
			wantActions: []string{"bypass:ns/p/r"},
			wantSkipped: map[string]int{},
			wantError:   "boom",
		},
		{
			name: "record then preempt promotes to skipped",
			apply: func(c *auditCollector) {
				c.noteRecord("mutator", auditProfile("ns", "p"), auditRule("r"))
				c.preemptRecorded()
			},
			wantOutcome: outcomePassthrough,
			wantSkipped: map[string]int{"mutator": 1},
		},
		{
			name: "commitRecordedAsMutate promotes pending plugin into actions",
			apply: func(c *auditCollector) {
				c.noteRecord("mutator", auditProfile("ns", "p"), auditRule("r"))
				c.commitRecordedAsMutate("mutator")
			},
			wantOutcome: outcomeMutated,
			wantActions: []string{"mutator:ns/p/r"},
			wantSkipped: map[string]int{},
		},
		{
			name: "commitRecordedAsImmediate from bypass marks bypassed",
			apply: func(c *auditCollector) {
				c.noteRecord("bypass", auditProfile("ns", "p"), auditRule("r"))
				c.commitRecordedAsImmediate("bypass")
			},
			wantOutcome: outcomeBypassed,
			wantActions: []string{"bypass:ns/p/r"},
			wantSkipped: map[string]int{},
		},
		{
			name: "finalizeContinued captures Allow-path error",
			apply: func(c *auditCollector) {
				c.noteRecord("mutator", auditProfile("ns", "p"), auditRule("r"))
				c.finalizeContinued("mutator", errors.New("provider 502"))
			},
			wantOutcome: outcomePassthrough,
			wantSkipped: map[string]int{"mutator": 1},
			wantError:   "provider 502",
		},
		{
			name: "commit on unknown plugin is a noop",
			apply: func(c *auditCollector) {
				c.commitRecordedAsMutate("ghost")
				c.commitRecordedAsImmediate("ghost")
			},
			wantOutcome: outcomePassthrough,
			wantSkipped: map[string]int{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newAuditCollector()
			tc.apply(c)
			entry := c.buildEntry(types.NamespacedName{Namespace: "default", Name: "pod-x"},
				model.RequestInfo{Method: "GET", Host: "h", Path: "/p"}, 1)
			if entry.Outcome != tc.wantOutcome {
				t.Errorf("outcome: want %q, got %q", tc.wantOutcome, entry.Outcome)
			}
			if !reflect.DeepEqual(entry.Actions, tc.wantActions) && !(len(entry.Actions) == 0 && len(tc.wantActions) == 0) {
				t.Errorf("actions: want %v, got %v", tc.wantActions, entry.Actions)
			}
			if !reflect.DeepEqual(entry.Skipped, tc.wantSkipped) {
				t.Errorf("skipped: want %v, got %v", tc.wantSkipped, entry.Skipped)
			}
			if entry.Error != tc.wantError {
				t.Errorf("error: want %q, got %q", tc.wantError, entry.Error)
			}
		})
	}
}

// TestActionID_HandlesMissingProfile verifies actionID stays well-formed
// when invoked without a profile (which the orchestrator never does, but
// the helper is defensive).
func TestActionID_HandlesMissingProfile(t *testing.T) {
	if got := actionID("p", nil, auditRule("r")); got != "p://r" {
		t.Errorf("nil profile: want %q, got %q", "p://r", got)
	}
	if got := actionID("p", &model.SecurityProfile{}, auditRule("r")); got != "p://r" {
		t.Errorf("empty profile: want %q, got %q", "p://r", got)
	}
	if got := actionID("p", auditProfile("ns", "name"), nil); got != "p:ns/name/" {
		t.Errorf("nil rule: want %q, got %q", "p:ns/name/", got)
	}
}

func TestImmediateOutcome(t *testing.T) {
	if got := immediateOutcome("bypass"); got != outcomeBypassed {
		t.Errorf("bypass should map to bypassed, got %q", got)
	}
	if got := immediateOutcome("block"); got != outcomeBlocked {
		t.Errorf("block should map to blocked, got %q", got)
	}
	if got := immediateOutcome("future-plugin"); got != outcomeBlocked {
		t.Errorf("unknown plugin should fall back to blocked, got %q", got)
	}
}
