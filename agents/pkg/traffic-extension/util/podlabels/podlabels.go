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

// Package podlabels provides utilities for decoding and parsing pod labels
// extracted from Envoy filter_state (sandbox.labels).
package podlabels

import (
	"encoding/base64"
	"strings"
)

// ParseSandboxLabels decodes a base64-encoded sandbox.labels string and returns
// the resulting label map.
//
// The encoded string follows the Kubernetes label format: "key1=value1,key2=value2"
// For example: "app=sleep,networking.istio.io/tunnel=http,service.istio.io/canonical-name=sleep"
//
// If the input is empty or invalid, an empty map is returned.
func ParseSandboxLabels(encoded string) map[string]string {
	result := make(map[string]string)

	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return result
	}

	raw := string(decoded)
	if raw == "" {
		return result
	}

	// Split by comma to get individual label pairs.
	pairs := strings.Split(raw, ",")
	for _, pair := range pairs {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}

		// Split by "=" to separate key and value.
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) == 2 {
			result[parts[0]] = parts[1]
		}
	}

	return result
}
