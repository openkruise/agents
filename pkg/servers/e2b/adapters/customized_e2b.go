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
	"net/netip"
	"strconv"
	"strings"
)

type CustomizedE2BAdapter struct{}

const CustomPrefix = "/kruise"

func isCustomizedPath(path string) bool {
	return strings.HasPrefix(path, CustomPrefix)
}

// Map maps paths like /kruise/sandbox1234/3000/xxx to sandboxID=sandbox1234 and port=3000
func (a *CustomizedE2BAdapter) Map(req *ParsedRequest) (
	sandboxID string, sandboxPort int, extraHeaders map[string]string, err error) {
	path := req.Path
	if len(path) <= len(CustomPrefix)+1 {
		err = fmt.Errorf("invalid path: %s", path)
		return
	}
	path = path[len(CustomPrefix)+1:] // remove prefix "/kruise/"
	split := strings.SplitN(path, "/", 3)
	if len(split) < 2 {
		err = fmt.Errorf("invalid path: %s", path)
		return
	}
	sandboxID = split[0]
	sandboxPort, err = strconv.Atoi(split[1])
	if err != nil {
		return
	}
	realPath := "/"
	if len(split) > 2 {
		realPath += split[2]
	}
	extraHeaders = map[string]string{
		":path": realPath,
	}
	return
}

func (a *CustomizedE2BAdapter) IsSandboxRequest(_, path string, _ int) bool {
	return !strings.HasPrefix(path, CustomPrefix+"/api")
}

// GetDomain resolves a customized E2B domain from the request authority.
func (a *CustomizedE2BAdapter) GetDomain(authority string) (string, error) {
	host, port := splitDomainHostPort(authority)
	if addr, err := netip.ParseAddr(host); err == nil && addr.Is6() {
		host = "[" + addr.String() + "]"
	}
	return finishDomain(host, port)
}

// GetSandboxAddress returns the customized path address for a sandbox port.
func (a *CustomizedE2BAdapter) GetSandboxAddress(domain, sandboxID string, port int32) string {
	return fmt.Sprintf("%s%s/%s/%d", domain, CustomPrefix, sandboxID, port)
}
