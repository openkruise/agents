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

package infra

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
)

// TestClaimMetrics_String tests the String() method of ClaimMetrics
func TestClaimMetrics_String(t *testing.T) {
	tests := []struct {
		name            string
		metrics         ClaimMetrics
		wantContains    []string
		shouldNotHave   []string
		checkSingleLine bool
	}{
		{
			name: "normal metrics without error",
			metrics: ClaimMetrics{
				Retries:     3,
				Total:       5 * time.Second,
				Wait:        1 * time.Second,
				RetryCost:   2 * time.Second,
				PickAndLock: 500 * time.Millisecond,
				WaitReady:   1500 * time.Millisecond,
				InitRuntime: 800 * time.Millisecond,
				CSIMount:    200 * time.Millisecond,
				LastError:   nil,
			},
			wantContains: []string{
				"Retries: 3",
				"Total: 5s",
				"Wait: 1s",
				"RetryCost: 2s",
				"PickAndLock: 500ms",
				"WaitReady: 1.5s",
				"InitRuntime: 800ms",
				"CSIMount: 200ms",
			},
			checkSingleLine: true,
		},
		{
			name: "metrics with simple error",
			metrics: ClaimMetrics{
				Retries:   2,
				Total:     3 * time.Second,
				LastError: errors.New("simple error message"),
			},
			wantContains: []string{
				"Retries: 2",
				"Total: 3s",
				"LastError: simple error message",
			},
			checkSingleLine: true,
		},
		{
			name: "metrics with error containing newline",
			metrics: ClaimMetrics{
				Retries:   1,
				Total:     2 * time.Second,
				LastError: errors.New("error with\nnewline"),
			},
			wantContains: []string{
				"Retries: 1",
				"LastError: error with newline",
			},
			shouldNotHave: []string{
				"\n",
			},
			checkSingleLine: true,
		},
		{
			name: "metrics with error containing multiple newlines",
			metrics: ClaimMetrics{
				Retries:   2,
				Total:     4 * time.Second,
				LastError: errors.New("line1\nline2\nline3\nline4"),
			},
			wantContains: []string{
				"Retries: 2",
				"LastError: line1 line2 line3 line4",
			},
			shouldNotHave: []string{
				"\n",
			},
			checkSingleLine: true,
		},
		{
			name: "metrics with error containing carriage return",
			metrics: ClaimMetrics{
				Retries:   1,
				Total:     1 * time.Second,
				LastError: errors.New("error with\rcarriage return"),
			},
			wantContains: []string{
				"LastError: error with carriage return",
			},
			shouldNotHave: []string{
				"\r",
			},
			checkSingleLine: true,
		},
		{
			name: "metrics with error containing tab",
			metrics: ClaimMetrics{
				Retries:   1,
				Total:     1 * time.Second,
				LastError: errors.New("error with\ttab"),
			},
			wantContains: []string{
				"LastError: error with tab",
			},
			shouldNotHave: []string{
				"\t",
			},
			checkSingleLine: true,
		},
		{
			name: "metrics with error containing mixed control characters",
			metrics: ClaimMetrics{
				Retries:   3,
				Total:     5 * time.Second,
				LastError: errors.New("error\nwith\rmixed\tcontrol\ncharacters"),
			},
			wantContains: []string{
				"Retries: 3",
				"LastError: error with mixed control characters",
			},
			shouldNotHave: []string{
				"\n",
				"\r",
				"\t",
			},
			checkSingleLine: true,
		},
		{
			name: "metrics with error containing consecutive newlines",
			metrics: ClaimMetrics{
				Retries:   1,
				Total:     2 * time.Second,
				LastError: errors.New("error\n\n\nwith\n\nconsecutive\nnewlines"),
			},
			wantContains: []string{
				"LastError: error   with  consecutive newlines",
			},
			shouldNotHave: []string{
				"\n",
			},
			checkSingleLine: true,
		},
		{
			name: "zero values",
			metrics: ClaimMetrics{
				Retries:     0,
				Total:       0,
				Wait:        0,
				RetryCost:   0,
				PickAndLock: 0,
				WaitReady:   0,
				InitRuntime: 0,
				CSIMount:    0,
				LastError:   nil,
			},
			wantContains: []string{
				"Retries: 0",
				"Total: 0s",
			},
			checkSingleLine: true,
		},
		{
			name: "realistic scenario with multiline k8s error",
			metrics: ClaimMetrics{
				Retries:     5,
				Total:       15 * time.Second,
				Wait:        2 * time.Second,
				RetryCost:   10 * time.Second,
				PickAndLock: 3 * time.Second,
				LastError:   errors.New("failed to lock sandbox: Operation cannot be fulfilled on sandboxes.agents.kruise.io \"sbx-test\":\nthe object has been modified; please apply your changes to the latest version and try again\nresourceVersion: 12345"),
			},
			wantContains: []string{
				"Retries: 5",
				"Total: 15s",
				"failed to lock sandbox",
				"the object has been modified",
				"resourceVersion: 12345",
			},
			shouldNotHave: []string{
				"\n",
			},
			checkSingleLine: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.metrics.String()

			// Check if output is single-line
			if tt.checkSingleLine {
				if strings.Contains(got, "\n") {
					t.Errorf("ClaimMetrics.String() output contains newline, should be single-line.\nGot: %s", got)
				}
				if strings.Contains(got, "\r") {
					t.Errorf("ClaimMetrics.String() output contains carriage return, should be single-line.\nGot: %s", got)
				}
				if strings.Contains(got, "\t") {
					t.Errorf("ClaimMetrics.String() output contains tab, should be single-line.\nGot: %s", got)
				}
			}

			// Check if output contains expected strings
			for _, want := range tt.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("ClaimMetrics.String() output missing expected substring.\nWant substring: %s\nGot: %s", want, got)
				}
			}

			// Check if output does not contain unwanted strings
			for _, unwanted := range tt.shouldNotHave {
				if strings.Contains(got, unwanted) {
					t.Errorf("ClaimMetrics.String() output contains unwanted substring.\nUnwanted: %q\nGot: %s", unwanted, got)
				}
			}

			// Verify the output starts with expected prefix
			if !strings.HasPrefix(got, "ClaimMetrics{") {
				t.Errorf("ClaimMetrics.String() output should start with 'ClaimMetrics{'\nGot: %s", got)
			}

			// Verify the output ends with expected suffix
			if !strings.HasSuffix(got, "}") {
				t.Errorf("ClaimMetrics.String() output should end with '}'\nGot: %s", got)
			}
		})
	}
}

