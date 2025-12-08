package adapters

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

var (
	UserNoNeedToAuth = "<port-no-need-to-auth>"
	UserUnknown      = "<unknown>"
)

type CommonAdapter struct {
	Port int
	Keys *keys.SecretKeyStorage // If nil, authentication is disabled
}

var hostRegex = regexp.MustCompile(`^(\d+)-([a-zA-Z0-9\-]+)\.`)

func (a *CommonAdapter) Map(_, authority, _ string, _ int, headers map[string]string) (
	sandboxID string, sandboxPort int, extraHeaders map[string]string, user string, err error) {
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

	if a.Keys == nil {
		return
	}

	// Parse user
	if sandboxPort == models.CDPPort || sandboxPort == models.VNCPort {
		// no auth for CDP
		user = UserNoNeedToAuth
		return
	}

	token := headers["x-access-token"] // from sandbox.EnvdAccessToken
	key, ok := a.Keys.LoadByKey(token)
	if ok {
		user = key.ID.String()
	} else {
		user = UserUnknown
	}
	return
}

func (a *CommonAdapter) Authorize(user, owner string) bool {
	if a.Keys == nil {
		return true
	}
	return user == UserNoNeedToAuth || user == owner
}

func (a *CommonAdapter) IsSandboxRequest(authority, _ string, _ int) bool {
	return !strings.HasPrefix(authority, "api.")
}

func (a *CommonAdapter) Entry() string {
	return fmt.Sprintf("127.0.0.1:%d", a.Port)
}
