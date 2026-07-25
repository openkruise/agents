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

package e2b

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	timeoututils "github.com/openkruise/agents/pkg/utils/timeout"
)

func TestMergeConnectTimeout(t *testing.T) {
	base := time.Date(2026, 7, 7, 10, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	currentPause := base.Add(10 * time.Minute)
	currentShutdown := base.Add(2 * time.Hour)
	laterPause := base.Add(20 * time.Minute)
	laterShutdown := base.Add(3 * time.Hour)
	earlierPause := base.Add(5 * time.Minute)
	earlierShutdown := base.Add(time.Hour)

	tests := []struct {
		name      string
		current   timeoututils.Options
		requested timeoututils.Options
		want      timeoututils.Options
	}{
		{
			name: "updates only shutdown when requested pause is earlier",
			current: timeoututils.Options{
				PauseTime:    currentPause,
				ShutdownTime: currentShutdown,
			},
			requested: timeoututils.Options{
				PauseTime:    earlierPause,
				ShutdownTime: laterShutdown,
			},
			want: timeoututils.Options{
				PauseTime:    timeoututils.NormalizeTime(currentPause),
				ShutdownTime: timeoututils.NormalizeTime(laterShutdown),
			},
		},
		{
			name: "updates only pause when requested shutdown is earlier",
			current: timeoututils.Options{
				PauseTime:    currentPause,
				ShutdownTime: currentShutdown,
			},
			requested: timeoututils.Options{
				PauseTime:    laterPause,
				ShutdownTime: earlierShutdown,
			},
			want: timeoututils.Options{
				PauseTime:    timeoututils.NormalizeTime(laterPause),
				ShutdownTime: timeoututils.NormalizeTime(currentShutdown),
			},
		},
		{
			name: "skips both earlier fields",
			current: timeoututils.Options{
				PauseTime:    currentPause,
				ShutdownTime: currentShutdown,
			},
			requested: timeoututils.Options{
				PauseTime:    earlierPause,
				ShutdownTime: earlierShutdown,
			},
			want: timeoututils.Options{
				PauseTime:    timeoututils.NormalizeTime(currentPause),
				ShutdownTime: timeoututils.NormalizeTime(currentShutdown),
			},
		},
		{
			name: "requested zero does not clear finite current fields",
			current: timeoututils.Options{
				PauseTime:    currentPause,
				ShutdownTime: currentShutdown,
			},
			requested: timeoututils.Options{},
			want: timeoututils.Options{
				PauseTime:    timeoututils.NormalizeTime(currentPause),
				ShutdownTime: timeoututils.NormalizeTime(currentShutdown),
			},
		},
		{
			name: "current zero is not converted to finite",
			current: timeoututils.Options{
				ShutdownTime: currentShutdown,
			},
			requested: timeoututils.Options{
				PauseTime:    laterPause,
				ShutdownTime: laterShutdown,
			},
			want: timeoututils.Options{
				ShutdownTime: timeoututils.NormalizeTime(laterShutdown),
			},
		},
		{
			name: "same second subsecond differences normalize to no update",
			current: timeoututils.Options{
				PauseTime:    currentPause.Add(100 * time.Millisecond),
				ShutdownTime: currentShutdown.Add(100 * time.Millisecond),
			},
			requested: timeoututils.Options{
				PauseTime:    currentPause.Add(900 * time.Millisecond),
				ShutdownTime: currentShutdown.Add(900 * time.Millisecond),
			},
			want: timeoututils.Options{
				PauseTime:    timeoututils.NormalizeTime(currentPause),
				ShutdownTime: timeoututils.NormalizeTime(currentShutdown),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeConnectTimeout(tt.current, tt.requested)
			assert.True(t, timeoututils.Equal(tt.want, got), "want %v, got %v", tt.want, got)
		})
	}
}
