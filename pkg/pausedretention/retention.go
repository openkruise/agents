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

package pausedretention

import (
	"fmt"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils/timeout"
)

func ParseReservePausedSandboxFor(raw string) (time.Duration, error) {
	if raw == timeout.ReservePausedSandboxForDefaultValue {
		return timeout.DefaultReservePausedSandboxFor, nil
	}
	retention, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid reserve paused sandbox duration %q: %w; use %q for the built-in 100-year retention", raw, err, timeout.ReservePausedSandboxForDefaultValue)
	}
	if retention <= 0 {
		return 0, fmt.Errorf("reserve paused sandbox duration %q must be positive; use %q for the built-in 100-year retention", raw, timeout.ReservePausedSandboxForDefaultValue)
	}
	return retention, nil
}

func ResolveReservePausedSandboxForAnnotation(annotations map[string]string) (time.Duration, bool, error) {
	if annotations == nil {
		return 0, false, nil
	}
	raw, ok := annotations[agentsv1alpha1.AnnotationReservePausedSandboxFor]
	if !ok {
		return 0, false, nil
	}
	retention, err := ParseReservePausedSandboxFor(raw)
	if err != nil {
		return 0, true, err
	}
	return retention, true, nil
}

func PausedShutdownTime(anchor time.Time, pausedRetention time.Duration) time.Time {
	return timeout.NormalizeTime(anchor.Add(pausedRetention))
}
