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
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// testRequestAdapter is a RequestAdapter implementation for testing
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

// mockProcessServer is a mock implementation of the ExternalProcessor_ProcessServer interface
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
			name: "normal",
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
			name:        "non-sandbox",
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
			name: "mapping failed",
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
			name:        "route not found",
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
			name: "unauthorized",
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
			name:        "receive failed",
			setupRoutes: []Route{},
			adapter: &testRequestAdapter{
				entry: "127.0.0.1:8080",
			},
			requests:    []*extProcPb.ProcessingRequest{},
			serverError: status.Errorf(codes.Unknown, "receive error"),
			expectError: true,
		},
		{
			name: "send response error",
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
			name:        "unknown request type",
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
			// Create server
			server := NewServer(tt.adapter)

			// Setup routes
			for _, route := range tt.setupRoutes {
				route.State = agentsv1alpha1.SandboxStateRunning
				server.SetRoute(route)
			}

			// Create mock processing server
			mockServer := &mockProcessServer{
				reqs: tt.requests,
				err:  tt.serverError,
			}

			// Execute test
			err := server.Process(mockServer)

			// Verify results
			if tt.expectError {
				if err == nil {
					t.Errorf("an error is expected")
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}

				// Verify response count
				if len(mockServer.resp) != len(tt.expectResp) {
					t.Errorf("expect %d responses, got %d", len(tt.expectResp), len(mockServer.resp))
				}
				// Verify response content
				for i, expected := range tt.expectResp {
					if i >= len(mockServer.resp) {
						break
					}

					actual := mockServer.resp[i]

					// Check response type
					switch expected.Response.(type) {
					case *extProcPb.ProcessingResponse_RequestHeaders:
						if actualHeader, ok := actual.Response.(*extProcPb.ProcessingResponse_RequestHeaders); ok {
							expectedHeader := expected.Response.(*extProcPb.ProcessingResponse_RequestHeaders)
							// Check if HeaderMutation exists
							if expectedHeader.RequestHeaders.Response.HeaderMutation != nil {
								if actualHeader.RequestHeaders.Response.HeaderMutation == nil {
									t.Errorf("expect HeaderMutation")
								} else {
									// Check number of set headers
									expectedHeaders := expectedHeader.RequestHeaders.Response.HeaderMutation.SetHeaders
									actualHeaders := actualHeader.RequestHeaders.Response.HeaderMutation.SetHeaders
									if len(expectedHeaders) != len(actualHeaders) {
										t.Errorf("expect %d setHeaders, got %d", len(expectedHeaders), len(actualHeaders))
									}

									sort.Slice(actualHeaders, func(i, j int) bool {
										return actualHeaders[i].Header.Key < actualHeaders[j].Header.Key
									})
									sort.Slice(expectedHeaders, func(i, j int) bool {
										return expectedHeaders[i].Header.Key < expectedHeaders[j].Header.Key
									})

									// Check each header
									for j, expectedHeader := range expectedHeaders {
										if j >= len(actualHeaders) {
											continue
										}
										actualHeader := actualHeaders[j]

										if string(expectedHeader.Header.RawValue) != string(actualHeader.Header.RawValue) {
											t.Errorf("header key %s not match, expect: %s, actual: %s", expectedHeader.Header.Key,
												string(expectedHeader.Header.RawValue),
												string(actualHeader.Header.RawValue))
										}
									}
								}
							}
						} else {
							t.Errorf("response type mismatch, expected RequestHeaders")
						}
					case *extProcPb.ProcessingResponse_ImmediateResponse:
						if actualImmediate, ok := actual.Response.(*extProcPb.ProcessingResponse_ImmediateResponse); ok {
							expectedImmediate := expected.Response.(*extProcPb.ProcessingResponse_ImmediateResponse)

							// Check status code
							if expectedImmediate.ImmediateResponse.Status.Code != actualImmediate.ImmediateResponse.Status.Code {
								t.Errorf("status code mismatch, expected: %v, actual: %v",
									expectedImmediate.ImmediateResponse.Status.Code,
									actualImmediate.ImmediateResponse.Status.Code)
							}

							// Check response body
							if string(expectedImmediate.ImmediateResponse.Body) != string(actualImmediate.ImmediateResponse.Body) {
								t.Errorf("response body mismatch, expected: %s, actual: %s",
									string(expectedImmediate.ImmediateResponse.Body),
									string(actualImmediate.ImmediateResponse.Body))
							}
						} else {
							t.Errorf("response type mismatch, expected ImmediateResponse")
						}
					}
				}
			}
		})
	}
}

// TestServer_Run_Stop tests server start and stop
func TestServer_Run_Stop(t *testing.T) {
	// Create test adapter
	adapter := &testRequestAdapter{
		entry: "127.0.0.1:8080",
	}

	// Create server
	server := NewServer(adapter)

	// Start server in background
	go func() {
		// This will fail due to port occupation, but we only care about API calls
		_ = server.Run()
	}()

	// Wait a bit for goroutine to start
	time.Sleep(10 * time.Millisecond)

	// Stop server
	server.Stop()

	// Stopping again should be fine
	server.Stop()
}
