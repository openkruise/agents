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

package discovery

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	fakediscovery "k8s.io/client-go/discovery/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/openkruise/agents/api/v1alpha1"
)

func TestDiscoverGVKRetriesOnlyRecoverableErrors(t *testing.T) {
	gvk := v1alpha1.GroupVersion.WithKind("TrafficPolicy")
	groupVersion := gvk.GroupVersion().String()

	tests := []struct {
		name string
		// resources is what the API server reports for the group version. Omitting
		// the group version makes the fake return an error, standing in for a
		// discovery failure the caller may still recover from.
		resources []*metav1.APIResourceList
		want      bool
		wantCalls int
	}{
		{
			name: "kind present is discovered in one call",
			resources: []*metav1.APIResourceList{{
				GroupVersion: groupVersion,
				APIResources: []metav1.APIResource{{Kind: gvk.Kind}},
			}},
			want:      true,
			wantCalls: 1,
		},
		{
			// Discovery answered and the kind is absent, so the answer cannot
			// change; retrying would only delay an uninstalled optional CRD.
			name: "absent kind is reported without retrying",
			resources: []*metav1.APIResourceList{{
				GroupVersion: groupVersion,
				APIResources: []metav1.APIResource{{Kind: "SomeOtherKind"}},
			}},
			want:      false,
			wantCalls: 1,
		},
		{
			// Discovery failure keeps its existing retry-then-fail-open
			// behavior: this fix narrows only the absent-kind case.
			name:      "failed discovery is retried",
			resources: nil,
			want:      true,
			wantCalls: backOff.Steps,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Keep the retried case from sleeping out the production backoff.
			restore := backOff
			backOff = wait.Backoff{Steps: restore.Steps, Duration: time.Millisecond}
			defer func() { backOff = restore }()

			calls := 0
			client := &fakediscovery.FakeDiscovery{Fake: &k8stesting.Fake{Resources: tt.resources}}
			client.PrependReactor("*", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
				calls++
				return false, nil, nil
			})

			if got := discoverGVKWithClient(client, gvk); got != tt.want {
				t.Errorf("discoverGVKWithClient() = %v, want %v", got, tt.want)
			}
			if calls != tt.wantCalls {
				t.Errorf("discovery calls = %d, want %d", calls, tt.wantCalls)
			}
		})
	}
}
