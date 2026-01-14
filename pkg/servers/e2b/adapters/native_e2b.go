package adapters

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type NativeE2BAdapter struct{}

var hostRegex = regexp.MustCompile(`^(\d+)-([a-zA-Z0-9\-]+)\.`)

// Map maps authorities like 3000-sandbox1234.example.com to sandboxID=sandbox1234 and port=3000
func (a *NativeE2BAdapter) Map(_, authority, _ string, _ int, _ map[string]string) (
	sandboxID string, sandboxPort int, extraHeaders map[string]string, err error) {
	matches := hostRegex.FindStringSubmatch(authority)
	if len(matches) != 3 {
		err = fmt.Errorf("invalid authority format: %s", authority)
		return
	}

	// Extract port number and sandboxID
	sandboxPort, err = strconv.Atoi(matches[1])
	if err != nil {
		return // impossible
	}
	sandboxID = matches[2]
	return
}

func (a *NativeE2BAdapter) IsSandboxRequest(authority, _ string, _ int) bool {
	return !strings.HasPrefix(authority, "api.")
}
