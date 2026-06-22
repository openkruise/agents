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

package storage

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
)

// fakeProvider is a Provider stub used to drive registry tests without
// touching real CSI sockets.
type fakeProvider struct {
	driver string
	subDir string
}

func (f *fakeProvider) Driver() string                                          { return f.driver }
func (f *fakeProvider) SubDir() string                                          { return f.subDir }
func (f *fakeProvider) Validate(_ csi.NodePublishVolumeRequest) error           { return nil }
func (f *fakeProvider) Mount(_ context.Context, _ csi.NodePublishVolumeRequest, _ bool) error {
	return nil
}
func (f *fakeProvider) Unmount(_ context.Context, _ csi.NodePublishVolumeRequest) error {
	return nil
}

func TestRegistry(t *testing.T) {
	tests := []struct {
		name      string
		seed      []Provider
		lookup    string
		wantFound bool
		wantList  []string
	}{
		{
			name:      "empty registry returns no providers",
			lookup:    "any",
			wantFound: false,
			wantList:  []string{},
		},
		{
			name: "single driver round trip",
			seed: []Provider{
				&fakeProvider{driver: "drv-a", subDir: "a"},
			},
			lookup:    "drv-a",
			wantFound: true,
			wantList:  []string{"drv-a"},
		},
		{
			name: "drivers list is sorted",
			seed: []Provider{
				&fakeProvider{driver: "drv-c", subDir: "c"},
				&fakeProvider{driver: "drv-a", subDir: "a"},
				&fakeProvider{driver: "drv-b", subDir: "b"},
			},
			lookup:    "drv-b",
			wantFound: true,
			wantList:  []string{"drv-a", "drv-b", "drv-c"},
		},
		{
			name: "lookup miss",
			seed: []Provider{
				&fakeProvider{driver: "drv-a", subDir: "a"},
			},
			lookup:    "drv-x",
			wantFound: false,
			wantList:  []string{"drv-a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetRegistryForTesting()
			defer resetRegistryForTesting()

			for _, p := range tt.seed {
				Register(p)
			}

			_, ok := Lookup(tt.lookup)
			assert.Equal(t, tt.wantFound, ok)
			assert.Equal(t, tt.wantList, Drivers())
		})
	}
}

func TestRegisterPanics(t *testing.T) {
	tests := []struct {
		name        string
		setup       func()
		call        func()
		expectPanic string
	}{
		{
			name:        "nil provider panics",
			setup:       func() {},
			call:        func() { Register(nil) },
			expectPanic: "nil Provider",
		},
		{
			name:        "empty driver name panics",
			setup:       func() {},
			call:        func() { Register(&fakeProvider{driver: ""}) },
			expectPanic: "empty Driver()",
		},
		{
			name: "duplicate driver panics",
			setup: func() {
				Register(&fakeProvider{driver: "dup", subDir: "d"})
			},
			call:        func() { Register(&fakeProvider{driver: "dup", subDir: "d2"}) },
			expectPanic: `already registered for driver "dup"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetRegistryForTesting()
			defer resetRegistryForTesting()

			tt.setup()

			recovered := capturePanic(tt.call)
			if recovered == nil {
				t.Fatalf("expected panic containing %q, got none", tt.expectPanic)
			}
			msg, ok := recovered.(string)
			if !ok {
				t.Fatalf("panic value is not a string: %T %v", recovered, recovered)
			}
			assert.Contains(t, msg, tt.expectPanic)
		})
	}
}

// capturePanic runs fn and returns the recovered panic value, or nil if fn
// returned normally.
func capturePanic(fn func()) (recovered any) {
	defer func() {
		recovered = recover()
	}()
	fn()
	return nil
}
