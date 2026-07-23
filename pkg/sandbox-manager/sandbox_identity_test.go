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

package sandbox_manager

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/sandboxid"
)

func TestGuardPreModifier(t *testing.T) {
	tests := []struct {
		name           string
		labels         map[string]string
		modifier       func(infra.Sandbox) error
		expectError    string
		expectReserved bool
	}{
		{name: "nil modifier stays nil"},
		{
			name: "unrelated mutation succeeds",
			modifier: func(sandbox infra.Sandbox) error {
				sandbox.SetAnnotations(map[string]string{"example": "value"})
				return nil
			},
		},
		{
			name: "reserved addition is rejected",
			modifier: func(sandbox infra.Sandbox) error {
				sandbox.SetLabels(map[string]string{sandboxid.LabelKey: "spoofed"})
				return nil
			},
			expectError:    "reserved sandbox ID label was mutated",
			expectReserved: true,
		},
		{
			name:   "reserved deletion is rejected",
			labels: map[string]string{sandboxid.LabelKey: "existing"},
			modifier: func(sandbox infra.Sandbox) error {
				delete(sandbox.GetLabels(), sandboxid.LabelKey)
				return nil
			},
			expectError:    "reserved sandbox ID label was mutated",
			expectReserved: true,
		},
		{
			name:   "reserved value change is rejected",
			labels: map[string]string{sandboxid.LabelKey: "existing"},
			modifier: func(sandbox infra.Sandbox) error {
				sandbox.GetLabels()[sandboxid.LabelKey] = "changed"
				return nil
			},
			expectError:    "reserved sandbox ID label was mutated",
			expectReserved: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			modifier := guardPreModifier(tt.modifier)
			if tt.modifier == nil {
				assert.Nil(t, modifier)
				return
			}
			sandbox := sandboxcr.AsSandbox(&agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Labels: tt.labels}}, nil)
			err := modifier(sandbox)
			if tt.expectError == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectError)
			assert.Equal(t, tt.expectReserved, errors.Is(err, ErrReservedSandboxIDMutation))
		})
	}
}

func TestTrackSandboxIDAssignmentObject(t *testing.T) {
	tests := []struct {
		name          string
		enabled       bool
		caller        bool
		callerError   string
		expectNil     bool
		expectCalls   int
		expectTracked bool
	}{
		{name: "disabled nil modifier stays nil", expectNil: true},
		{name: "disabled caller remains untracked", caller: true, expectCalls: 1},
		{name: "enabled nil modifier tracks selected object", enabled: true, expectTracked: true},
		{name: "enabled caller error preserves tracking", enabled: true, caller: true, callerError: "caller failed", expectCalls: 1, expectTracked: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &sandboxIDAssignmentState{enabled: tt.enabled}
			calls := 0
			var modifier func(infra.Sandbox) error
			if tt.caller {
				modifier = func(infra.Sandbox) error {
					calls++
					if tt.callerError != "" {
						return errors.New(tt.callerError)
					}
					return nil
				}
			}

			tracked := trackSandboxIDAssignmentObject(state, modifier)
			if tt.expectNil {
				assert.Nil(t, tracked)
				return
			}
			require.NotNil(t, tracked)
			sandbox := sandboxcr.AsSandbox(&agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{
				Namespace: "team-a",
				Name:      "sandbox-a",
			}}, nil)
			err := tracked(sandbox)
			if tt.callerError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.callerError)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.expectCalls, calls)
			if tt.expectTracked {
				assert.Equal(t, "team-a", state.namespace)
				assert.Equal(t, "sandbox-a", state.name)
			} else {
				assert.Empty(t, state.namespace)
				assert.Empty(t, state.name)
			}
		})
	}
}

