package filter

import (
	"encoding/json"
	"fmt"
	"strconv"

	v3 "github.com/cncf/xds/go/xds/type/v3"
	"github.com/envoyproxy/envoy/contrib/golang/common/go/api"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/openkruise/agents/pkg/servers/e2b/adapters"
)

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

// GetSandboxPortHeader returns the effective sandbox port header name
func (c *Config) GetSandboxPortHeader() string {
	if c.SandboxPortHeader != "" {
		return c.SandboxPortHeader
	}
	return DefaultSandboxPortHeader
}

// GetDefaultPort returns the default port as an integer
func (c *Config) GetDefaultPort() int {
	if c.DefaultPort != "" {
		if p, err := strconv.Atoi(c.DefaultPort); err == nil {
			return p
		}
	}
	p, _ := strconv.Atoi(DefaultSandboxPort)
	return p
}

// FilterConfig wraps Config and holds the adapter created from the config
type FilterConfig struct {
	*Config
	Adapter *adapters.E2BAdapter
}

// NewFilterConfig creates a FilterConfig with an adapter built from the config values
func NewFilterConfig(cfg *Config) *FilterConfig {
	adapter := adapters.NewE2BAdapterWithOptions(
		0, // port not used by gateway
		adapters.E2BAdapterOptions{
			SandboxIDHeader:   cfg.GetSandboxHeaderName(),
			SandboxPortHeader: cfg.GetSandboxPortHeader(),
			HostHeader:        cfg.GetHostHeaderName(),
			DefaultPort:       cfg.GetDefaultPort(),
		},
	)
	return &FilterConfig{
		Config:  cfg,
		Adapter: adapter,
	}
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
		return NewFilterConfig(cfg), nil
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

	return NewFilterConfig(cfg), nil
}

func (p *ConfigParser) Merge(parent interface{}, child interface{}) interface{} {
	parentCfg := parent.(*FilterConfig)
	childCfg := child.(*FilterConfig)

	// Child overrides parent for all fields
	merged := DefaultConfig()
	*merged = *parentCfg.Config

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

	return NewFilterConfig(merged)
}
