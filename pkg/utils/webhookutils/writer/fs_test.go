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

package writer

import (
	"os"
	"testing"

	"github.com/openkruise/agents/pkg/utils/webhookutils/generator"
)

func TestPrepareToWrite(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "cert-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Test directory creation
	testDir := tempDir + "/new-dir"
	err = prepareToWrite(testDir)
	if err != nil {
		t.Fatalf("prepareToWrite failed: %v", err)
	}

	info, err := os.Stat(testDir)
	if err != nil {
		t.Fatalf("Failed to stat test dir: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("Expected %s to be a directory", testDir)
	}
	// Verify directory permissions (0755)
	if mode := info.Mode().Perm(); mode != 0755 {
		t.Errorf("Expected directory permissions 0755, got %o", mode)
	}
}

func TestCertToProjectionMap(t *testing.T) {
	artifacts := &generator.Artifacts{
		CAKey:  []byte("ca-key"),
		CACert: []byte("ca-cert"),
		Cert:   []byte("cert"),
		Key:    []byte("key"),
	}

	projectionMap := certToProjectionMap(artifacts)

	tests := []struct {
		name         string
		expectedMode int32
	}{
		{CAKeyName, 0600},
		{CACertName, 0644},
		{ServerCertName, 0644},
		{ServerCertName2, 0644},
		{ServerKeyName, 0600},
		{ServerKeyName2, 0600},
	}

	for _, tt := range tests {
		proj, ok := projectionMap[tt.name]
		if !ok {
			t.Errorf("Missing entry for %s in projection map", tt.name)
			continue
		}
		if proj.Mode != tt.expectedMode {
			t.Errorf("Expected mode %o for %s, got %o", tt.expectedMode, tt.name, proj.Mode)
		}
	}
}