func TestComposePostModifier(t *testing.T) {
	callerErr := errors.New("caller failed")
	tests := []struct {
		name             string
		enableAssignment bool
		uid              types.UID
		labels           map[string]string
		modifier         func(metav1.Object) (bool, error)
		expectNil        bool
		expectChanged    bool
		expectID         string
		expectError      string
		expectReserved   bool
		expectAssigned   bool
		expectAnnotation string
	}{
		{name: "disabled without caller stays nil", expectNil: true},
		{
			name: "disabled caller mutation is preserved",
			modifier: func(object metav1.Object) (bool, error) {
				object.SetAnnotations(map[string]string{"example": "value"})
				return true, nil
			},
			expectChanged: true,
		},
		{
			name: "caller cannot add reserved label",
			modifier: func(object metav1.Object) (bool, error) {
				object.SetLabels(map[string]string{sandboxid.LabelKey: "spoofed"})
				return true, nil
			},
			expectError:    "reserved sandbox ID label was mutated",
			expectReserved: true,
		},
		{
			name:   "caller cannot delete existing empty entry",
			labels: map[string]string{sandboxid.LabelKey: ""},
			modifier: func(object metav1.Object) (bool, error) {
				delete(object.GetLabels(), sandboxid.LabelKey)
				return true, nil
			},
			expectError:    "reserved sandbox ID label was mutated",
			expectReserved: true,
		},
		{
			name:   "caller cannot change existing value",
			labels: map[string]string{sandboxid.LabelKey: "existing"},
			modifier: func(object metav1.Object) (bool, error) {
				object.GetLabels()[sandboxid.LabelKey] = "changed"
				return true, nil
			},
			expectError:    "reserved sandbox ID label was mutated",
			expectReserved: true,
		},
		{
			name:             "caller runs before core assignment",
			enableAssignment: true,
			uid:              types.UID("00000000-0000-0000-0000-000000000001"),
			modifier: func(object metav1.Object) (bool, error) {
				if _, present := object.GetLabels()[sandboxid.LabelKey]; present {
					return false, errors.New("core assignment ran before caller")
				}
				object.SetAnnotations(map[string]string{"order": "caller-first"})
				return true, nil
			},
			expectChanged:    true,
			expectID:         "aaaaaaaaaaaaaaaaaaaaaaaaae",
			expectAssigned:   true,
			expectAnnotation: "caller-first",
		},
		{
			name:             "enabled assignment uses final UID",
			enableAssignment: true,
			uid:              types.UID("00000000-0000-0000-0000-000000000001"),
			expectChanged:    true,
			expectID:         "aaaaaaaaaaaaaaaaaaaaaaaaae",
			expectAssigned:   true,
		},
		{
			name:             "enabled assignment preserves existing ID",
			enableAssignment: true,
			uid:              types.UID("invalid"),
			labels:           map[string]string{sandboxid.LabelKey: "existing"},
			expectID:         "existing",
		},
		{
			name:             "caller failure stops assignment",
			enableAssignment: true,
			uid:              types.UID("00000000-0000-0000-0000-000000000001"),
			modifier: func(metav1.Object) (bool, error) {
				return false, callerErr
			},
			expectError: "caller failed",
		},
		{
			name:             "invalid UID fails assignment",
			enableAssignment: true,
			uid:              types.UID("invalid"),
			expectError:      "invalid sandbox UID",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			object := &metav1.ObjectMeta{UID: tt.uid, Labels: tt.labels}
			modifier, state := composePostModifier(tt.modifier, tt.enableAssignment)
			if tt.expectNil {
				assert.Nil(t, modifier)
				return
			}
			require.NotNil(t, modifier)

			changed, err := modifier(object)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				assert.Equal(t, tt.expectReserved, errors.Is(err, ErrReservedSandboxIDMutation))
				assert.False(t, changed)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.expectChanged, changed)
			assert.Equal(t, tt.expectID, object.GetLabels()[sandboxid.LabelKey])
			assert.Equal(t, tt.expectAssigned, state.assigned)
			assert.Equal(t, tt.expectAnnotation, object.GetAnnotations()["order"])
		})
	}
}

func TestSandboxIDAssignmentErrorReason(t *testing.T) {
	operationErr := errors.New("assignment operation failed")
	tests := []struct {
		name     string
		state    *sandboxIDAssignmentState
		duration time.Duration
		err      error
		expect   string
	}{
		{name: "nil state", duration: time.Nanosecond, err: operationErr},
		{name: "assignment disabled", state: &sandboxIDAssignmentState{}, duration: time.Nanosecond, err: operationErr},
		{name: "successful operation", state: &sandboxIDAssignmentState{enabled: true}, duration: time.Nanosecond},
		{name: "failure before final stage", state: &sandboxIDAssignmentState{enabled: true}, err: operationErr},
		{name: "caller callback failure", state: &sandboxIDAssignmentState{enabled: true, callerFailed: true}, duration: time.Nanosecond, err: operationErr},
		{name: "direct read failure", state: &sandboxIDAssignmentState{enabled: true}, duration: time.Nanosecond, err: operationErr, expect: "read_failed"},
		{name: "invalid UID", state: &sandboxIDAssignmentState{enabled: true, assignmentRan: true, invalidUID: true}, duration: time.Nanosecond, err: operationErr, expect: "invalid_uid"},
		{name: "context canceled", state: &sandboxIDAssignmentState{enabled: true}, duration: time.Nanosecond, err: context.Canceled, expect: "context_done"},
		{name: "context deadline", state: &sandboxIDAssignmentState{enabled: true}, duration: time.Nanosecond, err: context.DeadlineExceeded, expect: "context_done"},
		{name: "update failure", state: &sandboxIDAssignmentState{enabled: true, assignmentRan: true}, duration: time.Nanosecond, err: operationErr, expect: "update_failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expect, sandboxIDAssignmentErrorReason(tt.state, tt.duration, tt.err))
		})
	}
}
