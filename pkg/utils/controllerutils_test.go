package utils

import (
	"fmt"
	"sync"
	"testing"
)

func TestDoItSlowly(t *testing.T) {
	tests := []struct {
		name             string
		count            int
		initialBatchSize int
		failAtCall       int // -1 means no failure
		wantSuccesses    int
		wantError        bool
	}{
		{
			name:             "all succeed with count 1",
			count:            1,
			initialBatchSize: 1,
			failAtCall:       -1,
			wantSuccesses:    1,
			wantError:        false,
		},
		{
			name:             "all succeed with count 5",
			count:            5,
			initialBatchSize: 1,
			failAtCall:       -1,
			wantSuccesses:    5,
			wantError:        false,
		},
		{
			name:             "all succeed with larger batch size",
			count:            10,
			initialBatchSize: 5,
			failAtCall:       -1,
			wantSuccesses:    10,
			wantError:        false,
		},
		{
			name:             "zero count",
			count:            0,
			initialBatchSize: 1,
			failAtCall:       -1,
			wantSuccesses:    0,
			wantError:        false,
		},
		{
			name:             "failure in first batch",
			count:            5,
			initialBatchSize: 2,
			failAtCall:       1, // fail on first call
			wantSuccesses:    1, // at least one succeeds before failure is detected
			wantError:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			callCount := 0
			var mu sync.Mutex

			fn := func() error {
				mu.Lock()
				callCount++
				currentCall := callCount
				mu.Unlock()

				if tt.failAtCall > 0 && currentCall >= tt.failAtCall {
					return fmt.Errorf("intentional failure at call %d", currentCall)
				}
				return nil
			}

			successes, err := DoItSlowly(tt.count, tt.initialBatchSize, fn)

			if tt.wantError {
				if err == nil {
					t.Errorf("DoItSlowly() expected error but got none")
				}
			} else {
				if err != nil {
					t.Errorf("DoItSlowly() unexpected error: %v", err)
				}
			}

			if !tt.wantError && successes != tt.wantSuccesses {
				t.Errorf("DoItSlowly() successes = %d, want %d", successes, tt.wantSuccesses)
			}
		})
	}
}

func TestDoItSlowlyWithInputs(t *testing.T) {
	tests := []struct {
		name             string
		inputs           []int
		initialBatchSize int
		failOnInput      int // -1 means no failure
		wantSuccesses    int
		wantError        bool
	}{
		{
			name:             "all succeed empty inputs",
			inputs:           []int{},
			initialBatchSize: 1,
			failOnInput:      -1,
			wantSuccesses:    0,
			wantError:        false,
		},
		{
			name:             "all succeed single input",
			inputs:           []int{1},
			initialBatchSize: 1,
			failOnInput:      -1,
			wantSuccesses:    1,
			wantError:        false,
		},
		{
			name:             "all succeed multiple inputs",
			inputs:           []int{1, 2, 3, 4, 5},
			initialBatchSize: 2,
			failOnInput:      -1,
			wantSuccesses:    5,
			wantError:        false,
		},
		{
			name:             "process string inputs",
			inputs:           []int{10, 20, 30},
			initialBatchSize: 1,
			failOnInput:      -1,
			wantSuccesses:    3,
			wantError:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			processedInputs := make([]int, 0)
			var mu sync.Mutex

			fn := func(input int) error {
				mu.Lock()
				processedInputs = append(processedInputs, input)
				mu.Unlock()

				if tt.failOnInput > 0 && input == tt.failOnInput {
					return fmt.Errorf("intentional failure on input %d", input)
				}
				return nil
			}

			successes, err := DoItSlowlyWithInputs(tt.inputs, tt.initialBatchSize, fn)

			if tt.wantError {
				if err == nil {
					t.Errorf("DoItSlowlyWithInputs() expected error but got none")
				}
			} else {
				if err != nil {
					t.Errorf("DoItSlowlyWithInputs() unexpected error: %v", err)
				}
			}

			if !tt.wantError && successes != tt.wantSuccesses {
				t.Errorf("DoItSlowlyWithInputs() successes = %d, want %d", successes, tt.wantSuccesses)
			}

			// Verify all inputs were processed when no error
			if !tt.wantError && len(processedInputs) != len(tt.inputs) {
				t.Errorf("DoItSlowlyWithInputs() processed %d inputs, want %d", len(processedInputs), len(tt.inputs))
			}
		})
	}
}
