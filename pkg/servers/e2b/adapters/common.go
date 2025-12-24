package adapters

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type CommonAdapter struct {
	Port int
}

var hostRegex = regexp.MustCompile(`^(\d+)-([a-zA-Z0-9\-]+)\.`)

func (a *CommonAdapter) Map(_, authority, _ string, _ int, _ map[string]string) (
	sandboxID string, sandboxPort int, extraHeaders map[string]string, err error) {
	matches := hostRegex.FindStringSubmatch(authority)
	if len(matches) != 3 {
		err = fmt.Errorf("invalid authority format: %s", authority)
		return
	}

	// Extract port number and sandboxID
	sandboxPort, err = strconv.Atoi(matches[1])
	if err != nil {
		return
	}
	sandboxID = matches[2]

	return sandboxID, sandboxPort, extraHeaders, err
}

func (a *CommonAdapter) IsSandboxRequest(authority, _ string, _ int) bool {
	return !strings.HasPrefix(authority, "api.")
}

func (a *CommonAdapter) Entry() string {
	return fmt.Sprintf("127.0.0.1:%d", a.Port)
}