// TestClaimMetrics_String_NilError tests that nil error is handled correctly
func TestClaimMetrics_String_NilError(t *testing.T) {
	metrics := ClaimMetrics{
		Retries:   1,
		Total:     1 * time.Second,
		LastError: nil,
	}

	got := metrics.String()

	// Should not contain "LastError: <nil>" or panic
	if !strings.Contains(got, "Retries: 1") {
		t.Errorf("ClaimMetrics.String() with nil error should still contain other fields")
	}

	// The output should be single-line
	if strings.Contains(got, "\n") {
		t.Errorf("ClaimMetrics.String() output should be single-line even with nil error")
	}
}

// TestClaimMetrics_String_EmptyError tests that empty error message is handled correctly
func TestClaimMetrics_String_EmptyError(t *testing.T) {
	metrics := ClaimMetrics{
		Retries:   1,
		Total:     1 * time.Second,
		LastError: errors.New(""),
	}

	got := metrics.String()

	// Should handle empty error gracefully
	if strings.Contains(got, "\n") {
		t.Errorf("ClaimMetrics.String() output should be single-line even with empty error")
	}
}

// TestClaimMetrics_String_LongError tests handling of very long error messages
func TestClaimMetrics_String_LongError(t *testing.T) {
	longError := strings.Repeat("error\nmessage\n", 100) // Create a very long multiline error
	metrics := ClaimMetrics{
		Retries:   10,
		Total:     30 * time.Second,
		LastError: errors.New(longError),
	}

	got := metrics.String()

	// Should still be single-line despite long error
	if strings.Contains(got, "\n") {
		t.Errorf("ClaimMetrics.String() output should be single-line even with long error.\nOutput length: %d", len(got))
	}

	// Should contain the error content (spaces instead of newlines)
	if !strings.Contains(got, "error message") {
		t.Errorf("ClaimMetrics.String() should contain the error content")
	}
}

