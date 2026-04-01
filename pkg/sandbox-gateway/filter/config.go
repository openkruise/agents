package filter

import (
	"encoding/json"
	"fmt"
	"regexp"

	v3 "github.com/cncf/xds/go/xds/type/v3"
	"github.com/envoyproxy/envoy/contrib/golang/common/go/api"
	"google.golang.org/protobuf/types/known/anypb"
)

// hostPattern matches the host format: {port}-{namespace}--{name}.{domain}
// Group 1: port (digits), Group 2: namespace--name (alphanumeric and hyphens)
var hostPattern = regexp.MustCompile(`^(\d+)-([a-zA-Z0-9\-]+)\.`)

const (
	DefaultHostHeaderName    = "Host"
	DefaultSandboxHeaderName = "e2b-sandbox-id"
	DefaultSandboxPortHeader = "e2b-sandbox-port"
	DefaultSandboxPort       = "49983"
)

// Config holds the filter configuration
type Config struct {
	// SandboxHeaderName is the header name for sandbox ID (checked first)
	SandboxHeaderName string `json:"sandbox-header-name,omitempty"`
	// SandboxPortHeader is the header name for sandbox port
	SandboxPortHeader string `json:"sandbox-port-header,omitempty"`
	// HostHeaderName is the header name for host matching (fallback when sandbox header not found)
	HostHeaderName string `json:"host-header-name,omitempty"`
	// DefaultPort is the default port if not specified
	DefaultPort string `json:"default-port,omitempty"`
}

// DefaultConfig returns default configuration
func DefaultConfig() *Config {
	return &Config{
		SandboxHeaderName: DefaultSandboxHeaderName,
		SandboxPortHeader: DefaultSandboxPortHeader,
		HostHeaderName:    DefaultHostHeaderName,
		DefaultPort:       DefaultSandboxPort,
	}
}

// Validate checks configuration validity
func (c *Config) Validate() error {
	return nil
}

// GetSandboxHeaderName returns the effective sandbox header name
func (c *Config) GetSandboxHeaderName() string {
	if c.SandboxHeaderName != "" {
		return c.SandboxHeaderName
	}
	return DefaultSandboxHeaderName
}

// GetHostHeaderName returns the effective host header name
func (c *Config) GetHostHeaderName() string {
	if c.HostHeaderName != "" {
		return c.HostHeaderName
	}
	return DefaultHostHeaderName
}

// ExtractHostInfo extracts both host key and port from the header in one regex call
// Only for host mode: extracts both from the host format (<port>-<namespace>--<name>.domain)
// Returns (hostKey, port) - both empty if parsing fails
func (c *Config) ExtractHostInfo(headerValue string) (string, string) {
	if headerValue == "" {
		return "", ""
	}

	// Use regex to extract both port and namespace--name from host format
	// e.g., "8080-abc--def.example.com" -> hostKey: "abc--def", port: "8080"
	matches := hostPattern.FindStringSubmatch(headerValue)
	if len(matches) < 3 {
		return "", ""
	}
	return matches[2], matches[1]
}

type ConfigParser struct{}

func (p *ConfigParser) Parse(any *anypb.Any, callbacks api.ConfigCallbackHandler) (interface{}, error) {
	cfg := DefaultConfig()

	// Unmarshal the xds.type.v3.TypedStruct protobuf message
	typedStruct := &v3.TypedStruct{}
	if err := any.UnmarshalTo(typedStruct); err != nil {
		return nil, fmt.Errorf("failed to unmarshal TypedStruct: %w", err)
	}

	// Get the value from TypedStruct which contains the actual config as Struct
	valueStruct := typedStruct.GetValue()
	if valueStruct == nil {
		// No value field, use defaults
		return cfg, nil
	}

	// Convert the struct to JSON
	configBytes, err := json.Marshal(valueStruct.AsMap())
	if err != nil {
		return nil, fmt.Errorf("failed to marshal config value to JSON: %w", err)
	}

	// Parse actual config from JSON
	if len(configBytes) > 0 && string(configBytes) != "null" {
		if err := json.Unmarshal(configBytes, cfg); err != nil {
			return nil, fmt.Errorf("failed to parse config: %w", err)
		}
	}

	// Validate
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func (p *ConfigParser) Merge(parent interface{}, child interface{}) interface{} {
	parentCfg := parent.(*Config)
	childCfg := child.(*Config)

	// Child overrides parent for all fields
	merged := DefaultConfig()
	*merged = *parentCfg

	if childCfg.SandboxHeaderName != "" {
		merged.SandboxHeaderName = childCfg.SandboxHeaderName
	}
	if childCfg.SandboxPortHeader != "" {
		merged.SandboxPortHeader = childCfg.SandboxPortHeader
	}
	if childCfg.HostHeaderName != "" {
		merged.HostHeaderName = childCfg.HostHeaderName
	}
	if childCfg.DefaultPort != "" {
		merged.DefaultPort = childCfg.DefaultPort
	}

	return merged
}
