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

import "strings"

// PatchMetadataRequest is a JSON Merge Patch (RFC 7396) body: present keys
// with a string value are set, present keys mapped to JSON null are deleted,
// and absent keys are left untouched. A nil *string marshals to JSON null.
type PatchMetadataRequest map[string]*string

// HasReservedKey reports whether the patch touches a key under
// ReservedMetadataPrefix, which the spec requires the server to reject.
func (p PatchMetadataRequest) HasReservedKey() (key string, found bool) {
	for k := range p {
		if strings.HasPrefix(k, ReservedMetadataPrefix) {
			return k, true
		}
	}
	return "", false
}