func TestClaimMetrics_String_IncludesSecurityToken(t *testing.T) {
	tests := []struct {
		name     string
		metrics  ClaimMetrics
		expected string
	}{
		{
			name: "zero security token duration",
			metrics: ClaimMetrics{
				SecurityToken: 0,
			},
			expected: "SecurityToken: 0s",
		},
		{
			name: "non-zero security token duration",
			metrics: ClaimMetrics{
				SecurityToken: 250 * time.Millisecond,
			},
			expected: "SecurityToken: 250ms",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.metrics.String()
			if !strings.Contains(got, tt.expected) {
				t.Fatalf("ClaimMetrics.String() missing security token duration %q, got %q", tt.expected, got)
			}
		})
	}
}

func TestClaimMetrics_RecordPickSandboxFailure(t *testing.T) {
	tests := []struct {
		name     string
		record   func(metrics *ClaimMetrics)
		expected []PickSandboxFailure
	}{
		{
			name: "records new failure with count one",
			record: func(metrics *ClaimMetrics) {
				metrics.RecordPickSandboxFailure("default/sbx-1", errors.New("failed to lock sandbox"))
			},
			expected: []PickSandboxFailure{
				{Key: "default/sbx-1", Reason: "failed to lock sandbox", Count: 1},
			},
		},
		{
			name: "aggregates same key and reason",
			record: func(metrics *ClaimMetrics) {
				metrics.RecordPickSandboxFailure("default/sbx-1", errors.New("failed to lock sandbox"))
				metrics.RecordPickSandboxFailure("default/sbx-1", errors.New("failed to lock sandbox"))
			},
			expected: []PickSandboxFailure{
				{Key: "default/sbx-1", Reason: "failed to lock sandbox", Count: 2},
			},
		},
		{
			name: "keeps different reasons separate and sanitizes control characters",
			record: func(metrics *ClaimMetrics) {
				metrics.RecordPickSandboxFailure("", errors.New("no available\nsandboxes"))
				metrics.RecordPickSandboxFailure("", errors.New("all candidates\tpicked"))
			},
			expected: []PickSandboxFailure{
				{Key: "", Reason: "no available sandboxes", Count: 1},
				{Key: "", Reason: "all candidates picked", Count: 1},
			},
		},
		{
			name: "ignores nil error",
			record: func(metrics *ClaimMetrics) {
				metrics.RecordPickSandboxFailure("default/sbx-1", nil)
			},
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metrics := &ClaimMetrics{}
			tt.record(metrics)
			if len(metrics.PickSandboxFailures) != len(tt.expected) {
				t.Fatalf("expected %d failures, got %d: %#v", len(tt.expected), len(metrics.PickSandboxFailures), metrics.PickSandboxFailures)
			}
			for i := range tt.expected {
				if metrics.PickSandboxFailures[i] != tt.expected[i] {
					t.Fatalf("failure[%d] = %#v, want %#v", i, metrics.PickSandboxFailures[i], tt.expected[i])
				}
			}
		})
	}
}

func TestClaimMetrics_MergePickSandboxFailures(t *testing.T) {
	metrics := &ClaimMetrics{
		PickSandboxFailures: []PickSandboxFailure{
			{Key: "default/sbx-1", Reason: "failed to lock sandbox", Count: 2},
		},
	}
	metrics.MergePickSandboxFailures([]PickSandboxFailure{
		{Key: "default/sbx-1", Reason: "failed to lock sandbox", Count: 3},
		{Key: "", Reason: "no available sandboxes", Count: 4},
	})

	expected := []PickSandboxFailure{
		{Key: "default/sbx-1", Reason: "failed to lock sandbox", Count: 5},
		{Key: "", Reason: "no available sandboxes", Count: 4},
	}
	if len(metrics.PickSandboxFailures) != len(expected) {
		t.Fatalf("expected %d failures, got %d: %#v", len(expected), len(metrics.PickSandboxFailures), metrics.PickSandboxFailures)
	}
	for i := range expected {
		if metrics.PickSandboxFailures[i] != expected[i] {
			t.Fatalf("failure[%d] = %#v, want %#v", i, metrics.PickSandboxFailures[i], expected[i])
		}
	}
}

