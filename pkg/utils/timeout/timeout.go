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

package timeout

import (
	"encoding/json"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

var jsonMarshalTimeoutOptions = json.Marshal

func GetTimeoutFromSandbox(sbx *agentsv1alpha1.Sandbox) infra.TimeoutOptions {
	opts := infra.TimeoutOptions{}
	if sbx.Spec.ShutdownTime != nil {
		opts.ShutdownTime = NormalizeTime(sbx.Spec.ShutdownTime.Time)
	}
	if sbx.Spec.PauseTime != nil {
		opts.PauseTime = NormalizeTime(sbx.Spec.PauseTime.Time)
	}
	return opts
}

// SetTimeoutSnapshot stores the current timeout status into the snapshot annotation.
func SetTimeoutSnapshot(sbx *agentsv1alpha1.Sandbox) error {
	opts := GetTimeoutFromSandbox(sbx)
	if opts.ShutdownTime.IsZero() && opts.PauseTime.IsZero() {
		ClearPauseTimeoutSnapshot(sbx)
		return nil
	}

	by, err := jsonMarshalTimeoutOptions(opts)
	if err != nil {
		return err
	}
	if sbx.Annotations == nil {
		sbx.Annotations = map[string]string{}
	}
	sbx.Annotations[agentsv1alpha1.AnnotationPauseTimeoutSnapshot] = string(by)
	return nil
}

// IsTimeoutMatchedSnapshot reports whether the current timeout status matches the snapshot annotation.
func IsTimeoutMatchedSnapshot(sbx *agentsv1alpha1.Sandbox) (bool, error) {
	snapshot, exists, err := GetTimeoutSnapshot(sbx)
	if err != nil || !exists {
		return false, err
	}
	return Equal(GetTimeoutFromSandbox(sbx), snapshot), nil
}

// ClearPauseTimeoutSnapshot removes the temporary pause-cycle timeout marker.
func ClearPauseTimeoutSnapshot(sbx *agentsv1alpha1.Sandbox) {
	delete(sbx.Annotations, agentsv1alpha1.AnnotationPauseTimeoutSnapshot)
}

// GetTimeoutSnapshot parses the pause timeout snapshot annotation.
func GetTimeoutSnapshot(sbx *agentsv1alpha1.Sandbox) (infra.TimeoutOptions, bool, error) {
	if sbx.Annotations == nil {
		return infra.TimeoutOptions{}, false, nil
	}

	raw := sbx.Annotations[agentsv1alpha1.AnnotationPauseTimeoutSnapshot]
	if raw == "" {
		return infra.TimeoutOptions{}, false, nil
	}

	var snapshot infra.TimeoutOptions
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		return infra.TimeoutOptions{}, false, err
	}
	// NormalizeTime maybe unnecessary, but it's safe to keep it
	snapshot.ShutdownTime = NormalizeTime(snapshot.ShutdownTime)
	snapshot.PauseTime = NormalizeTime(snapshot.PauseTime)
	return snapshot, true, nil
}

// Equal compares timeout options after normalizing time precision.
func Equal(a, b infra.TimeoutOptions) bool {
	return timeEqual(a.ShutdownTime, b.ShutdownTime) && timeEqual(a.PauseTime, b.PauseTime)
}

// ShouldExtendTimeout reports whether requested extends the effective end time.
func ShouldExtendTimeout(current, requested infra.TimeoutOptions) bool {
	currentEndAt := timeoutEndAt(current)
	requestedEndAt := timeoutEndAt(requested)
	if currentEndAt.IsZero() || requestedEndAt.IsZero() {
		return false
	}
	return requestedEndAt.After(currentEndAt)
}

// NormalizeTime converts timeout values to the precision Kubernetes persists and
// E2B exposes: wall-clock time at whole-second precision. This removes Go's
// monotonic clock reading and drops sub-second differences so timeout comparison,
// snapshot matching, and retry conflict handling stay stable across in-memory
// values, metav1.Time serialization, API server round trips, and annotation JSON.
func NormalizeTime(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return t.Round(0).Truncate(time.Second)
}

func timeoutEndAt(opts infra.TimeoutOptions) time.Time {
	if !opts.PauseTime.IsZero() {
		return opts.PauseTime
	}
	return opts.ShutdownTime
}

func timeEqual(a, b time.Time) bool {
	if a.IsZero() && b.IsZero() {
		return true
	}
	return NormalizeTime(a).Equal(NormalizeTime(b))
}
