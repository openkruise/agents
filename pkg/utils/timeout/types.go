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

import "time"

// Options is the time when Sandbox will be shut down or paused. Zero means never.
type Options struct {
	ShutdownTime time.Time
	PauseTime    time.Time
	// SetAnnotations is applied to metadata.annotations in the same retryUpdate round
	// that writes ShutdownTime / PauseTime. Empty-string values delete the key. Callers
	// pass it via Sandbox.SaveTimeoutWithPolicy; the modifier writes annotations when
	// they differ, independently of whether the timeout policy decides to write.
	// json:"-" because Options has no wire form today and SetAnnotations should never
	// leak into one if a wire form is added later.
	SetAnnotations map[string]string `json:"-"`
}

type UpdatePolicy string

const (
	UpdatePolicyAlways     UpdatePolicy = "Always"
	UpdatePolicyExtendOnly UpdatePolicy = "ExtendOnly"
)
