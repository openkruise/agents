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

package adapters

import (
	"errors"
	"strings"
)

var errEmptySandboxDomain = errors.New("cannot resolve sandbox domain: empty host")

// GetDomain resolves the user-facing sandbox domain for an API request.
func (a *E2BAdapter) GetDomain(authority, path string) (string, error) {
	return a.ChooseAdapter(path).GetDomain(authority)
}

// GetSandboxAddress formats a resolved domain for the request shape selected by path.
func (a *E2BAdapter) GetSandboxAddress(domain, path, sandboxID string, port int32) string {
	return a.ChooseAdapter(path).GetSandboxAddress(domain, sandboxID, port)
}

// finishDomain strips one trailing dot from host, rejects empty hosts, and
// rejoins host[:port].
func finishDomain(host, port string) (string, error) {
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return "", errEmptySandboxDomain
	}
	if port == "" {
		return host, nil
	}
	return host + ":" + port, nil
}

// splitDomainHostPort splits a "host[:port]" authority without requiring a port.
// It preserves bracketed IPv6 hosts and treats raw IPv6 as a host without an
// explicit port.
func splitDomainHostPort(authority string) (host, port string) {
	if strings.HasPrefix(authority, "[") {
		end := strings.Index(authority, "]")
		if end < 0 {
			return authority, ""
		}
		host = authority[:end+1]
		if len(authority) > end+1 && authority[end+1] == ':' {
			return host, authority[end+2:]
		}
		return authority, ""
	}
	if strings.Count(authority, ":") > 1 {
		return authority, ""
	}
	idx := strings.LastIndex(authority, ":")
	if idx < 0 {
		return authority, ""
	}
	return authority[:idx], authority[idx+1:]
}
