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

package filter

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	v3 "github.com/cncf/xds/go/xds/type/v3"
	"github.com/envoyproxy/envoy/contrib/golang/common/go/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/openkruise/agents/pkg/identity/oidc"
)

type fakeJWTAuthManager struct {
	configureErr   error
	configuredWith []bool
	verifier       oidc.Verifier
}

type fakeConfigCallbackHandler struct{}

func (*fakeConfigCallbackHandler) DefineCounterMetric(string) api.CounterMetric { return nil }
func (*fakeConfigCallbackHandler) DefineGaugeMetric(string) api.GaugeMetric     { return nil }

func (m *fakeJWTAuthManager) Configure(enabled bool) error {
	m.configuredWith = append(m.configuredWith, enabled)
	return m.configureErr
}

func (m *fakeJWTAuthManager) Current() oidc.Verifier {
	return m.verifier
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *Config
		wantErr bool
	}{
		{
			name:    "default config",
			cfg:     DefaultConfig(),
			wantErr: false,
		},
		{
			name: "custom sandbox header",
			cfg: &Config{
				SandboxHeaderName: "custom-sandbox-id",
				SandboxPortHeader: "custom-sandbox-port",
				HostHeaderName:    "X-Host",
				DefaultPort:       "8080",
			},
			wantErr: false,
		},
		{
			name:    "empty config",
			cfg:     &Config{},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestGetSandboxHeaderName(t *testing.T) {
	tests := []struct {
		name           string
		cfg            *Config
		wantHeaderName string
	}{
		{
			name:           "empty config uses default",
			cfg:            &Config{},
			wantHeaderName: "e2b-sandbox-id",
		},
		{
			name: "custom sandbox header name",
			cfg: &Config{
				SandboxHeaderName: "custom-sandbox-id",
			},
			wantHeaderName: "custom-sandbox-id",
		},
		{
			name:           "default config",
			cfg:            DefaultConfig(),
			wantHeaderName: "e2b-sandbox-id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.GetSandboxHeaderName()
			if got != tt.wantHeaderName {
				t.Errorf("GetSandboxHeaderName() = %q, want %q", got, tt.wantHeaderName)
			}
		})
	}
}

func TestGetHostHeaderName(t *testing.T) {
	tests := []struct {
		name           string
		cfg            *Config
		wantHeaderName string
	}{
		{
			name:           "empty config uses default",
			cfg:            &Config{},
			wantHeaderName: "Host",
		},
		{
			name: "custom host header name",
			cfg: &Config{
				HostHeaderName: "X-Forwarded-Host",
			},
			wantHeaderName: "X-Forwarded-Host",
		},
		{
			name:           "default config",
			cfg:            DefaultConfig(),
			wantHeaderName: "Host",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.GetHostHeaderName()
			if got != tt.wantHeaderName {
				t.Errorf("GetHostHeaderName() = %q, want %q", got, tt.wantHeaderName)
			}
		})
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.SandboxHeaderName != "e2b-sandbox-id" {
		t.Errorf("DefaultConfig().SandboxHeaderName = %q, want %q", cfg.SandboxHeaderName, "e2b-sandbox-id")
	}
	if cfg.SandboxPortHeader != "e2b-sandbox-port" {
		t.Errorf("DefaultConfig().SandboxPortHeader = %q, want %q", cfg.SandboxPortHeader, "e2b-sandbox-port")
	}
	if cfg.HostHeaderName != "Host" {
		t.Errorf("DefaultConfig().HostHeaderName = %q, want %q", cfg.HostHeaderName, "Host")
	}
	if cfg.DefaultPort != "49983" {
		t.Errorf("DefaultConfig().DefaultPort = %q, want %q", cfg.DefaultPort, "49983")
	}
	if cfg.EnableAuth || cfg.EnableJWTAuth {
		t.Errorf("DefaultConfig() auth modes = (%t, %t), want both false", cfg.EnableAuth, cfg.EnableJWTAuth)
	}
	if cfg.GetTrafficAccessTokenHeader() != DefaultTrafficAccessTokenHeader {
		t.Errorf("GetTrafficAccessTokenHeader() = %q, want %q", cfg.GetTrafficAccessTokenHeader(), DefaultTrafficAccessTokenHeader)
	}
}

func TestConfigValidateJWT(t *testing.T) {
	tests := []struct {
		name        string
		cfg         Config
		expectError string
	}{
		{name: "JWT mode", cfg: Config{EnableAuth: true, EnableJWTAuth: true}},
		{name: "JWT requires auth", cfg: Config{EnableJWTAuth: true}, expectError: "requires enable-auth"},
		{name: "custom header", cfg: Config{EnableAuth: true, EnableJWTAuth: true, TrafficAccessTokenHeader: "X-Custom-JWT"}},
		{name: "invalid header", cfg: Config{EnableAuth: true, EnableJWTAuth: true, TrafficAccessTokenHeader: "bad header"}, expectError: "not a valid HTTP header"},
		{name: "runtime token conflict", cfg: Config{EnableAuth: true, EnableJWTAuth: true, TrafficAccessTokenHeader: "X-Access-Token"}, expectError: "must differ"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.expectError == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.expectError) {
				t.Fatalf("Validate() error = %v, want containing %q", err, tt.expectError)
			}
		})
	}
}

