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
	"fmt"
	"strconv"
	"strings"
)

// wakeOnTrafficPrefix is the required key prefix of the AnnotationWakeOnTraffic value.
const wakeOnTrafficPrefix = "timeout:"

// wakeOnTrafficNever is the sentinel value meaning "wake enabled, sandbox never auto-times-out".
const wakeOnTrafficNever = wakeOnTrafficPrefix + "never"

// WakeOnTrafficConfig is the decoded form of the AnnotationWakeOnTraffic value.
// Never and TimeoutSeconds are mutually exclusive: when Never is true the sandbox
// carries no deadline and TimeoutSeconds is 0; otherwise TimeoutSeconds is a
// positive number of seconds.
type WakeOnTrafficConfig struct {
	Never          bool
	TimeoutSeconds int
}

// FormatWakeOnTraffic encodes the wake-on-traffic annotation value. It is the
// only writer of the annotation format; all producers MUST go through it.
func FormatWakeOnTraffic(never bool, timeoutSeconds int) string {
	if never {
		return wakeOnTrafficNever
	}
	return fmt.Sprintf("%s%d", wakeOnTrafficPrefix, timeoutSeconds)
}

// ParseWakeOnTraffic decodes the wake-on-traffic annotation value. The second
// return value reports whether wake is enabled. A malformed, empty, zero,
// negative, signed, fractional, or whitespace-padded value yields (zero, false).
func ParseWakeOnTraffic(value string) (WakeOnTrafficConfig, bool) {
	if value == wakeOnTrafficNever {
		return WakeOnTrafficConfig{Never: true}, true
	}
	if !strings.HasPrefix(value, wakeOnTrafficPrefix) {
		return WakeOnTrafficConfig{}, false
	}
	raw := strings.TrimPrefix(value, wakeOnTrafficPrefix)
	if raw == "" || strings.TrimSpace(raw) != raw {
		return WakeOnTrafficConfig{}, false
	}
	timeoutSeconds, err := strconv.Atoi(raw)
	if err != nil || timeoutSeconds <= 0 {
		return WakeOnTrafficConfig{}, false
	}
	return WakeOnTrafficConfig{TimeoutSeconds: timeoutSeconds}, true
}
