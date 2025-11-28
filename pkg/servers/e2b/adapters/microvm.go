package adapters

import (
	"fmt"
	"strconv"

	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

type MicroVMAdapter struct {
	*CommonAdapter
}

const MicroVMPort = 5007

func (a *MicroVMAdapter) Map(_, authority, _ string, _ int, headers map[string]string) (
	sandboxID string, sandboxPort int, extraHeaders map[string]string, user string, err error) {
	matches := hostRegex.FindStringSubmatch(authority)
	if len(matches) != 3 {
		err = fmt.Errorf("invalid authority format: %s", authority)
		return
	}

	sandboxID = matches[2]
	sandboxPort = MicroVMPort

	var targetPort int
	// 提取端口号和sandboxID
	targetPort, err = strconv.Atoi(matches[1])
	if err != nil {
		return
	}

	extraHeaders = map[string]string{
		"X-MICROSANDBOX-PORT": matches[1],
		"X-Access-Token":      "",
	}

	// 解析用户
	if a.Keys == nil {
		return
	}
	if targetPort == models.CDPPort || targetPort == models.VNCPort {
		// no auth for CDP
		user = UserNoNeedToAuth
	} else {
		token := headers["x-access-token"] // from sandbox.EnvdAccessToken
		key, ok := a.Keys.LoadByKey(token)
		if ok {
			user = key.ID.String()
		} else {
			user = UserUnknown
		}
	}
	return
}