func TestConfigParserJWT(t *testing.T) {
	tests := []struct {
		name             string
		values           map[string]any
		nilValue         bool
		manager          *fakeJWTAuthManager
		expectError      string
		expectConfigured []bool
		expectJWT        bool
		expectHeader     string
	}{
		{
			name:             "global nil value configures JWT disabled",
			nilValue:         true,
			manager:          &fakeJWTAuthManager{},
			expectConfigured: []bool{false},
			expectHeader:     DefaultTrafficAccessTokenHeader,
		},
		{
			name:             "JWT enabled",
			values:           map[string]any{"enable-auth": true, "enable-jwt-auth": true},
			manager:          &fakeJWTAuthManager{},
			expectConfigured: []bool{true},
			expectJWT:        true,
			expectHeader:     DefaultTrafficAccessTokenHeader,
		},
		{
			name:             "JWT disabled",
			values:           map[string]any{"enable-auth": true},
			manager:          &fakeJWTAuthManager{},
			expectConfigured: []bool{false},
			expectHeader:     DefaultTrafficAccessTokenHeader,
		},
		{
			name:        "missing manager",
			values:      map[string]any{"enable-auth": true, "enable-jwt-auth": true},
			expectError: "manager is not configured",
		},
		{
			name:             "manager configuration error",
			values:           map[string]any{"enable-auth": true, "enable-jwt-auth": true},
			manager:          &fakeJWTAuthManager{configureErr: errors.New("conflict")},
			expectError:      "conflict",
			expectConfigured: []bool{true},
		},
		{
			name:             "custom header is normalized",
			values:           map[string]any{"enable-auth": true, "enable-jwt-auth": true, "traffic-access-token-header": "X-Custom-JWT"},
			manager:          &fakeJWTAuthManager{},
			expectConfigured: []bool{true},
			expectJWT:        true,
			expectHeader:     "x-custom-jwt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			typed := &v3.TypedStruct{}
			if !tt.nilValue {
				value, err := structpb.NewStruct(tt.values)
				if err != nil {
					t.Fatalf("NewStruct() error = %v", err)
				}
				typed.Value = value
			}
			input, err := anypb.New(typed)
			if err != nil {
				t.Fatalf("anypb.New() error = %v", err)
			}
			parser := &ConfigParser{}
			if tt.manager != nil {
				parser = NewConfigParser(tt.manager)
			}
			result, err := parser.Parse(input, &fakeConfigCallbackHandler{})
			if tt.expectError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.expectError) {
					t.Fatalf("Parse() error = %v, want containing %q", err, tt.expectError)
				}
			} else {
				if err != nil {
					t.Fatalf("Parse() error = %v", err)
				}
				cfg := result.(*FilterConfig)
				if cfg.EnableJWTAuth != tt.expectJWT {
					t.Errorf("EnableJWTAuth = %t, want %t", cfg.EnableJWTAuth, tt.expectJWT)
				}
				if got := cfg.GetTrafficAccessTokenHeader(); got != tt.expectHeader {
					t.Errorf("JWT header = %q, want %q", got, tt.expectHeader)
				}
			}
			if tt.manager != nil && !reflect.DeepEqual(tt.manager.configuredWith, tt.expectConfigured) {
				t.Errorf("Configure calls = %v, want %v", tt.manager.configuredWith, tt.expectConfigured)
			}
		})
	}
}

