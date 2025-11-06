package proxy

// Route 表示一条内部沙箱路由规则
type Route struct {
	IP           string
	ID           string
	Owner        string
	ExtraHeaders map[string]string
}

func (s *Server) SetRoute(id string, route Route) {
	s.routes.Store(id, route)
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
