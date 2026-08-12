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

package sandboxid

import (
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func TestResolve(t *testing.T) {
	tests := []struct {
		name     string
		labels   map[string]string
		expected string
	}{
		{name: "non-empty label is authoritative", labels: map[string]string{LabelKey: "operator-assigned-value"}, expected: "operator-assigned-value"},
		{name: "absent label uses legacy ID", labels: map[string]string{"app": "sandbox"}, expected: "team-a--sandbox-a"},
		{name: "empty label uses legacy ID", labels: map[string]string{LabelKey: ""}, expected: "team-a--sandbox-a"},
		{name: "nil labels use legacy ID", labels: nil, expected: "team-a--sandbox-a"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandbox := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{
				Namespace: "team-a",
				Name:      "sandbox-a",
				Labels:    tt.labels,
			}}
			assert.Equal(t, tt.expected, Resolve(sandbox))
		})
	}
}

func TestLegacy(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		sandbox   string
		expected  string
	}{
		{name: "standard names", namespace: "team-a", sandbox: "sandbox-a", expected: "team-a--sandbox-a"},
		{name: "name contains separator", namespace: "team-a", sandbox: "sandbox--a", expected: "team-a--sandbox--a"},
		{name: "empty namespace preserves encoding", namespace: "", sandbox: "sandbox-a", expected: "--sandbox-a"},
		{name: "empty name preserves encoding", namespace: "team-a", sandbox: "", expected: "team-a--"},
	}

	assert.Equal(t, agentsv1alpha1.LabelSandboxID, LabelKey)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, Legacy(tt.namespace, tt.sandbox))
		})
	}
}

func TestEncodeShortID(t *testing.T) {
	assert.Equal(t, "aaaaaaaaaaaac", encodeShortID(1))
	assert.Len(t, encodeShortID(1<<62), ShortIDLength)
}

func TestGenerator(t *testing.T) {
	generator, err := NewGenerator(7)
	require.NoError(t, err)

	const count = 1000
	ids := make(chan string, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := generator()
			if err != nil {
				errs <- err
				return
			}
			ids <- id
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	seen := make(map[string]struct{}, count)
	for id := range ids {
		assert.Len(t, id, ShortIDLength)
		assert.Regexp(t, `^[a-z2-7]{13}$`, id)
		if _, found := seen[id]; found {
			t.Fatalf("duplicate generated ID %q", id)
		}
		seen[id] = struct{}{}
	}
	assert.Len(t, seen, count)
}

func TestNewGeneratorWorkerIDBoundary(t *testing.T) {
	assert.Equal(t, 63, 41+workerIDBits+sequenceBits)

	_, err := NewGenerator(WorkerIDLimit - 1)
	require.NoError(t, err)

	_, err = NewGenerator(WorkerIDLimit)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid machine id")
}

func TestValidatePrefix(t *testing.T) {
	tests := []struct {
		name        string
		prefix      string
		expectError string
	}{
		{name: "empty prefix"},
		{name: "lowercase prefix", prefix: "prod-"},
		{name: "numeric prefix", prefix: "42-"},
		{name: "long prefix is accepted", prefix: strings.Repeat("a", 128)},
		{name: "uppercase is rejected", prefix: "Prod-", expectError: "invalid"},
		{name: "underscore is rejected", prefix: "prod_", expectError: "invalid"},
		{name: "leading hyphen is rejected", prefix: "-prod", expectError: "invalid"},
		{name: "legacy separator is rejected", prefix: "prod--x", expectError: "legacy ID separator"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePrefix(tt.prefix)
			if tt.expectError == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectError)
		})
	}
}