func TestConfigParserProcessWideJWTMode(t *testing.T) {
	tests := []struct {
		name            string
		childValues     map[string]any
		parseRouteFirst bool
		expectError     string
		expectHeader    string
	}{
		{
			name:            "route parsed first does not configure process mode",
			childValues:     map[string]any{},
			parseRouteFirst: true,
			expectHeader:    "x-global-jwt",
		},
		{
			name:         "explicit route enable is rejected",
			childValues:  map[string]any{"enable-auth": true, "enable-jwt-auth": true},
			expectError:  "cannot be configured per route",
			expectHeader: "x-global-jwt",
		},
		{
			name:         "explicit route disable is rejected",
			childValues:  map[string]any{"enable-jwt-auth": false},
			expectError:  "cannot be configured per route",
			expectHeader: "x-global-jwt",
		},
		{
			name: "explicit route header overrides global header",
			childValues: map[string]any{
				"traffic-access-token-header": "x-route-jwt",
			},
			expectHeader: "x-route-jwt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := &fakeJWTAuthManager{}
			parser := NewConfigParser(manager)
			if tt.parseRouteFirst {
				_, err := parser.Parse(typedFilterConfig(t, map[string]any{}), nil)
				require.NoError(t, err)
				assert.Empty(t, manager.configuredWith)
			}

			parentInput := typedFilterConfig(t, map[string]any{
				"enable-auth":                 true,
				"enable-jwt-auth":             true,
				"traffic-access-token-header": "x-global-jwt",
			})
			parentResult, err := parser.Parse(parentInput, &fakeConfigCallbackHandler{})
			require.NoError(t, err)

			childResult, err := parser.Parse(typedFilterConfig(t, tt.childValues), nil)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				require.NoError(t, err)
				merged := parser.Merge(parentResult, childResult).(*FilterConfig)
				assert.True(t, merged.EnableJWTAuth)
				assert.Equal(t, tt.expectHeader, merged.GetTrafficAccessTokenHeader())
			}
			assert.Equal(t, []bool{true}, manager.configuredWith)
		})
	}
}

func typedFilterConfig(t *testing.T, values map[string]any) *anypb.Any {
	t.Helper()
	value, err := structpb.NewStruct(values)
	require.NoError(t, err)
	input, err := anypb.New(&v3.TypedStruct{Value: value})
	require.NoError(t, err)
	return input
}

