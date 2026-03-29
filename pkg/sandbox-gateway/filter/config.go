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

// HeaderMatchPolicy defines how to extract the host key from the request
type HeaderMatchPolicy string

const (
	// HeaderMatchPolicySandbox extracts sandbox ID directly from the specified header
	HeaderMatchPolicySandbox HeaderMatchPolicy = "sandbox"
	// HeaderMatchPolicyHost extracts the first label from the domain in the specified header
	HeaderMatchPolicyHost HeaderMatchPolicy = "host"
)

// Config holds the filter configuration
type Config struct {
	// HeaderMatchPolicy specifies how to extract the host key: "sandbox" or "host"
	HeaderMatchPolicy HeaderMatchPolicy `json:"header-match-policy,omitempty"`
	// HeaderMatchName is the name of the header to extract the host key from
	HeaderMatchName string `json:"header-match-name,omitempty"`
	// HeaderSandboxPort is the header name for sandbox port
	HeaderSandboxPort string `json:"header-sandbox-port,omitempty"`
	// DefaultPort is the default port if not specified
	DefaultPort string `json:"default-port,omitempty"`
}

// DefaultConfig returns default configuration
func DefaultConfig() *Config {
	return &Config{
		HeaderMatchPolicy: HeaderMatchPolicySandbox,
		HeaderMatchName:   "", // Empty by default - for host policy, will use Host() method
		HeaderSandboxPort: "e2b-sandbox-port",
		DefaultPort:       "80",
	}
}

// Validate checks configuration validity
func (c *Config) Validate() error {
	if c.HeaderMatchPolicy != HeaderMatchPolicySandbox && c.HeaderMatchPolicy != HeaderMatchPolicyHost {
		return fmt.Errorf("invalid header-match-policy: %s, must be 'sandbox' or 'host'", c.HeaderMatchPolicy)
	}
	return nil
}

// GetHeaderMatchName returns the effective header name to use
// For sandbox policy with empty config, returns "e2b-sandbox-id" as default
// For host policy with empty config, returns "" (will use Host() method)
func (c *Config) GetHeaderMatchName() string {
	if c.HeaderMatchName != "" {
		return c.HeaderMatchName
	}
	// Default values when not explicitly set
	if c.HeaderMatchPolicy == HeaderMatchPolicySandbox {
		return "e2b-sandbox-id"
	}
	// For host policy, empty means use Host() method
	return ""
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

	if childCfg.HeaderMatchPolicy != "" {
		merged.HeaderMatchPolicy = childCfg.HeaderMatchPolicy
	}
	if childCfg.HeaderMatchName != "" {
		merged.HeaderMatchName = childCfg.HeaderMatchName
	}
	if childCfg.HeaderSandboxPort != "" {
		merged.HeaderSandboxPort = childCfg.HeaderSandboxPort
	}
	if childCfg.DefaultPort != "" {
		merged.DefaultPort = childCfg.DefaultPort
	}

	return merged
}
