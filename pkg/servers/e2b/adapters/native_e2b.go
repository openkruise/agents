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
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const (
	// DefaultSandboxIDHeader is the default header name for sandbox ID
	DefaultSandboxIDHeader = "e2b-sandbox-id"
	// DefaultSandboxPortHeader is the default header name for sandbox port
	DefaultSandboxPortHeader = "e2b-sandbox-port"
	// DefaultHostHeader is the default header name for host-based routing
	DefaultHostHeader = "Host"
)

// NativeE2BAdapter extracts sandbox ID and port from headers (primary) or hostname (fallback).
// When SandboxIDHeader/SandboxPortHeader/HostHeader are empty, defaults are used.
// DefaultPort is used when the port header is absent in header-based mode (0 = no default).
type NativeE2BAdapter struct {
	SandboxIDHeader   string
	SandboxPortHeader string
	HostHeader        string
	DefaultPort       int
}

var hostRegex = regexp.MustCompile(`^(\d+)-([a-zA-Z0-9\-]+)\.`)

func (a *NativeE2BAdapter) getSandboxIDHeader() string {
	if a.SandboxIDHeader != "" {
		return a.SandboxIDHeader
	}
	return DefaultSandboxIDHeader
}

func (a *NativeE2BAdapter) getSandboxPortHeader() string {
	if a.SandboxPortHeader != "" {
		return a.SandboxPortHeader
	}
	return DefaultSandboxPortHeader
}

func (a *NativeE2BAdapter) getHostHeader() string {
	if a.HostHeader != "" {
		return a.HostHeader
	}
	return DefaultHostHeader
}

// Map extracts sandbox ID and port using header-first, hostname-fallback strategy:
//  1. Check headers for sandbox ID header (e.g., e2b-sandbox-id). If present, use it along with
//     the port header (or DefaultPort when port header is absent).
//  2. If sandbox ID header is not found, fall back to hostname parsing from authority or a custom
//     host header. Parse the hostname format: {port}-{namespace}--{name}.{domain}.
//  3. If neither yields a result, return error.
func (a *NativeE2BAdapter) Map(req *ParsedRequest) (
	sandboxID string, sandboxPort int, extraHeaders map[string]string, err error) {
	authority := req.Authority
	headers := req.Headers

	// Step 1: Header-based extraction (primary)
	if headers != nil {
		if id, ok := headers[a.getSandboxIDHeader()]; ok && id != "" {
			sandboxID = id
			if portStr, ok := headers[a.getSandboxPortHeader()]; ok && portStr != "" {
				sandboxPort, err = strconv.Atoi(portStr)
				if err != nil {
					err = fmt.Errorf("invalid sandbox port header value: %s", portStr)
					return
				}
			} else if a.DefaultPort > 0 {
				sandboxPort = a.DefaultPort
			} else {
				err = fmt.Errorf("sandbox port header not found and no default port configured")
				return
			}
			return
		}
	}

	// Step 2: Hostname-based extraction (fallback)
	// If a custom host header is configured and present, use it as the authority for parsing
	resolvedAuthority := authority
	hostHeader := a.getHostHeader()
	if hostHeader != DefaultHostHeader && headers != nil {
		if customHost, ok := headers[hostHeader]; ok && customHost != "" {
			resolvedAuthority = customHost
		}
	}

	matches := hostRegex.FindStringSubmatch(resolvedAuthority)
	if len(matches) == 3 {
		sandboxPort, err = strconv.Atoi(matches[1])
		if err != nil {
			return // impossible
		}
		sandboxID = matches[2]
		return
	}

	err = fmt.Errorf("cannot extract sandbox info from authority %q or headers", authority)
	return
}

func (a *NativeE2BAdapter) IsSandboxRequest(authority, _ string, _ int) bool {
	return !strings.HasPrefix(authority, "api.")
}
