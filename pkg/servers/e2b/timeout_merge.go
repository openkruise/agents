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
	"time"

	timeoututils "github.com/openkruise/agents/pkg/utils/timeout"
)

func laterFinite(current, requested time.Time) time.Time {
	current = timeoututils.NormalizeTime(current)
	requested = timeoututils.NormalizeTime(requested)
	if current.IsZero() || requested.IsZero() || !requested.After(current) {
		return current
	}
	return requested
}

func mergeConnectTimeout(current, requested timeoututils.Options) timeoututils.Options {
	return timeoututils.Options{
		PauseTime:    laterFinite(current.PauseTime, requested.PauseTime),
		ShutdownTime: laterFinite(current.ShutdownTime, requested.ShutdownTime),
	}
}
