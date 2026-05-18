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

package proxy

type WakeAction string

const (
	WakeActionResumed                 WakeAction = "Resumed"
	WakeActionAlreadyRunning          WakeAction = "AlreadyRunning"
	WakeActionAutoResumeDisabled      WakeAction = "AutoResumeDisabled"
	WakeActionInvalidAutoResumePolicy WakeAction = "InvalidAutoResumePolicy"
	WakeActionNotFound                WakeAction = "NotFound"
	WakeActionPausing                 WakeAction = "Pausing"
	WakeActionBadState                WakeAction = "BadState"
	WakeActionGone                    WakeAction = "Gone"
)

type WakeResult struct {
	Action WakeAction `json:"action"`
	State  string     `json:"state"`
	// ResourceVersion is the manager's best-effort observed Sandbox version.
	// Gateways do not depend on it; wake convergence is based on registry polling.
	ResourceVersion string `json:"resourceVersion"`
	Message         string `json:"message"`
}
