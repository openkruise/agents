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

// Package utils provides utility functions for sandbox lock TTL tracking.
//
//nolint:revive // Package name is acceptable for this utility package
package utils

import (
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/api/v1alpha1"
)

// IsLockExpired reports whether the lock timestamp annotation on the given object
// indicates that the lock has been held for longer than 5 minutes, meaning it can
// be considered stale and safely overridden.
func IsLockExpired(sbx client.Object) bool {
	annotations := sbx.GetAnnotations()
	if annotations == nil {
		return false
	}
	lockTimeStr := annotations[v1alpha1.AnnotationLockTimestamp]
	if lockTimeStr == "" {
		return false
	}
	lockTime, err := time.Parse(time.RFC3339, lockTimeStr)
	if err != nil {
		return false
	}
	return time.Since(lockTime) > 5*time.Minute
}
