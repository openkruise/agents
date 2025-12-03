package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	utils "github.com/openkruise/agents/pkg/utils/sandbox-manager/proxyutils"
)

// Route 表示一条内部沙箱路由规则
type Route struct {
	IP           string            `json:"ip"`
	ID           string            `json:"id"`
	Owner        string            `json:"owner"`
	State        string            `json:"state"`
	ExtraHeaders map[string]string `json:"extra_headers"`
}

func (s *Server) SetRoute(route Route) {
	s.routes.Store(route.ID, route)
}

func (s *Server) SyncRouteWithPeers(route Route) error {
	body, err := json.Marshal(route)
	if err != nil {
		return err
	}
	var errStrings []string
	for _, ip := range s.Peers {
		request, err := http.NewRequest(http.MethodPost, RefreshAPI, bytes.NewBuffer(body))
		if err != nil {
			return err
		}
		if _, err = utils.ProxyRequest(request, RefreshAPI, SystemPort, ip); err != nil {
			errStrings = append(errStrings, err.Error())
		}
	}
	if len(errStrings) == 0 {
		return nil
	}
	return errors.New(strings.Join(errStrings, ";"))
}

func (s *Server) LoadRoute(id string) (Route, bool) {
	raw, ok := s.routes.Load(id)
	if !ok {
		return Route{}, false
	}
	return raw.(Route), true
}

func (s *Server) ListRoutes() []Route {
	routes := make([]Route, 0)
	s.routes.Range(func(key, value any) bool {
		routes = append(routes, value.(Route))
		return true
	})
	return routes
}

func (s *Server) DeleteRoute(id string) {
	s.routes.Delete(id)
}

// RequestAdapter 用于注册来自业务侧的沙箱请求到内部逻辑的映射
type RequestAdapter interface {
	// Map 从请求中提取沙箱 id 和端口等信息
	Map(scheme, authority, path string, port int, headers map[string]string) (
		sandboxID string, sandboxPort int, extraHeaders map[string]string, user string, err error)
	// Authorize 判断用户是否有权限访问该沙箱
	Authorize(user, owner string) bool
	// IsSandboxRequest 判断该请求是否为沙箱请求，如果返回 true，代表是是请求沙箱的，否则是请求 API Server 的。只有沙箱请求会经过 Adapter 处理。
	IsSandboxRequest(authority, path string, port int) bool
	// Entry 获取服务进程的入口地址，比如 "127.0.0.1:8080"
	Entry() string
}
