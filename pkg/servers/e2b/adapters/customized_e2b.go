package adapters

import (
	"fmt"
	"strconv"
	"strings"
)

type CustomizedE2BAdapter struct{}

const CustomPrefix = "/kruise"

// Map maps paths like /kruise/sandbox1234/3000/xxx to sandboxID=sandbox1234 and port=3000
func (a *CustomizedE2BAdapter) Map(_, _, path string, _ int, _ map[string]string) (
	sandboxID string, sandboxPort int, extraHeaders map[string]string, err error) {
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
