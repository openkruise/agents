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

package sandboxcr

import (
	"fmt"
	"testing"

	"github.com/openkruise/agents/api/v1alpha1"
)

// BenchmarkAsSandboxes measures wrapping the cached Sandboxes of one list
// request, which Infra.SelectSandboxes does for every Sandbox of the user
// before filtering and pagination.
func BenchmarkAsSandboxes(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		objs := make([]*v1alpha1.Sandbox, n)
		for i := range objs {
			objs[i] = createTestSandbox(fmt.Sprintf("sandbox-%d", i), "user", v1alpha1.SandboxRunning, true)
		}
		i := &Infra{}
		b.Run(fmt.Sprintf("sandboxes=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if got := i.asSandboxes(objs); len(got) != n {
					b.Fatalf("expected %d sandboxes, got %d", n, len(got))
				}
			}
		})
	}
}