func TestConfigParserParse(t *testing.T) {
	parser := &ConfigParser{}

	tests := []struct {
		name              string
		input             *anypb.Any
		wantErr           bool
		wantSandboxHeader string
		wantHostHeader    string
		wantPortHeader    string
		wantDefaultPort   string
		wantEnableAuth    bool
	}{
		{
			name: "nil value in TypedStruct returns defaults",
			input: func() *anypb.Any {
				ts := &v3.TypedStruct{}
				a, _ := anypb.New(ts)
				return a
			}(),
			wantErr:           false,
			wantSandboxHeader: DefaultSandboxHeaderName,
			wantHostHeader:    DefaultHostHeaderName,
			wantPortHeader:    DefaultSandboxPortHeader,
			wantDefaultPort:   DefaultSandboxPort,
		},
		{
			name: "empty struct returns defaults",
			input: func() *anypb.Any {
				s, _ := structpb.NewStruct(map[string]any{})
				ts := &v3.TypedStruct{Value: s}
				a, _ := anypb.New(ts)
				return a
			}(),
			wantErr:           false,
			wantSandboxHeader: DefaultSandboxHeaderName,
			wantHostHeader:    DefaultHostHeaderName,
			wantPortHeader:    DefaultSandboxPortHeader,
			wantDefaultPort:   DefaultSandboxPort,
		},
		{
			name: "custom sandbox header",
			input: func() *anypb.Any {
				s, _ := structpb.NewStruct(map[string]any{
					"sandbox-header-name": "x-custom-sandbox",
				})
				ts := &v3.TypedStruct{Value: s}
				a, _ := anypb.New(ts)
				return a
			}(),
			wantErr:           false,
			wantSandboxHeader: "x-custom-sandbox",
			wantHostHeader:    DefaultHostHeaderName,
			wantPortHeader:    DefaultSandboxPortHeader,
			wantDefaultPort:   DefaultSandboxPort,
		},
		{
			name: "all custom values",
			input: func() *anypb.Any {
				s, _ := structpb.NewStruct(map[string]any{
					"sandbox-header-name": "x-sandbox",
					"host-header-name":    "X-Forwarded-Host",
					"sandbox-port-header": "x-port",
					"default-port":        "8080",
				})
				ts := &v3.TypedStruct{Value: s}
				a, _ := anypb.New(ts)
				return a
			}(),
			wantErr:           false,
			wantSandboxHeader: "x-sandbox",
			wantHostHeader:    "X-Forwarded-Host",
			wantPortHeader:    "x-port",
			wantDefaultPort:   "8080",
		},
		{
			name: "enable-auth parsed correctly",
			input: func() *anypb.Any {
				s, _ := structpb.NewStruct(map[string]any{
					"enable-auth": true,
				})
				ts := &v3.TypedStruct{Value: s}
				a, _ := anypb.New(ts)
				return a
			}(),
			wantErr:           false,
			wantSandboxHeader: DefaultSandboxHeaderName,
			wantHostHeader:    DefaultHostHeaderName,
			wantPortHeader:    DefaultSandboxPortHeader,
			wantDefaultPort:   DefaultSandboxPort,
			wantEnableAuth:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parser.Parse(tt.input, nil)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Parse() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			fc, ok := result.(*FilterConfig)
			if !ok {
				t.Fatalf("Parse() returned %T, want *FilterConfig", result)
			}
			if got := fc.GetSandboxHeaderName(); got != tt.wantSandboxHeader {
				t.Errorf("SandboxHeaderName = %q, want %q", got, tt.wantSandboxHeader)
			}
			if got := fc.GetHostHeaderName(); got != tt.wantHostHeader {
				t.Errorf("HostHeaderName = %q, want %q", got, tt.wantHostHeader)
			}
			if got := fc.GetSandboxPortHeader(); got != tt.wantPortHeader {
				t.Errorf("SandboxPortHeader = %q, want %q", got, tt.wantPortHeader)
			}
			if fc.DefaultPort != tt.wantDefaultPort && tt.wantDefaultPort != "" {
				t.Errorf("DefaultPort = %q, want %q", fc.DefaultPort, tt.wantDefaultPort)
			}
			if fc.EnableAuth != tt.wantEnableAuth {
				t.Errorf("EnableAuth = %v, want %v", fc.EnableAuth, tt.wantEnableAuth)
			}
			if fc.Adapter == nil {
				t.Error("Parse() returned FilterConfig with nil Adapter")
			}
		})
	}
}

func TestConfigParserParseInvalidAny(t *testing.T) {
	parser := &ConfigParser{}

	// Provide an Any that does not contain a TypedStruct
	invalidAny := &anypb.Any{
		TypeUrl: "type.googleapis.com/some.invalid.Type",
		Value:   []byte("not a valid proto"),
	}

	_, err := parser.Parse(invalidAny, nil)
	if err == nil {
		t.Fatal("Parse() expected error for invalid Any, got nil")
	}
}

