package proxyutils

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

//goland:noinspection DuplicatedCode
func TestSandbox_ProxyRequest(t *testing.T) {
	// 创建一个测试HTTP服务器
	server := http.NewServeMux()
	server.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent) // 返回204状态码
	})

	// 添加一个处理特定路径的处理器，用于测试路径转发
	server.HandleFunc("/test-path", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("test response"))
	})

	// 添加一个返回错误状态码的处理器
	server.HandleFunc("/error", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal server error"))
	})

	// 启动HTTP服务器监听本地端口
	listener, err := net.Listen("tcp", "127.0.0.1:11111")
	require.NoError(t, err)

	httpServer := &http.Server{Handler: server}
	go func() {
		_ = httpServer.Serve(listener)
	}()

	// 确保服务器已启动
	time.Sleep(100 * time.Millisecond)

	defer func() {
		_ = httpServer.Shutdown(context.Background())
	}()

	tests := []struct {
		name            string
		path            string
		ip              string
		wantErr         bool
		wantStatus      int
		wantErrContains string
	}{
		{
			name:       "valid pod IP",
			path:       "/",
			ip:         "127.0.0.1",
			wantErr:    false,
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "specific path",
			path:       "/test-path",
			ip:         "127.0.0.1",
			wantErr:    false,
			wantStatus: http.StatusOK,
		},
		{
			name:            "error response",
			path:            "/error",
			ip:              "127.0.0.1",
			wantErr:         true, // 应该返回错误，因为状态码是5xx
			wantErrContains: "internal server error",
		},
		{
			name:            "unreachable server",
			path:            "/",
			ip:              "192.168.100.100", // 使用一个不可达的IP地址
			wantErr:         true,              // 当服务器不可达时应该返回错误
			wantErrContains: "failed to proxy request to sandbox",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 创建一个测试请求
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:11111"+tt.path, nil)
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")

			// 调用ProxyRequest方法
			resp, err := ProxyRequest(req, tt.path, 11111, tt.ip)

			// 检查错误
			if tt.wantErr {
				assert.Error(t, err)
				if tt.wantErrContains != "" {
					assert.True(t, strings.Contains(err.Error(), tt.wantErrContains),
						"Expected error to contain '%s', but got '%s'", tt.wantErrContains, err.Error())
				}
				return
			}

			require.NoError(t, err)
			assert.NotNil(t, resp)
			assert.Equal(t, tt.wantStatus, resp.StatusCode)

			// 关闭响应体
			if resp.Body != nil {
				_ = resp.Body.Close()
			}
		})
	}
}
