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
	"strings"
	"testing"

	v3 "github.com/cncf/xds/go/xds/type/v3"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/structpb"
)

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

func TestConfigParserParseUnmarshalError(t *testing.T) {
	parser := &ConfigParser{}

	// Provide a string value for an int field, causing json.Unmarshal to fail
	s, err := structpb.NewStruct(map[string]any{
		"wake-timeout-seconds": "not-a-number",
	})
	if err != nil {
		t.Fatalf("NewStruct() error = %v", err)
	}
	ts := &v3.TypedStruct{Value: s}
	input, _ := anypb.New(ts)

	_, err = parser.Parse(input, nil)
	if err == nil {
		t.Fatal("Parse() expected error for invalid field type, got nil")
	}
	if got := err.Error(); !strings.Contains(got, "failed to parse config") {
		t.Errorf("Parse() error = %q, want containing 'failed to parse config'", got)
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

func TestGetWakeTimeoutSeconds(t *testing.T) {
	tests := []struct {
		name string
		cfg  *Config
		want int
	}{
		{
			name: "default config returns 60",
			cfg:  DefaultConfig(),
			want: 60,
		},
		{
			name: "custom wake timeout",
			cfg:  &Config{WakeTimeoutSeconds: 300},
			want: 300,
		},
		{
			name: "zero returns default 60",
			cfg:  &Config{},
			want: 60,
		},
		{
			name: "negative returns default 60",
			cfg:  &Config{WakeTimeoutSeconds: -1},
			want: 60,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.GetWakeTimeoutSeconds()
			if got != tt.want {
				t.Errorf("GetWakeTimeoutSeconds() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestMergeWakeOnTraffic(t *testing.T) {
	parser := &ConfigParser{}
	parent := DefaultConfig()
	child := &Config{
		EnableWakeOnTraffic: true,
		WakeTimeoutSeconds:  120,
	}

	result := parser.Merge(NewFilterConfig(parent), NewFilterConfig(child))
	fc, ok := result.(*FilterConfig)
	if !ok {
		t.Fatalf("Merge() returned %T, want *FilterConfig", result)
	}
	if !fc.EnableWakeOnTraffic {
		t.Error("Merge() did not propagate EnableWakeOnTraffic from child")
	}
	if fc.WakeTimeoutSeconds != 120 {
		t.Errorf("WakeTimeoutSeconds = %d, want 120", fc.WakeTimeoutSeconds)
	}
}