func TestCloneMetrics_String(t *testing.T) {
	tests := []struct {
		name          string
		metrics       CloneMetrics
		wantContains  []string
		shouldNotHave []string
	}{
		{
			name: "all durations and retries",
			metrics: CloneMetrics{
				Retries:       2,
				Wait:          time.Second,
				GetTemplate:   2 * time.Second,
				CreateSandbox: 3 * time.Second,
				WaitReady:     4 * time.Second,
				InitRuntime:   5 * time.Second,
				CSIMount:      6 * time.Second,
				Total:         21 * time.Second,
			},
			wantContains: []string{
				"Retries: 2",
				"Wait: 1s",
				"GetTemplate: 2s",
				"CreateSandbox: 3s",
				"WaitReady: 4s",
				"InitRuntime: 5s",
				"CSIMount: 6s",
				"Total: 21s",
			},
		},
		{
			name: "sanitizes last error",
			metrics: CloneMetrics{
				Retries:   1,
				LastError: errors.New("clone failed\nresource conflict\ttry again"),
			},
			wantContains: []string{
				"Retries: 1",
				"LastError: clone failed resource conflict try again",
			},
			shouldNotHave: []string{
				"\n",
				"\t",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.metrics.String()
			for _, want := range tt.wantContains {
				if !strings.Contains(got, want) {
					t.Fatalf("CloneMetrics.String() missing expected substring %q, got %q", want, got)
				}
			}
			for _, unwanted := range tt.shouldNotHave {
				if strings.Contains(got, unwanted) {
					t.Fatalf("CloneMetrics.String() contains unwanted substring %q, got %q", unwanted, got)
				}
			}
			if !strings.HasPrefix(got, "CloneMetrics{") {
				t.Fatalf("CloneMetrics.String() should start with CloneMetrics{, got %q", got)
			}
			if !strings.HasSuffix(got, "}") {
				t.Fatalf("CloneMetrics.String() should end with }, got %q", got)
			}
		})
	}
}

