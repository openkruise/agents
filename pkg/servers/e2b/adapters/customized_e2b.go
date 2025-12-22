package adapters

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

type CustomizedE2BAdapter struct {
	Keys *keys.SecretKeyStorage
}

const CustomPrefix = "/kruise"

// Map maps paths like /kruise/sandbox1234/3000/xxx to sandboxID=sandbox1234 and port=3000
func (a *CustomizedE2BAdapter) Map(_, _, path string, _ int, headers map[string]string) (
	sandboxID string, sandboxPort int, extraHeaders map[string]string, user string, err error) {
	if len(path) < len(CustomPrefix)+1 {
		err = fmt.Errorf("invalid path: %s", path)
		return
	}
	path = path[len(CustomPrefix)+1:] // remove prefix "/kruise/"
	split := strings.SplitN(path, "/", 3)
	if len(split) < 3 {
		err = fmt.Errorf("invalid path: %s", path)
		return
	}
	sandboxID = split[0]
	sandboxPort, err = strconv.Atoi(split[1])
	if err != nil {
		return
	}
	extraHeaders = map[string]string{
		":path": "/" + split[2],
	}
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

func (a *CustomizedE2BAdapter) IsSandboxRequest(_, path string, _ int) bool {
	return !strings.HasPrefix(path, CustomPrefix+"/api")
}
