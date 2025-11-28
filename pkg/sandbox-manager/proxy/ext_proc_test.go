package proxy

import (
	"context"
	"io"
	"sort"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	types "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// testRequestAdapter 是一个用于测试的 RequestAdapter 实现
type testRequestAdapter struct {
	entry            string
	isSandboxRequest bool
	mapResult        mapResult
	authorizeResult  bool
}

type mapResult struct {
	sandboxID    string
	sandboxPort  int
	extraHeaders map[string]string
	user         string
	err          error
}

func (t *testRequestAdapter) Map(string, string, string, int, map[string]string) (string, int, map[string]string, string, error) {
	return t.mapResult.sandboxID, t.mapResult.sandboxPort, t.mapResult.extraHeaders, t.mapResult.user, t.mapResult.err
}

func (t *testRequestAdapter) Authorize(string, string) bool {
	return t.authorizeResult
}

func (t *testRequestAdapter) IsSandboxRequest(string, string, int) bool {
	return t.isSandboxRequest
}

func (t *testRequestAdapter) Entry() string {
	return t.entry
}

// mockProcessServer 是 ExternalProcessor_ProcessServer 接口的模拟实现
type mockProcessServer struct {
	extProcPb.ExternalProcessor_ProcessServer
	ctx  context.Context
	reqs []*extProcPb.ProcessingRequest
	resp []*extProcPb.ProcessingResponse
	err  error
}

func (m *mockProcessServer) Context() context.Context {
	if m.ctx != nil {
		return m.ctx
	}
	return context.Background()
}

func (m *mockProcessServer) Send(resp *extProcPb.ProcessingResponse) error {
	if m.err != nil {
		return m.err
	}
	m.resp = append(m.resp, resp)
	return nil
}

func (m *mockProcessServer) Recv() (*extProcPb.ProcessingRequest, error) {
	if m.err != nil {
		return nil, m.err
	}

	if len(m.reqs) == 0 {
		return nil, io.EOF
	}

	req := m.reqs[0]
	m.reqs = m.reqs[1:]
	return req, nil
}

func TestServer_Process(t *testing.T) {
	tests := []struct {
		name        string
		setupRoutes []Route
		adapter     *testRequestAdapter
		requests    []*extProcPb.ProcessingRequest
		serverError error
		expectError bool
		expectResp  []*extProcPb.ProcessingResponse
	}{
		{
			name: "正常处理请求头",
			setupRoutes: []Route{
				{ID: "sandbox1", IP: "192.168.1.10", Owner: "user1"},
			},
			adapter: &testRequestAdapter{
				isSandboxRequest: true,
				mapResult: mapResult{
					sandboxID:   "sandbox1",
					sandboxPort: 8080,
					user:        "user1",
					err:         nil,
				},
				authorizeResult: true,
				entry:           "127.0.0.1:8080",
			},
			requests: []*extProcPb.ProcessingRequest{
				{
					Request: &extProcPb.ProcessingRequest_RequestHeaders{
						RequestHeaders: &extProcPb.HttpHeaders{
							Headers: &corev3.HeaderMap{
								Headers: []*corev3.HeaderValue{
									{Key: ":scheme", RawValue: []byte("http")},
									{Key: ":authority", RawValue: []byte("localhost:9002")},
									{Key: ":path", RawValue: []byte("/sandbox")},
								},
							},
						},
					},
				},
			},
			expectError: false,
			expectResp: []*extProcPb.ProcessingResponse{
				{
					Response: &extProcPb.ProcessingResponse_RequestHeaders{
						RequestHeaders: &extProcPb.HeadersResponse{
							Response: &extProcPb.CommonResponse{
								HeaderMutation: &extProcPb.HeaderMutation{
									SetHeaders: []*corev3.HeaderValueOption{
										{
											Header: &corev3.HeaderValue{
												Key:      "x-envoy-original-dst-host",
												RawValue: []byte("192.168.1.10:8080"),
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
		{
			name:        "非沙箱请求，转发到负载均衡器",
			setupRoutes: []Route{},
			adapter: &testRequestAdapter{
				isSandboxRequest: false,
				entry:            "127.0.0.1:8080",
			},
			requests: []*extProcPb.ProcessingRequest{
				{
					Request: &extProcPb.ProcessingRequest_RequestHeaders{
						RequestHeaders: &extProcPb.HttpHeaders{
							Headers: &corev3.HeaderMap{
								Headers: []*corev3.HeaderValue{
									{Key: ":scheme", RawValue: []byte("http")},
									{Key: ":authority", RawValue: []byte("api.example.com")},
									{Key: ":path", RawValue: []byte("/api")},
								},
							},
						},
					},
				},
			},
			expectError: false,
			expectResp: []*extProcPb.ProcessingResponse{
				{
					Response: &extProcPb.ProcessingResponse_RequestHeaders{
						RequestHeaders: &extProcPb.HeadersResponse{
							Response: &extProcPb.CommonResponse{
								HeaderMutation: &extProcPb.HeaderMutation{
									SetHeaders: []*corev3.HeaderValueOption{
										{
											Header: &corev3.HeaderValue{
												Key:      "x-envoy-original-dst-host",
												RawValue: []byte("127.0.0.1:8080"),
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
		{
			name: "沙箱映射失败",
			setupRoutes: []Route{
				{ID: "sandbox1", IP: "192.168.1.10", Owner: "user1"},
			},
			adapter: &testRequestAdapter{
				isSandboxRequest: true,
				mapResult: mapResult{
					sandboxID:   "",
					sandboxPort: 0,
					user:        "",
					err:         status.Errorf(codes.Internal, "mapping failed"),
				},
				entry: "127.0.0.1:8080",
			},
			requests: []*extProcPb.ProcessingRequest{
				{
					Request: &extProcPb.ProcessingRequest_RequestHeaders{
						RequestHeaders: &extProcPb.HttpHeaders{
							Headers: &corev3.HeaderMap{
								Headers: []*corev3.HeaderValue{
									{Key: ":scheme", RawValue: []byte("http")},
									{Key: ":authority", RawValue: []byte("localhost:9002")},
									{Key: ":path", RawValue: []byte("/sandbox")},
								},
							},
						},
					},
				},
			},
			expectError: false,
			expectResp: []*extProcPb.ProcessingResponse{
				{
					Response: &extProcPb.ProcessingResponse_ImmediateResponse{
						ImmediateResponse: &extProcPb.ImmediateResponse{
							Status: &types.HttpStatus{
								Code: types.StatusCode(500),
							},
							Body: []byte("failed to map request to sandbox, URL=http://localhost:9002/sandbox"),
						},
					},
				},
			},
		},
		{
			name:        "沙箱路由未找到",
			setupRoutes: []Route{},
			adapter: &testRequestAdapter{
				isSandboxRequest: true,
				mapResult: mapResult{
					sandboxID:   "nonexistent",
					sandboxPort: 8080,
					user:        "user1",
					err:         nil,
				},
				authorizeResult: true,
				entry:           "127.0.0.1:8080",
			},
			requests: []*extProcPb.ProcessingRequest{
				{
					Request: &extProcPb.ProcessingRequest_RequestHeaders{
						RequestHeaders: &extProcPb.HttpHeaders{
							Headers: &corev3.HeaderMap{
								Headers: []*corev3.HeaderValue{
									{Key: ":scheme", RawValue: []byte("http")},
									{Key: ":authority", RawValue: []byte("localhost:9002")},
									{Key: ":path", RawValue: []byte("/sandbox")},
								},
							},
						},
					},
				},
			},
			expectError: false,
			expectResp: []*extProcPb.ProcessingResponse{
				{
					Response: &extProcPb.ProcessingResponse_ImmediateResponse{
						ImmediateResponse: &extProcPb.ImmediateResponse{
							Status: &types.HttpStatus{
								Code: types.StatusCode(404),
							},
							Body: []byte("route for sandbox nonexistent not found"),
						},
					},
				},
			},
		},
		{
			name: "用户未授权访问沙箱",
			setupRoutes: []Route{
				{ID: "sandbox1", IP: "192.168.1.10", Owner: "owner1"},
			},
			adapter: &testRequestAdapter{
				isSandboxRequest: true,
				mapResult: mapResult{
					sandboxID:   "sandbox1",
					sandboxPort: 8080,
					user:        "user2",
					err:         nil,
				},
				authorizeResult: false,
				entry:           "127.0.0.1:8080",
			},
			requests: []*extProcPb.ProcessingRequest{
				{
					Request: &extProcPb.ProcessingRequest_RequestHeaders{
						RequestHeaders: &extProcPb.HttpHeaders{
							Headers: &corev3.HeaderMap{
								Headers: []*corev3.HeaderValue{
									{Key: ":scheme", RawValue: []byte("http")},
									{Key: ":authority", RawValue: []byte("localhost:9002")},
									{Key: ":path", RawValue: []byte("/sandbox")},
								},
							},
						},
					},
				},
			},
			expectError: false,
			expectResp: []*extProcPb.ProcessingResponse{
				{
					Response: &extProcPb.ProcessingResponse_ImmediateResponse{
						ImmediateResponse: &extProcPb.ImmediateResponse{
							Status: &types.HttpStatus{
								Code: types.StatusCode(401),
							},
							Body: []byte("user user2 is not authorized to access sandbox sandbox1"),
						},
					},
				},
			},
		},
		{
			name:        "接收请求时发生错误",
			setupRoutes: []Route{},
			adapter: &testRequestAdapter{
				entry: "127.0.0.1:8080",
			},
			requests:    []*extProcPb.ProcessingRequest{},
			serverError: status.Errorf(codes.Unknown, "receive error"),
			expectError: true,
		},
		{
			name: "发送响应时发生错误",
			setupRoutes: []Route{
				{ID: "sandbox1", IP: "192.168.1.10", Owner: "user1"},
			},
			adapter: &testRequestAdapter{
				isSandboxRequest: true,
				mapResult: mapResult{
					sandboxID:   "sandbox1",
					sandboxPort: 8080,
					user:        "user1",
					err:         nil,
				},
				authorizeResult: true,
				entry:           "127.0.0.1:8080",
			},
			requests: []*extProcPb.ProcessingRequest{
				{
					Request: &extProcPb.ProcessingRequest_RequestHeaders{
						RequestHeaders: &extProcPb.HttpHeaders{
							Headers: &corev3.HeaderMap{
								Headers: []*corev3.HeaderValue{
									{Key: ":scheme", RawValue: []byte("http")},
									{Key: ":authority", RawValue: []byte("localhost:9002")},
									{Key: ":path", RawValue: []byte("/sandbox")},
								},
							},
						},
					},
				},
			},
			serverError: status.Errorf(codes.Unknown, "send error"),
			expectError: true,
		},
		{
			name:        "未知请求类型",
			setupRoutes: []Route{},
			adapter: &testRequestAdapter{
				entry: "127.0.0.1:8080",
			},
			requests: []*extProcPb.ProcessingRequest{
				{
					Request: &extProcPb.ProcessingRequest_ResponseHeaders{
						ResponseHeaders: &extProcPb.HttpHeaders{},
					},
				},
			},
			expectError: false,
			expectResp: []*extProcPb.ProcessingResponse{
				{
					Response: &extProcPb.ProcessingResponse_RequestHeaders{
						RequestHeaders: &extProcPb.HeadersResponse{
							Response: &extProcPb.CommonResponse{},
						},
					},
				},
			},
		},
		{
			name: "extra headers",
			setupRoutes: []Route{
				{ID: "sandbox1", IP: "192.168.1.10", Owner: "user1"},
			},
			adapter: &testRequestAdapter{
				isSandboxRequest: true,
				mapResult: mapResult{
					sandboxID:   "sandbox1",
					sandboxPort: 8080,
					extraHeaders: map[string]string{
						"foo": "bar",
					},
					user: "user1",
					err:  nil,
				},
				authorizeResult: true,
				entry:           "127.0.0.1:8080",
			},
			requests: []*extProcPb.ProcessingRequest{
				{
					Request: &extProcPb.ProcessingRequest_RequestHeaders{
						RequestHeaders: &extProcPb.HttpHeaders{
							Headers: &corev3.HeaderMap{
								Headers: []*corev3.HeaderValue{
									{Key: ":scheme", RawValue: []byte("http")},
									{Key: ":authority", RawValue: []byte("localhost:9002")},
									{Key: ":path", RawValue: []byte("/sandbox")},
								},
							},
						},
					},
				},
			},
			expectError: false,
			expectResp: []*extProcPb.ProcessingResponse{
				{
					Response: &extProcPb.ProcessingResponse_RequestHeaders{
						RequestHeaders: &extProcPb.HeadersResponse{
							Response: &extProcPb.CommonResponse{
								HeaderMutation: &extProcPb.HeaderMutation{
									SetHeaders: []*corev3.HeaderValueOption{
										{
											Header: &corev3.HeaderValue{
												Key:      "foo",
												RawValue: []byte("bar"),
											},
										},
										{
											Header: &corev3.HeaderValue{
												Key:      "x-envoy-original-dst-host",
												RawValue: []byte("192.168.1.10:8080"),
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 创建服务器
			server := NewServer(tt.adapter)

			// 设置路由
			for _, route := range tt.setupRoutes {
				server.SetRoute(route.ID, route)
			}

			// 创建模拟的处理服务器
			mockServer := &mockProcessServer{
				reqs: tt.requests,
				err:  tt.serverError,
			}

			// 执行测试
			err := server.Process(mockServer)

			// 验证结果
			if tt.expectError {
				if err == nil {
					t.Errorf("期望错误但没有错误")
				}
			} else {
				if err != nil {
					t.Errorf("未期望错误但发生了错误: %v", err)
				}

				// 验证响应数量
				if len(mockServer.resp) != len(tt.expectResp) {
					t.Errorf("期望 %d 个响应，但得到了 %d 个", len(tt.expectResp), len(mockServer.resp))
				}
				// 验证响应内容
				for i, expected := range tt.expectResp {
					if i >= len(mockServer.resp) {
						break
					}

					actual := mockServer.resp[i]

					// 检查响应类型
					switch expected.Response.(type) {
					case *extProcPb.ProcessingResponse_RequestHeaders:
						if actualHeader, ok := actual.Response.(*extProcPb.ProcessingResponse_RequestHeaders); ok {
							expectedHeader := expected.Response.(*extProcPb.ProcessingResponse_RequestHeaders)
							// 检查 HeaderMutation 是否存在
							if expectedHeader.RequestHeaders.Response.HeaderMutation != nil {
								if actualHeader.RequestHeaders.Response.HeaderMutation == nil {
									t.Errorf("期望 HeaderMutation 但没有得到")
								} else {
									// 检查设置的头部数量
									expectedHeaders := expectedHeader.RequestHeaders.Response.HeaderMutation.SetHeaders
									actualHeaders := actualHeader.RequestHeaders.Response.HeaderMutation.SetHeaders
									if len(expectedHeaders) != len(actualHeaders) {
										t.Errorf("期望 %d 个设置的头部，但得到了 %d 个", len(expectedHeaders), len(actualHeaders))
									}

									sort.Slice(actualHeaders, func(i, j int) bool {
										return actualHeaders[i].Header.Key < actualHeaders[j].Header.Key
									})
									sort.Slice(expectedHeaders, func(i, j int) bool {
										return expectedHeaders[i].Header.Key < expectedHeaders[j].Header.Key
									})

									// 检查每个头部
									for j, expectedHeader := range expectedHeaders {
										if j >= len(actualHeaders) {
											continue
										}
										actualHeader := actualHeaders[j]

										if string(expectedHeader.Header.RawValue) != string(actualHeader.Header.RawValue) {
											t.Errorf("头部值 key %s 不匹配，期望: %s, 实际: %s", expectedHeader.Header.Key,
												string(expectedHeader.Header.RawValue),
												string(actualHeader.Header.RawValue))
										}
									}
								}
							}
						} else {
							t.Errorf("响应类型不匹配，期望 RequestHeaders")
						}
					case *extProcPb.ProcessingResponse_ImmediateResponse:
						if actualImmediate, ok := actual.Response.(*extProcPb.ProcessingResponse_ImmediateResponse); ok {
							expectedImmediate := expected.Response.(*extProcPb.ProcessingResponse_ImmediateResponse)

							// 检查状态码
							if expectedImmediate.ImmediateResponse.Status.Code != actualImmediate.ImmediateResponse.Status.Code {
								t.Errorf("状态码不匹配，期望: %v, 实际: %v",
									expectedImmediate.ImmediateResponse.Status.Code,
									actualImmediate.ImmediateResponse.Status.Code)
							}

							// 检查响应体
							if string(expectedImmediate.ImmediateResponse.Body) != string(actualImmediate.ImmediateResponse.Body) {
								t.Errorf("响应体不匹配，期望: %s, 实际: %s",
									string(expectedImmediate.ImmediateResponse.Body),
									string(actualImmediate.ImmediateResponse.Body))
							}
						} else {
							t.Errorf("响应类型不匹配，期望 ImmediateResponse")
						}
					}
				}
			}
		})
	}
}

// TestServer_Run_Stop 测试服务器的启动和停止
func TestServer_Run_Stop(t *testing.T) {
	// 创建测试适配器
	adapter := &testRequestAdapter{
		entry: "127.0.0.1:8080",
	}

	// 创建服务器
	server := NewServer(adapter)

	// 在后台启动服务器
	go func() {
		// 这里会因为端口占用而失败，但我们只关心API调用
		_ = server.Run()
	}()

	// 等待一点时间让 goroutine 启动
	time.Sleep(10 * time.Millisecond)

	// 停止服务器
	server.Stop()

	// 再次停止应该不会有问题
	server.Stop()
}
