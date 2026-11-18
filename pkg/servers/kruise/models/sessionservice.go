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

package models

// SessionServiceSession describes one durable Session binding.
type SessionServiceSession struct {
	SessionID        string `json:"sessionId"`
	SessionServiceID string `json:"sessionServiceID"`
	SandboxID        string `json:"sandboxID"`
	LastRequestAt    string `json:"lastRequestAt"`

	SessionServiceNamespace string `json:"sessionServiceNamespace"`
	SessionServiceName      string `json:"sessionServiceName"`
}

// ClosedSessionServiceSession acknowledges deletion of the durable binding.
type ClosedSessionServiceSession struct {
	SessionID        string `json:"sessionId"`
	SessionServiceID string `json:"sessionServiceID"`

	SessionServiceNamespace string `json:"sessionServiceNamespace"`
	SessionServiceName      string `json:"sessionServiceName"`
}

// TrafficAccessToken is a refreshed SessionService traffic credential.
type TrafficAccessToken struct {
	TrafficAccessToken           string `json:"trafficAccessToken"`
	TrafficAccessTokenExpiration string `json:"trafficAccessTokenExpiration"`
}
