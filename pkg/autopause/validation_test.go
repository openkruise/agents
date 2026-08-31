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
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func execProbe(name string, command ...string) agentsv1alpha1.Probe {
	return agentsv1alpha1.Probe{
		Name: name,
		Probe: corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{Command: command},
			},
		},
	}
}

// fieldPaths returns the field path of every error in order, so a table case can
// assert which field a rejection is reported on. The paths are part of the
// contract: corev1.Probe is embedded inline, so a user writing periodSeconds has
// no "probe" level in their YAML to be pointed at.
func fieldPaths(errs field.ErrorList) []string {
	paths := make([]string, 0, len(errs))
	for _, err := range errs {
		paths = append(paths, err.Field)
	}
	return paths
}

func TestValidateProbes(t *testing.T) {
	tests := []struct {
		name        string
		probes      []agentsv1alpha1.Probe
		wantErrs    int
		wantFields  []string
		wantMessage string
	}{
		{
			name:   "no probes",
			probes: nil,
		},
		{
			name:   "valid exec probes",
			probes: []agentsv1alpha1.Probe{execProbe("idle", "cat", "/tmp/idle"), execProbe("schedule", "cat", "/tmp/schedule")},
		},
		{
			name: "valid probe with timing fields",
			probes: []agentsv1alpha1.Probe{func() agentsv1alpha1.Probe {
				p := execProbe("idle", "cat", "/tmp/idle")
				p.PeriodSeconds = 10
				p.TimeoutSeconds = 3
				p.FailureThreshold = 2
				return p
			}()},
		},
		{
			name:        "empty name",
			probes:      []agentsv1alpha1.Probe{execProbe("", "cat", "/tmp/idle")},
			wantErrs:    1,
			wantFields:  []string{"spec.probes[0].name"},
			wantMessage: "probe name is required",
		},
		{
			name:        "name is not a qualified name",
			probes:      []agentsv1alpha1.Probe{execProbe("idle probe!", "cat", "/tmp/idle")},
			wantErrs:    1,
			wantFields:  []string{"spec.probes[0].name"},
			wantMessage: "the name is reported as condition type",
		},
		{
			name:        "duplicate names",
			probes:      []agentsv1alpha1.Probe{execProbe("idle", "cat", "/tmp/a"), execProbe("idle", "cat", "/tmp/b")},
			wantErrs:    1,
			wantFields:  []string{"spec.probes[1].name"},
			wantMessage: `Duplicate value: "idle"`,
		},
		{
			name:        "no handler",
			probes:      []agentsv1alpha1.Probe{{Name: "idle"}},
			wantErrs:    1,
			wantFields:  []string{"spec.probes[0]"},
			wantMessage: "must specify exactly one probe handler",
		},
		{
			name: "multiple handlers",
			probes: []agentsv1alpha1.Probe{{
				Name: "idle",
				Probe: corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						Exec:    &corev1.ExecAction{Command: []string{"cat", "/tmp/idle"}},
						HTTPGet: &corev1.HTTPGetAction{Path: "/healthz"},
					},
				},
			}},
			wantErrs:    1,
			wantFields:  []string{"spec.probes[0]"},
			wantMessage: "only one probe handler can be specified",
		},
		{
			name: "non-exec handler",
			probes: []agentsv1alpha1.Probe{{
				Name: "idle",
				Probe: corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						HTTPGet: &corev1.HTTPGetAction{Path: "/healthz"},
					},
				},
			}},
			wantErrs:    1,
			wantFields:  []string{"spec.probes[0]"},
			wantMessage: `Unsupported value: "httpGet": supported values: "exec"`,
		},
		{
			name:        "empty exec command",
			probes:      []agentsv1alpha1.Probe{execProbe("idle")},
			wantErrs:    1,
			wantFields:  []string{"spec.probes[0].exec.command"},
			wantMessage: "exec command is required",
		},
		{
			name: "negative timing fields",
			probes: []agentsv1alpha1.Probe{func() agentsv1alpha1.Probe {
				p := execProbe("idle", "cat", "/tmp/idle")
				p.PeriodSeconds = -1
				p.TimeoutSeconds = -1
				p.FailureThreshold = -1
				return p
			}()},
			wantErrs: 3,
			// Inline embedding: no "probe" level between the index and the field.
			wantFields:  []string{"spec.probes[0].periodSeconds", "spec.probes[0].timeoutSeconds", "spec.probes[0].failureThreshold"},
			wantMessage: "must be greater than or equal to 0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := ValidateProbes(tt.probes, field.NewPath("spec", "probes"))
			if len(errs) != tt.wantErrs {
				t.Fatalf("expected %d errors, got %d: %v", tt.wantErrs, len(errs), errs)
			}
			if tt.wantFields != nil && !reflect.DeepEqual(fieldPaths(errs), tt.wantFields) {
				t.Errorf("expected errors on fields %v, got %v", tt.wantFields, fieldPaths(errs))
			}
			if tt.wantMessage != "" && !strings.Contains(errs.ToAggregate().Error(), tt.wantMessage) {
				t.Errorf("expected error to contain %q, got %v", tt.wantMessage, errs.ToAggregate())
			}
		})
	}
}