func TestConfigParserMerge(t *testing.T) {
	parser := &ConfigParser{}

	tests := []struct {
		name              string
		parent            *Config
		child             *Config
		wantSandboxHeader string
		wantHostHeader    string
		wantPortHeader    string
		wantDefaultPort   string
		wantEnableAuth    bool
	}{
		{
			name:              "child overrides all parent fields",
			parent:            DefaultConfig(),
			child:             &Config{SandboxHeaderName: "child-sbx", HostHeaderName: "child-host", SandboxPortHeader: "child-port", DefaultPort: "9999", EnableAuth: true},
			wantSandboxHeader: "child-sbx",
			wantHostHeader:    "child-host",
			wantPortHeader:    "child-port",
			wantDefaultPort:   "9999",
			wantEnableAuth:    true,
		},
		{
			name:              "empty child preserves parent",
			parent:            &Config{SandboxHeaderName: "parent-sbx", HostHeaderName: "parent-host", SandboxPortHeader: "parent-port", DefaultPort: "1234", EnableAuth: true},
			child:             &Config{},
			wantSandboxHeader: "parent-sbx",
			wantHostHeader:    "parent-host",
			wantPortHeader:    "parent-port",
			wantDefaultPort:   "1234",
			wantEnableAuth:    true,
		},
		{
			name:              "partial child override",
			parent:            DefaultConfig(),
			child:             &Config{SandboxHeaderName: "override-sbx"},
			wantSandboxHeader: "override-sbx",
			wantHostHeader:    DefaultHostHeaderName,
			wantPortHeader:    DefaultSandboxPortHeader,
			wantDefaultPort:   DefaultSandboxPort,
			wantEnableAuth:    false,
		},
		{
			name:              "both defaults",
			parent:            DefaultConfig(),
			child:             DefaultConfig(),
			wantSandboxHeader: DefaultSandboxHeaderName,
			wantHostHeader:    DefaultHostHeaderName,
			wantPortHeader:    DefaultSandboxPortHeader,
			wantDefaultPort:   DefaultSandboxPort,
			wantEnableAuth:    false,
		},
		{
			name:              "child enables auth overriding parent disabled",
			parent:            &Config{SandboxHeaderName: DefaultSandboxHeaderName, DefaultPort: DefaultSandboxPort, EnableAuth: false},
			child:             &Config{EnableAuth: true},
			wantSandboxHeader: DefaultSandboxHeaderName,
			wantHostHeader:    DefaultHostHeaderName,
			wantPortHeader:    DefaultSandboxPortHeader,
			wantDefaultPort:   DefaultSandboxPort,
			wantEnableAuth:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parentFC := NewFilterConfig(tt.parent)
			childFC := NewFilterConfig(tt.child)

			result := parser.Merge(parentFC, childFC)
			fc, ok := result.(*FilterConfig)
			if !ok {
				t.Fatalf("Merge() returned %T, want *FilterConfig", result)
			}
			if got := fc.GetSandboxHeaderName(); got != tt.wantSandboxHeader {
				t.Errorf("SandboxHeaderName = %q, want %q", got, tt.wantSandboxHeader)
			}
			if got := fc.GetHostHeaderName(); got != tt.wantHostHeader {
				t.Errorf("HostHeaderName = %q, want %q", got, tt.wantHostHeader)
			}
			if got := fc.GetSandboxPortHeader(); got != tt.wantPortHeader {
				t.Errorf("SandboxPortHeader = %q, want %q", got, tt.wantPortHeader)
			}
			if fc.DefaultPort != tt.wantDefaultPort {
				t.Errorf("DefaultPort = %q, want %q", fc.DefaultPort, tt.wantDefaultPort)
			}
			if fc.EnableAuth != tt.wantEnableAuth {
				t.Errorf("EnableAuth = %v, want %v", fc.EnableAuth, tt.wantEnableAuth)
			}
			if fc.Adapter == nil {
				t.Error("Merge() returned FilterConfig with nil Adapter")
			}
		})
	}
}

func TestConfigParserMergeJWT(t *testing.T) {
	manager := &fakeJWTAuthManager{}
	parser := NewConfigParser(manager)
	parent := newFilterConfig(&Config{EnableAuth: true}, manager)
	child := newFilterConfig(&Config{
		EnableJWTAuth:            true,
		TrafficAccessTokenHeader: "x-route-jwt",
	}, manager)

	merged := parser.Merge(parent, child).(*FilterConfig)
	if !merged.EnableAuth || !merged.EnableJWTAuth {
		t.Fatalf("merged auth modes = (%t, %t), want both true", merged.EnableAuth, merged.EnableJWTAuth)
	}
	if got := merged.GetTrafficAccessTokenHeader(); got != "x-route-jwt" {
		t.Errorf("merged JWT header = %q, want %q", got, "x-route-jwt")
	}
	if merged.jwtAuthManager != manager {
		t.Error("merged JWT manager was not preserved")
	}
}
