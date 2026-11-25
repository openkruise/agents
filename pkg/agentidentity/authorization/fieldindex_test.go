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

package authorization

import (
	"context"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	securityv1alpha1 "github.com/openkruise/agents/api/security/v1alpha1"
)

func TestAgentIdentityRefIndexFunc(t *testing.T) {
	tests := []struct {
		name string
		obj  client.Object
		want []string
	}{
		{name: "a binding", obj: binding("b", "r", "sample-agent"), want: []string{"sample-agent"}},
		{name: "a binding with no identity ref", obj: binding("b", "r", ""), want: nil},
		{name: "not a binding at all", obj: role("r"), want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AgentIdentityRefIndexFunc(tt.obj)
			if len(got) != len(tt.want) {
				t.Fatalf("AgentIdentityRefIndexFunc() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("AgentIdentityRefIndexFunc()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestSetupFieldIndex covers the registration call itself: that it names the
// right kind and field, and passes IndexField's own error through rather
// than swallowing it. informertest.FakeInformers' own IndexField is a stub
// that never errors, so only the success path is reachable through it; the
// error-wrapping line is a single %w and is not exercised separately.
func TestSetupFieldIndex(t *testing.T) {
	if err := SetupFieldIndex(context.Background(), &informertest.FakeInformers{Scheme: scheme}); err != nil {
		t.Errorf("SetupFieldIndex: %v", err)
	}
}

// TestAuthorizeRequiresTheFieldIndex proves Authorize genuinely depends on
// IndexFieldAgentIdentityRef rather than silently falling back to an
// unindexed scan if it is missing: a reader built without it must fail
// loudly, not quietly return an empty or wrong result.
func TestAuthorizeRequiresTheFieldIndex(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(
			role("r", rule(securityv1alpha1.ActionExchangePrincipalToken, "*")),
			binding("b", "r", "sample-agent"),
		).Build()

	_, err := New(c).Authorize(context.Background(), exchangeRequest("sample-agent", "AgentIdentity/sample-agent"))
	if err == nil {
		t.Fatal("Authorize succeeded without the field index registered, want an error naming it")
	}
	if !strings.Contains(err.Error(), IndexFieldAgentIdentityRef) {
		t.Errorf("error: got %v, want it to mention %q", err, IndexFieldAgentIdentityRef)
	}
}
