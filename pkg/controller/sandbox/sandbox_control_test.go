/*
Copyright 2025.

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

package sandbox

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/controller/sandbox/core"
)

func TestNewSandboxControl(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	tests := []struct {
		name        string
		wantNil     bool
		wantControl bool
	}{
		{
			name:        "returns non-nil map with common control",
			wantNil:     false,
			wantControl: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
			recorder := record.NewFakeRecorder(10)
			rl := core.NewRateLimiter()

			controls := core.NewSandboxControl(fakeClient, recorder, rl)

			if tt.wantNil && controls != nil {
				t.Errorf("NewSandboxControl() expected nil, got %v", controls)
			}
			if !tt.wantNil && controls == nil {
				t.Fatal("NewSandboxControl() returned nil, want non-nil")
			}

			if tt.wantControl {
				ctrl, ok := controls[core.CommonControlName]
				if !ok {
					t.Fatalf("NewSandboxControl() missing key %q", core.CommonControlName)
				}
				if ctrl == nil {
					t.Fatal("NewSandboxControl() common control is nil")
				}
				// Verify the returned value implements SandboxControl interface
				var _ core.SandboxControl = ctrl
			}
		})
	}
}
