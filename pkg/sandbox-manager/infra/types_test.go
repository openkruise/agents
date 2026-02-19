package infra

import (
	"errors"
	"strings"
	"testing"
	"time"
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
