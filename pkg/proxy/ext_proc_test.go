/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package proxy

import (
	"context"
	"io"
	"net"
	"sort"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	types "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	k8stypes "k8s.io/apimachinery/pkg/types"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandboxroute"
	"github.com/openkruise/agents/pkg/sandboxroute/refresh"
	"github.com/openkruise/agents/pkg/servers/e2b/adapters"
	"github.com/openkruise/agents/pkg/utils/network"
)

// testRequestAdapter is a RequestAdapter implementation for testing
type testRequestAdapter struct {
	entry            string
	isSandboxRequest bool
	mapResult        mapResult
}

type mapResult struct {
	sandboxID    string
	sandboxPort  int
	extraHeaders map[string]string
	err          error
}

func (t *testRequestAdapter) Map(*adapters.ParsedRequest) (string, int, map[string]string, error) {
	return t.mapResult.sandboxID, t.mapResult.sandboxPort, t.mapResult.extraHeaders, t.mapResult.err
}

func (t *testRequestAdapter) IsSandboxRequest(string, string, int) bool {
	return t.isSandboxRequest
}

func (t *testRequestAdapter) Entry() string {
	return t.entry
}

func (t *testRequestAdapter) ParseRequest(headers map[string]string) *adapters.ParsedRequest {
	// Use a real E2BAdapter to parse, so tests exercise the real parsing logic
	a := adapters.NewE2BAdapter(0, "")
	return a.ParseRequest(headers)
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
		setupRoutes []sandboxroute.Route
		adapter     *testRequestAdapter
		requests    []*extProcPb.ProcessingRequest
		serverError error
		expectError bool
		expectResp  []*extProcPb.ProcessingResponse
	}{
		{
			name: "normal",
			setupRoutes: []sandboxroute.Route{
				{ID: "sandbox1", IP: "192.168.1.10", Owner: "user1"},
			},
			adapter: &testRequestAdapter{
				isSandboxRequest: true,
				mapResult: mapResult{
					sandboxID:   "sandbox1",
					sandboxPort: 8080,
					err:         nil,
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
			name: "IPv6 sandbox upstream uses bracketed host port",
			setupRoutes: []sandboxroute.Route{
				{ID: "sandbox-ipv6", IP: "2001:db8::1", Owner: "user1"},
			},
			adapter: &testRequestAdapter{
				isSandboxRequest: true,
				mapResult: mapResult{
					sandboxID:   "sandbox-ipv6",
					sandboxPort: 49999,
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
					Response: &extProcPb.ProcessingResponse_RequestHeaders{
						RequestHeaders: &extProcPb.HeadersResponse{
							Response: &extProcPb.CommonResponse{
								HeaderMutation: &extProcPb.HeaderMutation{
									SetHeaders: []*corev3.HeaderValueOption{
										{
											Header: &corev3.HeaderValue{
												Key:      "x-envoy-original-dst-host",
												RawValue: []byte("[2001:db8::1]:49999"),
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
			setupRoutes: []sandboxroute.Route{},
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
			setupRoutes: []sandboxroute.Route{
				{ID: "sandbox1", IP: "192.168.1.10", Owner: "user1"},
			},
			adapter: &testRequestAdapter{
				isSandboxRequest: true,
				mapResult: mapResult{
					sandboxID:   "",
					sandboxPort: 0,
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
			setupRoutes: []sandboxroute.Route{},
			adapter: &testRequestAdapter{
				isSandboxRequest: true,
				mapResult: mapResult{
					sandboxID:   "nonexistent",
					sandboxPort: 8080,
					err:         nil,
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
								Code: types.StatusCode(502),
							},
							Body: []byte("sandbox nonexistent not found"),
						},
					},
				},
			},
		},
		{
			name:        "bad port",
			setupRoutes: []sandboxroute.Route{},
			adapter: &testRequestAdapter{
				isSandboxRequest: true,
				mapResult: mapResult{
					sandboxID:   "sandbox1",
					sandboxPort: 99999,
					err:         nil,
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
									{Key: ":authority", RawValue: []byte("99999-id.example.com")},
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
								Code: types.StatusCode(400),
							},
							Body: []byte("invalid sandbox port: 99999"),
						},
					},
				},
			},
		},
		{
			name:        "sandbox not healthy",
			setupRoutes: []sandboxroute.Route{{ID: "sandbox1", IP: "192.168.1.10", Owner: "user1", State: agentsv1alpha1.SandboxStateDead}},
			adapter: &testRequestAdapter{
				isSandboxRequest: true,
				mapResult: mapResult{
					sandboxID:   "sandbox1",
					sandboxPort: 9999,
					err:         nil,
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
									{Key: ":authority", RawValue: []byte("99999-id.example.com")},
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
								Code: types.StatusCode(502),
							},
							Body: []byte("healthy sandbox sandbox1 not found"),
						},
					},
				},
			},
		},
		{
			name:        "receive failed",
			setupRoutes: []sandboxroute.Route{},
			adapter: &testRequestAdapter{
				entry: "127.0.0.1:8080",
			},
			requests:    []*extProcPb.ProcessingRequest{},
			serverError: status.Errorf(codes.Unknown, "receive error"),
			expectError: true,
		},
		{
			name: "send response error",
			setupRoutes: []sandboxroute.Route{
				{ID: "sandbox1", IP: "192.168.1.10", Owner: "user1"},
			},
			adapter: &testRequestAdapter{
				isSandboxRequest: true,
				mapResult: mapResult{
					sandboxID:   "sandbox1",
					sandboxPort: 8080,
					err:         nil,
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
			serverError: status.Errorf(codes.Unknown, "send error"),
			expectError: true,
		},
		{
			name:        "unknown request type",
			setupRoutes: []sandboxroute.Route{},
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
			setupRoutes: []sandboxroute.Route{
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
					err: nil,
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
			server := NewServer(config.SandboxManagerOptions{ExtProcMaxConcurrency: 1000})
			server.SetRequestAdapter(tt.adapter)

			// Setup routes
			for _, route := range tt.setupRoutes {
				if route.State == "" {
					route.State = agentsv1alpha1.SandboxStateRunning
				}
				if route.UID == "" {
					route.UID = k8stypes.UID("uid-" + route.ID)
				}
				if route.ResourceVersion == "" {
					route.ResourceVersion = "1"
				}
				if route.Namespace == "" {
					route.Namespace = "ns"
				}
				if route.Name == "" {
					route.Name = route.ID
				}
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
							assert.Equal(t, expectedImmediate.ImmediateResponse.Status.Code, actualImmediate.ImmediateResponse.Status.Code)
							// Check response body
							assert.Contains(t, string(actualImmediate.ImmediateResponse.Body), string(expectedImmediate.ImmediateResponse.Body))
						} else {
							t.Errorf("response type mismatch, expected ImmediateResponse")
						}
					}
				}
			}
		})
	}
}

func TestSanitizedHeaders_MarshalLog(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		want    map[string]string
	}{
		{
			name:    "nil map",
			headers: nil,
			want:    map[string]string{},
		},
		{
			name: "routing headers are preserved",
			headers: map[string]string{
				":authority":                "8000-sandbox1.e2b.app",
				":path":                     "/health",
				"e2b-sandbox-id":            "sandbox1",
				"e2b-sandbox-port":          "8000",
				"x-request-id":              "req-1",
				"x-envoy-original-dst-host": "192.168.1.10:8000",
			},
			want: map[string]string{
				":authority":                "8000-sandbox1.e2b.app",
				":path":                     "/health",
				"e2b-sandbox-id":            "sandbox1",
				"e2b-sandbox-port":          "8000",
				"x-request-id":              "req-1",
				"x-envoy-original-dst-host": "192.168.1.10:8000",
			},
		},
		{
			name: "credential headers are redacted",
			headers: map[string]string{
				"authorization":            "Bearer secret-jwt",
				"proxy-authorization":      "Basic dXNlcjpwYXNz",
				"x-backend-authorization":  "Bearer backend-jwt",
				"x-access-token":           "raw-access-token",
				"e2b-traffic-access-token": "traffic-jwt",
				"x-api-key":                "e2b-api-key",
				"cookie":                   "session=abc",
			},
			want: map[string]string{
				"authorization":            redactedHeaderValue,
				"proxy-authorization":      redactedHeaderValue,
				"x-backend-authorization":  redactedHeaderValue,
				"x-access-token":           redactedHeaderValue,
				"e2b-traffic-access-token": redactedHeaderValue,
				"x-api-key":                redactedHeaderValue,
				"cookie":                   redactedHeaderValue,
			},
		},
		{
			name: "matching is case insensitive",
			headers: map[string]string{
				"Authorization":  "Bearer secret-jwt",
				"X-API-Key":      "e2b-api-key",
				"E2B-Sandbox-ID": "sandbox1",
			},
			want: map[string]string{
				"Authorization":  redactedHeaderValue,
				"X-API-Key":      redactedHeaderValue,
				"E2B-Sandbox-ID": "sandbox1",
			},
		},
		{
			name: "credentials are redacted alongside routing headers",
			headers: map[string]string{
				":path":          "/files",
				"e2b-sandbox-id": "sandbox1",
				"authorization":  "Bearer secret-jwt",
			},
			want: map[string]string{
				":path":          "/files",
				"e2b-sandbox-id": "sandbox1",
				"authorization":  redactedHeaderValue,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var original map[string]string
			if tt.headers != nil {
				original = make(map[string]string, len(tt.headers))
				for name, value := range tt.headers {
					original[name] = value
				}
			}

			got := sanitizedHeaders(tt.headers).MarshalLog()
			assert.Equal(t, tt.want, got)

			// Redaction must not touch the caller's map: the same map keeps
			// feeding the header mutation sent back to envoy.
			assert.Equal(t, original, tt.headers)
		})
	}
}

// TestServer_RunLifecycle binds the fixed route-refresh and ext-proc ports;
// `make test` runs packages serially so it cannot collide with the e2b
// controller tests that bind the same ports.
func TestServer_RunLifecycle(t *testing.T) {
	tests := []struct {
		name         string
		options      config.SandboxManagerOptions
		occupyGRPC   bool
		wantRunErr   string
		wantGRPC     bool
		wantReleased []string
	}{
		{
			name:         "ext-proc bind failure returns synchronously",
			options:      config.SandboxManagerOptions{ExtProcMaxConcurrency: 1000},
			occupyGRPC:   true,
			wantRunErr:   "listen for envoy ext-proc",
			wantReleased: []string{network.ListenAddress("", refresh.DefaultPort)},
		},
		{
			name:         "run then stop releases both listeners",
			options:      config.SandboxManagerOptions{ExtProcMaxConcurrency: 1000},
			wantGRPC:     true,
			wantReleased: []string{network.ListenAddress("", refresh.DefaultPort), network.ListenAddress("", consts.ExtProcPort)},
		},
		{
			name:         "grpc listener skipped when ext-proc disabled",
			options:      config.SandboxManagerOptions{DisableEnvoyExtProc: true},
			occupyGRPC:   true,
			wantReleased: []string{network.ListenAddress("", refresh.DefaultPort)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.occupyGRPC {
				occupied, err := net.Listen("tcp", network.ListenAddress("", consts.ExtProcPort))
				require.NoError(t, err)
				t.Cleanup(func() { _ = occupied.Close() })
			}

			server := NewServer(tt.options)
			err := server.Run()
			if tt.wantRunErr != "" {
				require.Error(t, err)
				assert.ErrorContains(t, err, tt.wantRunErr)
				assert.Nil(t, server.httpSrv)
				assert.Nil(t, server.grpcSrv)
			} else {
				require.NoError(t, err)
				require.NotNil(t, server.httpSrv)
				if tt.wantGRPC {
					require.NotNil(t, server.grpcSrv)
				} else {
					assert.Nil(t, server.grpcSrv)
				}
				server.Stop(t.Context())
			}
			// Serve may still be entering when Stop runs; it then closes the
			// listener itself, so release is prompt but not synchronous.
			for _, addr := range tt.wantReleased {
				require.Eventuallyf(t, func() bool {
					listener, err := net.Listen("tcp", addr)
					if err != nil {
						return false
					}
					return assert.NoError(t, listener.Close())
				}, time.Second, 5*time.Millisecond, "listener on %s must be released", addr)
			}
		})
	}
}
