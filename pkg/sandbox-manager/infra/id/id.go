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

// Package id defines the lightweight, policy-neutral identifiers of Sandbox
// backend implementations. It depends on the standard library only so that
// implementations, including those linked by independent controllers, can
// reference the identifiers without pulling in sandbox-manager packages.
package id

import "regexp"

// ID identifies a Sandbox backend implementation. IDs are case-sensitive.
type ID string

// OpenKruise is the identifier of the Kubernetes CRD backend implemented by
// sandboxcr. Other identifiers belong to the registrations of their owners.
const OpenKruise ID = "openkruise"

// idPattern is the complete syntax accepted by Valid. The pattern is
// deliberately independent of any registry: satisfying it only means the
// string is a well-formed identifier, not that the implementation is
// configured or approved.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// Valid reports whether value is a syntactically valid Infra ID. It performs
// no registry lookup.
func Valid(value string) bool {
	return idPattern.MatchString(value)
}