func TestValidateAutoPausePolicy(t *testing.T) {
	probes := []agentsv1alpha1.Probe{execProbe("idle", "cat", "/tmp/idle"), execProbe("schedule", "cat", "/tmp/schedule")}
	minute := &metav1.Duration{Duration: time.Minute}

	tests := []struct {
		name        string
		policy      *agentsv1alpha1.AutoPausePolicy
		probes      []agentsv1alpha1.Probe
		wantErrs    int
		wantFields  []string
		wantMessage string
	}{
		{
			name:   "nil policy",
			policy: nil,
			probes: probes,
		},
		{
			name:        "empty policy carries no rule",
			policy:      &agentsv1alpha1.AutoPausePolicy{},
			probes:      probes,
			wantErrs:    1,
			wantFields:  []string{"spec.autoPausePolicy"},
			wantMessage: "at least one of pause.whenProbedIdleState or resume.whenProbedScheduleTime is required",
		},
		{
			name: "policy with empty pause and resume sections carries no rule",
			policy: &agentsv1alpha1.AutoPausePolicy{
				Pause:  &agentsv1alpha1.PausePolicy{},
				Resume: &agentsv1alpha1.ResumePolicy{},
			},
			probes:      probes,
			wantErrs:    1,
			wantFields:  []string{"spec.autoPausePolicy"},
			wantMessage: "at least one of pause.whenProbedIdleState or resume.whenProbedScheduleTime is required",
		},
		{
			name: "valid pause and resume rules",
			policy: &agentsv1alpha1.AutoPausePolicy{
				Pause: &agentsv1alpha1.PausePolicy{
					WhenProbedIdleState: &agentsv1alpha1.ProbedIdleStateRule{
						Probe:             "idle",
						MessageRegex:      "^idle$",
						ThresholdDuration: minute,
					},
				},
				Resume: &agentsv1alpha1.ResumePolicy{
					WhenProbedScheduleTime: &agentsv1alpha1.ProbedScheduleTimeRule{
						Probe:      "schedule",
						TimeFormat: agentsv1alpha1.ProbeTimeFormatDatetime,
						LeadTime:   minute,
					},
				},
			},
			probes: probes,
		},
		{
			name: "resume rule with defaulted time format and no lead time",
			policy: &agentsv1alpha1.AutoPausePolicy{
				Resume: &agentsv1alpha1.ResumePolicy{
					WhenProbedScheduleTime: &agentsv1alpha1.ProbedScheduleTimeRule{Probe: "schedule"},
				},
			},
			probes: probes,
		},
		{
			name: "pause rule references undefined probe",
			policy: &agentsv1alpha1.AutoPausePolicy{
				Pause: &agentsv1alpha1.PausePolicy{
					WhenProbedIdleState: &agentsv1alpha1.ProbedIdleStateRule{
						Probe:             "missing",
						MessageRegex:      "^idle$",
						ThresholdDuration: minute,
					},
				},
			},
			probes:      probes,
			wantErrs:    1,
			wantFields:  []string{"spec.autoPausePolicy.pause.whenProbedIdleState.probe"},
			wantMessage: "must reference a probe name defined in spec.probes",
		},
		{
			name: "pause rule without probe",
			policy: &agentsv1alpha1.AutoPausePolicy{
				Pause: &agentsv1alpha1.PausePolicy{
					WhenProbedIdleState: &agentsv1alpha1.ProbedIdleStateRule{
						MessageRegex:      "^idle$",
						ThresholdDuration: minute,
					},
				},
			},
			probes:      probes,
			wantErrs:    1,
			wantFields:  []string{"spec.autoPausePolicy.pause.whenProbedIdleState.probe"},
			wantMessage: "probe is required",
		},
		{
			name: "pause rule without messageRegex and thresholdDuration",
			policy: &agentsv1alpha1.AutoPausePolicy{
				Pause: &agentsv1alpha1.PausePolicy{
					WhenProbedIdleState: &agentsv1alpha1.ProbedIdleStateRule{Probe: "idle"},
				},
			},
			probes:      probes,
			wantErrs:    2,
			wantFields:  []string{"spec.autoPausePolicy.pause.whenProbedIdleState.messageRegex", "spec.autoPausePolicy.pause.whenProbedIdleState.thresholdDuration"},
			wantMessage: "thresholdDuration is required",
		},
		{
			name: "pause rule with uncompilable messageRegex",
			policy: &agentsv1alpha1.AutoPausePolicy{
				Pause: &agentsv1alpha1.PausePolicy{
					WhenProbedIdleState: &agentsv1alpha1.ProbedIdleStateRule{
						Probe:             "idle",
						MessageRegex:      "([a-z",
						ThresholdDuration: minute,
					},
				},
			},
			probes:      probes,
			wantErrs:    1,
			wantFields:  []string{"spec.autoPausePolicy.pause.whenProbedIdleState.messageRegex"},
			wantMessage: "error parsing regexp",
		},
		{
			name: "pause rule with zero thresholdDuration pauses immediately",
			policy: &agentsv1alpha1.AutoPausePolicy{
				Pause: &agentsv1alpha1.PausePolicy{
					WhenProbedIdleState: &agentsv1alpha1.ProbedIdleStateRule{
						Probe:             "idle",
						MessageRegex:      "^idle$",
						ThresholdDuration: &metav1.Duration{},
					},
				},
			},
			probes: probes,
		},
		{
			name: "pause rule with negative thresholdDuration",
			policy: &agentsv1alpha1.AutoPausePolicy{
				Pause: &agentsv1alpha1.PausePolicy{
					WhenProbedIdleState: &agentsv1alpha1.ProbedIdleStateRule{
						Probe:             "idle",
						MessageRegex:      "^idle$",
						ThresholdDuration: &metav1.Duration{Duration: -time.Minute},
					},
				},
			},
			probes:      probes,
			wantErrs:    1,
			wantFields:  []string{"spec.autoPausePolicy.pause.whenProbedIdleState.thresholdDuration"},
			wantMessage: "must be greater than or equal to 0",
		},
		{
			name: "resume rule with unsupported time format",
			policy: &agentsv1alpha1.AutoPausePolicy{
				Resume: &agentsv1alpha1.ResumePolicy{
					WhenProbedScheduleTime: &agentsv1alpha1.ProbedScheduleTimeRule{
						Probe:      "schedule",
						TimeFormat: "rfc822",
					},
				},
			},
			probes:      probes,
			wantErrs:    1,
			wantFields:  []string{"spec.autoPausePolicy.resume.whenProbedScheduleTime.timeFormat"},
			wantMessage: `supported values: "unix", "datetime"`,
		},
		{
			name: "resume rule with negative lead time",
			policy: &agentsv1alpha1.AutoPausePolicy{
				Resume: &agentsv1alpha1.ResumePolicy{
					WhenProbedScheduleTime: &agentsv1alpha1.ProbedScheduleTimeRule{
						Probe:    "schedule",
						LeadTime: &metav1.Duration{Duration: -time.Minute},
					},
				},
			},
			probes:      probes,
			wantErrs:    1,
			wantFields:  []string{"spec.autoPausePolicy.resume.whenProbedScheduleTime.leadTime"},
			wantMessage: "must be greater than or equal to 0",
		},
		{
			name: "policy set without any probe defined",
			policy: &agentsv1alpha1.AutoPausePolicy{
				Pause: &agentsv1alpha1.PausePolicy{
					WhenProbedIdleState: &agentsv1alpha1.ProbedIdleStateRule{
						Probe:             "idle",
						MessageRegex:      "^idle$",
						ThresholdDuration: minute,
					},
				},
			},
			probes:      nil,
			wantErrs:    1,
			wantFields:  []string{"spec.autoPausePolicy.pause.whenProbedIdleState.probe"},
			wantMessage: "must reference a probe name defined in spec.probes",
		},
		{
			name: "unnamed probe cannot satisfy a policy reference",
			policy: &agentsv1alpha1.AutoPausePolicy{
				Pause: &agentsv1alpha1.PausePolicy{
					WhenProbedIdleState: &agentsv1alpha1.ProbedIdleStateRule{
						Probe:             "idle",
						MessageRegex:      "^idle$",
						ThresholdDuration: minute,
					},
				},
			},
			probes:      []agentsv1alpha1.Probe{execProbe("", "cat", "/tmp/idle")},
			wantErrs:    1,
			wantFields:  []string{"spec.autoPausePolicy.pause.whenProbedIdleState.probe"},
			wantMessage: "must reference a probe name defined in spec.probes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := ValidateAutoPausePolicy(tt.policy, tt.probes, field.NewPath("spec", "autoPausePolicy"))
			if len(errs) != tt.wantErrs {
				t.Fatalf("expected %d errors, got %d: %v", tt.wantErrs, len(errs), errs)
			}
			if tt.wantFields != nil && !reflect.DeepEqual(fieldPaths(errs), tt.wantFields) {
				t.Errorf("expected errors on fields %v, got %v", tt.wantFields, fieldPaths(errs))
			}
			if tt.wantMessage != "" && !strings.Contains(errs.ToAggregate().Error(), tt.wantMessage) {
				t.Errorf("expected error to contain %q, got %v", tt.wantMessage, errs.ToAggregate())
			}
		})
	}
}
