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

package sandbox_manager

import (
	"errors"
	"fmt"
	"strings"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

var (
	ErrAutoResumeDisabled      = errors.New("auto resume disabled")
	ErrInvalidAutoResumePolicy = errors.New("invalid auto resume policy")
)

const wakeTimeoutNeverValue = "timeout:never"

type Policy struct {
	Form     PolicyForm
	Duration time.Duration
}

type PolicyForm string

const (
	PolicyFormNever    PolicyForm = "never"
	PolicyFormDuration PolicyForm = "duration"
)

func ParseWakeOnTrafficPolicy(annotations map[string]string) (Policy, error) {
	value, ok := annotations[agentsv1alpha1.AnnotationWakeOnTraffic]
	if !ok {
		return Policy{}, ErrAutoResumeDisabled
	}
	if value == wakeTimeoutNeverValue {
		return Policy{Form: PolicyFormNever}, nil
	}

	kind, payload, ok := strings.Cut(value, ":")
	if !ok || kind != "timeout" {
		return Policy{}, ErrInvalidAutoResumePolicy
	}
	duration, err := time.ParseDuration(payload)
	if err != nil || duration < time.Second {
		return Policy{}, ErrInvalidAutoResumePolicy
	}
	return Policy{Form: PolicyFormDuration, Duration: duration}, nil
}

// FormatNeverWakeOnTrafficPolicy returns the annotation value for the
// no-shutdown-timeout wake-on-traffic policy. The output is parseable by
// ParseWakeOnTrafficPolicy.
func FormatNeverWakeOnTrafficPolicy() string {
	return wakeTimeoutNeverValue
}

// FormatTimeoutWakeOnTrafficPolicy returns the annotation value for a
// timeout-based wake-on-traffic policy. The output is parseable by
// ParseWakeOnTrafficPolicy.
func FormatTimeoutWakeOnTrafficPolicy(seconds int) string {
	return fmt.Sprintf("timeout:%ds", seconds)
}
