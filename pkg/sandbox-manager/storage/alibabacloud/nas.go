package alibabacloud

import (
	"fmt"
	"strings"
)

const (
	NasTypeGeneral = "generalnas"
	NasTypeFast    = "fastnas"
)

type NAS struct {
	Type     string
	Path     string
	Endpoint string
}

func (m NAS) ToMountArgs() []string {
	return []string{
		"--nasType", m.Type,
		"--nasPath", m.Path,
		"--endpoint", m.Endpoint,
	}
}

const (
	NASParamType     = "type"
	NASParamPath     = "path"
	NASParamEndpoint = "endpoint"
)

func NewMountVendorNAS(params map[string]string) (NAS, error) {
	nasType, endpoint, path := params[NASParamType], params[NASParamEndpoint], params[NASParamPath]
	if nasType != NasTypeGeneral && nasType != NasTypeFast {
		return NAS{}, fmt.Errorf("invalid nasType: %s", nasType)
	}
	if path == "" {
		return NAS{}, fmt.Errorf("path is required")
	}
	if !validateNasEndpoint(endpoint) {
		return NAS{}, fmt.Errorf("invalid endpoint: %s", endpoint)
	}
	return NAS{
		Type:     nasType,
		Path:     path,
		Endpoint: endpoint,
	}, nil
}

func validateNasEndpoint(endpoint string) bool {
	return strings.HasSuffix(endpoint, ".nas.aliyuncs.com")
}