func TestCloneMetrics_Merge(t *testing.T) {
	tests := []struct {
		name     string
		initial  CloneMetrics
		src      CloneMetrics
		expected CloneMetrics
	}{
		{
			name: "accumulates per-attempt durations",
			initial: CloneMetrics{
				Retries:       3,
				Wait:          time.Second,
				GetTemplate:   2 * time.Second,
				CreateSandbox: 3 * time.Second,
				WaitReady:     4 * time.Second,
				InitRuntime:   5 * time.Second,
				CSIMount:      6 * time.Second,
				Total:         21 * time.Second,
				LastError:     errors.New("outer retry error"),
			},
			src: CloneMetrics{
				Retries:       9,
				Wait:          10 * time.Millisecond,
				GetTemplate:   20 * time.Millisecond,
				CreateSandbox: 30 * time.Millisecond,
				WaitReady:     40 * time.Millisecond,
				InitRuntime:   50 * time.Millisecond,
				CSIMount:      60 * time.Millisecond,
				Total:         210 * time.Millisecond,
				LastError:     errors.New("per-attempt error"),
			},
			expected: CloneMetrics{
				Retries:       3,
				Wait:          time.Second + 10*time.Millisecond,
				GetTemplate:   2*time.Second + 20*time.Millisecond,
				CreateSandbox: 3*time.Second + 30*time.Millisecond,
				WaitReady:     4*time.Second + 40*time.Millisecond,
				InitRuntime:   5*time.Second + 50*time.Millisecond,
				CSIMount:      6*time.Second + 60*time.Millisecond,
				Total:         21*time.Second + 210*time.Millisecond,
				LastError:     errors.New("outer retry error"),
			},
		},
		{
			name: "keeps zero values stable",
			initial: CloneMetrics{
				Retries:   1,
				LastError: errors.New("failed"),
			},
			src: CloneMetrics{},
			expected: CloneMetrics{
				Retries:   1,
				LastError: errors.New("failed"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.initial.Merge(tt.src)
			if tt.initial.Retries != tt.expected.Retries {
				t.Fatalf("Retries = %d, want %d", tt.initial.Retries, tt.expected.Retries)
			}
			if tt.initial.Wait != tt.expected.Wait {
				t.Fatalf("Wait = %v, want %v", tt.initial.Wait, tt.expected.Wait)
			}
			if tt.initial.GetTemplate != tt.expected.GetTemplate {
				t.Fatalf("GetTemplate = %v, want %v", tt.initial.GetTemplate, tt.expected.GetTemplate)
			}
			if tt.initial.CreateSandbox != tt.expected.CreateSandbox {
				t.Fatalf("CreateSandbox = %v, want %v", tt.initial.CreateSandbox, tt.expected.CreateSandbox)
			}
			if tt.initial.WaitReady != tt.expected.WaitReady {
				t.Fatalf("WaitReady = %v, want %v", tt.initial.WaitReady, tt.expected.WaitReady)
			}
			if tt.initial.InitRuntime != tt.expected.InitRuntime {
				t.Fatalf("InitRuntime = %v, want %v", tt.initial.InitRuntime, tt.expected.InitRuntime)
			}
			if tt.initial.CSIMount != tt.expected.CSIMount {
				t.Fatalf("CSIMount = %v, want %v", tt.initial.CSIMount, tt.expected.CSIMount)
			}
			if tt.initial.Total != tt.expected.Total {
				t.Fatalf("Total = %v, want %v", tt.initial.Total, tt.expected.Total)
			}
			if tt.initial.LastError.Error() != tt.expected.LastError.Error() {
				t.Fatalf("LastError = %q, want %q", tt.initial.LastError.Error(), tt.expected.LastError.Error())
			}
		})
	}
}

func TestReserveFailedSandboxFor_JSON(t *testing.T) {
	tests := []struct {
		name       string
		options    any
		fieldName  string
		expectJSON string
	}{
		{
			name: "claim nil reserve duration keeps explicit null",
			options: ClaimSandboxOptions{
				ReserveFailedSandboxFor: nil,
			},
			fieldName:  "reserveFailedSandboxFor",
			expectJSON: `"reserveFailedSandboxFor":null`,
		},
		{
			name: "claim reserve forever uses sentinel duration",
			options: ClaimSandboxOptions{
				ReserveFailedSandboxFor: durationPtr(consts.ReserveFailedSandboxForever),
			},
			fieldName:  "reserveFailedSandboxFor",
			expectJSON: `"reserveFailedSandboxFor":-1`,
		},
		{
			name: "claim reserve positive duration uses nanoseconds",
			options: ClaimSandboxOptions{
				ReserveFailedSandboxFor: durationPtr(90 * time.Second),
			},
			fieldName:  "reserveFailedSandboxFor",
			expectJSON: `"reserveFailedSandboxFor":90000000000`,
		},
		{
			name: "clone reserve never uses sentinel duration",
			options: CloneSandboxOptions{
				ReserveFailedSandboxFor: durationPtr(consts.ReserveFailedSandboxNever),
			},
			fieldName:  "reserveFailedSandboxFor",
			expectJSON: `"reserveFailedSandboxFor":0`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(tt.options)
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			if !strings.Contains(string(got), tt.expectJSON) {
				t.Fatalf("json output missing %s, got %s", tt.expectJSON, got)
			}

			var decoded map[string]any
			if err := json.Unmarshal(got, &decoded); err != nil {
				t.Fatalf("json.Unmarshal() error = %v", err)
			}
			if _, ok := decoded[tt.fieldName]; !ok {
				t.Fatalf("json output missing field %q: %s", tt.fieldName, got)
			}
		})
	}
}

func durationPtr(duration time.Duration) *time.Duration {
	return &duration
}
