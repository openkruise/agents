package adapters

import (
	"fmt"
	"strings"
)

// E2BMapper is part of proxy.RequestAdapter
type E2BMapper interface {
	Map(scheme, authority, path string, port int, headers map[string]string) (
		sandboxID string, sandboxPort int, extraHeaders map[string]string, err error)
	IsSandboxRequest(authority, path string, port int) bool
}

var DefaultAdapterFactory = NewE2BAdapter

type E2BAdapter struct {
	Port       int
	native     *NativeE2BAdapter
	customized *CustomizedE2BAdapter
}

func NewE2BAdapter(port int) *E2BAdapter {
	return &E2BAdapter{
		Port:       port,
		native:     &NativeE2BAdapter{},
		customized: &CustomizedE2BAdapter{},
	}
}

func (a *E2BAdapter) Map(scheme, authority, path string, port int, headers map[string]string) (
	sandboxID string, sandboxPort int, extraHeaders map[string]string, err error) {
	return a.ChooseAdapter(path).Map(scheme, authority, path, port, headers)
}

func (a *E2BAdapter) IsSandboxRequest(authority, path string, port int) bool {
	return a.ChooseAdapter(path).IsSandboxRequest(authority, path, port)
}

func (a *E2BAdapter) Entry() string {
	return fmt.Sprintf("127.0.0.1:%d", a.Port)
}

func (a *E2BAdapter) ChooseAdapter(path string) E2BMapper {
	if strings.HasPrefix(path, CustomPrefix) {
		return a.customized
	}
	return a.native
}
