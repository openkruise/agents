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
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseWakeOnTraffic(t *testing.T) {
	tests := []struct {
		name          string
		value         string
		expectConfig  WakeOnTrafficConfig
		expectEnabled bool
	}{
		{name: "never", value: "timeout:never", expectConfig: WakeOnTrafficConfig{Never: true}, expectEnabled: true},
		{name: "positive seconds", value: "timeout:300", expectConfig: WakeOnTrafficConfig{TimeoutSeconds: 300}, expectEnabled: true},
		{name: "empty", value: "", expectEnabled: false},
		{name: "wrong key", value: "ttl:300", expectEnabled: false},
		{name: "missing value", value: "timeout:", expectEnabled: false},
		{name: "zero", value: "timeout:0", expectEnabled: false},
		{name: "negative", value: "timeout:-1", expectEnabled: false},
		{name: "plus sign", value: "timeout:+1", expectEnabled: true, expectConfig: WakeOnTrafficConfig{TimeoutSeconds: 1}},
		{name: "float", value: "timeout:1.5", expectEnabled: false},
		{name: "space before", value: " timeout:300", expectEnabled: false},
		{name: "space after", value: "timeout:300 ", expectEnabled: false},
		{name: "uppercase never", value: "timeout:Never", expectEnabled: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotConfig, gotEnabled := ParseWakeOnTraffic(tt.value)
			assert.Equal(t, tt.expectEnabled, gotEnabled)
			assert.Equal(t, tt.expectConfig, gotConfig)
		})
	}
}

func TestFormatWakeOnTraffic(t *testing.T) {
	tests := []struct {
		name           string
		never          bool
		timeoutSeconds int
		expect         string
	}{
		{name: "never", never: true, timeoutSeconds: 0, expect: "timeout:never"},
		{name: "never ignores seconds", never: true, timeoutSeconds: 300, expect: "timeout:never"},
		{name: "finite", never: false, timeoutSeconds: 300, expect: "timeout:300"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expect, FormatWakeOnTraffic(tt.never, tt.timeoutSeconds))
		})
	}
}

func TestWakeOnTrafficRoundTrip(t *testing.T) {
	tests := []struct {
		name           string
		never          bool
		timeoutSeconds int
	}{
		{name: "never round-trip", never: true, timeoutSeconds: 0},
		{name: "finite round-trip", never: false, timeoutSeconds: 600},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded := FormatWakeOnTraffic(tt.never, tt.timeoutSeconds)
			cfg, enabled := ParseWakeOnTraffic(encoded)
			assert.True(t, enabled)
			assert.Equal(t, tt.never, cfg.Never)
			if !tt.never {
				assert.Equal(t, tt.timeoutSeconds, cfg.TimeoutSeconds)
			}
		})
	}
}
