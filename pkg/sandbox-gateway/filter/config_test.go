package filter

import (
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

// helperTypedStructAny creates an *anypb.Any wrapping a v3.TypedStruct with given fields.
func helperTypedStructAny(t *testing.T, fields map[string]interface{}) *anypb.Any {
	t.Helper()
	ts := &v3.TypedStruct{}
	if fields != nil {
		s, err := structpb.NewStruct(fields)
		if err != nil {
			t.Fatalf("failed to create structpb.Struct: %v", err)
		}
		ts.Value = s
	}
	a, err := anypb.New(ts)
	if err != nil {
		t.Fatalf("failed to marshal TypedStruct to Any: %v", err)
	}
	return a
}

func TestConfigParserParse(t *testing.T) {
	parser := &ConfigParser{}

	tests := []struct {
		name              string
		any               *anypb.Any
		wantErr           bool
		wantSandboxHeader string
		wantHostHeader    string
		wantPortHeader    string
		wantDefaultPort   string
	}{
		{
			name:              "nil value in TypedStruct returns defaults",
			wantErr:           false,
			wantSandboxHeader: DefaultSandboxHeaderName,
			wantHostHeader:    DefaultHostHeaderName,
			wantPortHeader:    DefaultSandboxPortHeader,
			wantDefaultPort:   DefaultSandboxPort,
		},
		{
			name:              "empty struct returns defaults",
			wantErr:           false,
			wantSandboxHeader: DefaultSandboxHeaderName,
			wantHostHeader:    DefaultHostHeaderName,
			wantPortHeader:    DefaultSandboxPortHeader,
			wantDefaultPort:   DefaultSandboxPort,
		},
		{
			name:              "custom sandbox header",
			wantErr:           false,
			wantSandboxHeader: "x-custom-sandbox",
			wantHostHeader:    DefaultHostHeaderName,
			wantPortHeader:    DefaultSandboxPortHeader,
			wantDefaultPort:   DefaultSandboxPort,
		},
		{
			name:              "all custom values",
			wantErr:           false,
			wantSandboxHeader: "x-sandbox",
			wantHostHeader:    "X-Forwarded-Host",
			wantPortHeader:    "x-port",
			wantDefaultPort:   "8080",
		},
	}

	// Build the Any payloads based on test expectations
	tests[0].any = func() *anypb.Any {
		ts := &v3.TypedStruct{}
		a, _ := anypb.New(ts)
		return a
	}()
	tests[1].any = func() *anypb.Any {
		return helperTypedStructAny(t, map[string]interface{}{})
	}()
	tests[2].any = helperTypedStructAny(t, map[string]interface{}{
		"sandbox-header-name": "x-custom-sandbox",
	})
	tests[3].any = helperTypedStructAny(t, map[string]interface{}{
		"sandbox-header-name": "x-sandbox",
		"host-header-name":    "X-Forwarded-Host",
		"sandbox-port-header": "x-port",
		"default-port":        "8080",
	})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parser.Parse(tt.any, nil)
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
	}{
		{
			name:              "child overrides all parent fields",
			parent:            DefaultConfig(),
			child:             &Config{SandboxHeaderName: "child-sbx", HostHeaderName: "child-host", SandboxPortHeader: "child-port", DefaultPort: "9999"},
			wantSandboxHeader: "child-sbx",
			wantHostHeader:    "child-host",
			wantPortHeader:    "child-port",
			wantDefaultPort:   "9999",
		},
		{
			name:              "empty child preserves parent",
			parent:            &Config{SandboxHeaderName: "parent-sbx", HostHeaderName: "parent-host", SandboxPortHeader: "parent-port", DefaultPort: "1234"},
			child:             &Config{},
			wantSandboxHeader: "parent-sbx",
			wantHostHeader:    "parent-host",
			wantPortHeader:    "parent-port",
			wantDefaultPort:   "1234",
		},
		{
			name:              "partial child override",
			parent:            DefaultConfig(),
			child:             &Config{SandboxHeaderName: "override-sbx"},
			wantSandboxHeader: "override-sbx",
			wantHostHeader:    DefaultHostHeaderName,
			wantPortHeader:    DefaultSandboxPortHeader,
			wantDefaultPort:   DefaultSandboxPort,
		},
		{
			name:              "both defaults",
			parent:            DefaultConfig(),
			child:             DefaultConfig(),
			wantSandboxHeader: DefaultSandboxHeaderName,
			wantHostHeader:    DefaultHostHeaderName,
			wantPortHeader:    DefaultSandboxPortHeader,
			wantDefaultPort:   DefaultSandboxPort,
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
			if fc.Adapter == nil {
				t.Error("Merge() returned FilterConfig with nil Adapter")
			}
		})
	}
}
