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

// Package sandboxstate is the single source of the neutral lifecycle state
// vocabulary. The controller, manager, proxy and gateway closures consume these
// literals directly, so the values cannot drift between them; the CRD API
// package no longer redeclares them.
package sandboxstate

// The neutral state vocabulary fixes only the literal values; interpreting them
// and producing diagnostic reasons stay with the backend.
const (
	// Creating is an instance still being provisioned from scratch.
	Creating = "creating"
	// Available is a pool member that is ready and not yet claimed.
	Available = "available"
	// Provisioning is a claimed instance that is not ready to serve yet.
	Provisioning = "provisioning"
	// Running is a claimed instance that is ready and serving.
	Running = "running"
	// Paused is a claimed instance that is paused or resuming.
	Paused = "paused"
	// Expired is a settled instance that passed its shutdown deadline while
	// the backend still holds it.
	Expired = "expired"
	// Dead is a terminal observation (deleted, succeeded, failed or
	// terminating) that does not serve.
	Dead = "dead"
)
