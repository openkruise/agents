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

package tracing

import (
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// extractPodInfo extracts UID and creation time from a Pod or Sandbox object.
// This is the generic implementation used by the community OTEL emitter.
// Internal builds override this with extractPodInfoWithInstanceID for additional fields.
func extractPodInfo(obj interface{}) (podUID string, creationTime time.Time) {
	switch o := obj.(type) {
	case *corev1.Pod:
		if o != nil {
			podUID = string(o.UID)
			creationTime = o.CreationTimestamp.Time
		}
	case *agentsv1alpha1.Sandbox:
		if o != nil {
			podUID = string(o.UID)
			creationTime = o.CreationTimestamp.Time
		}
	}
	return
}
